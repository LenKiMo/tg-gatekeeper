// Package app 负责组装依赖、启动顺序与优雅退出。
package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/audit"
	"github.com/LenKiMo/tg-gatekeeper/internal/challenge"
	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/dispatch"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/gatekeeper"
	"github.com/LenKiMo/tg-gatekeeper/internal/image"
	"github.com/LenKiMo/tg-gatekeeper/internal/provider"
	"github.com/LenKiMo/tg-gatekeeper/internal/router"
	"github.com/LenKiMo/tg-gatekeeper/internal/store"
	"github.com/LenKiMo/tg-gatekeeper/internal/telegram"
)

// App 持有全部运行时依赖。
type App struct {
	cfg        *config.Config
	log        *slog.Logger
	stores     *store.Set
	dispatcher *dispatch.Dispatcher
	auditor    *audit.Logger
	client     *telegram.Client
	images     *image.Resolver
	service    *gatekeeper.Service
	router     *router.Router
}

// New 组装应用（不启动后台循环）。
func New(ctx context.Context, cfg *config.Config) (*App, error) {
	log := newLogger(cfg)
	for _, w := range cfg.WarnUnsafe() {
		log.Warn("配置告警", "detail", w)
	}

	stores, err := store.New(ctx, cfg)
	if err != nil {
		return nil, err
	}
	for _, n := range stores.Notes {
		log.Warn("存储说明", "detail", n)
	}

	dispatcher := dispatch.New(cfg.Dispatcher, dispatchReporter{log: log})
	auditor, err := audit.New(cfg.Audit)
	if err != nil {
		_ = stores.Close()
		return nil, err
	}
	client, err := telegram.NewClient(cfg)
	if err != nil {
		_ = auditor.Close()
		_ = stores.Close()
		return nil, err
	}
	images, err := image.New(cfg.Images)
	if err != nil {
		_ = client.Close()
		_ = auditor.Close()
		_ = stores.Close()
		return nil, err
	}
	src, err := provider.New(cfg)
	if err != nil {
		_ = images.Close()
		_ = client.Close()
		_ = auditor.Close()
		_ = stores.Close()
		return nil, err
	}

	svc, err := gatekeeper.New(gatekeeper.Deps{
		Config:   cfg,
		API:      client,
		Groups:   stores.Groups,
		Registry: stores.Registry,
		Deletion: stores.Deletion,
		Dispatch: dispatcher,
		Audit:    auditor,
		Images:   images,
		Provider: src,
		Logger:   log,
	})
	if err != nil {
		_ = images.Close()
		_ = client.Close()
		_ = auditor.Close()
		_ = stores.Close()
		return nil, err
	}

	r := router.NewRouter(client, svc, stores.Groups, dispatcher, cfg, log)
	return &App{
		cfg:        cfg,
		log:        log,
		stores:     stores,
		dispatcher: dispatcher,
		auditor:    auditor,
		client:     client,
		images:     images,
		service:    svc,
		router:     r,
	}, nil
}

// Run 启动并阻塞，直到收到 SIGINT/SIGTERM。
func (a *App) Run(ctx context.Context) error {
	botID, botName := a.client.Self()
	a.log.Info("启动 tg-gatekeeper",
		"bot_id", botID, "bot_username", botName,
		"provider", a.cfg.Provider.Active,
		"group_store", a.cfg.Storage.GroupStore,
		"session_registry", a.cfg.Storage.SessionRegistry,
	)

	if err := a.service.Start(ctx); err != nil {
		return err
	}
	if g := a.service.Generator(); g != nil {
		a.log.Info("题库就绪", "revision", g.Revision(), "labels", g.Buckets(), "pools", g.PoolSizes())
	}
	a.router.Register()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 客户端库的 Run() 没有停止接口：退出靠取消 ctx + 进程返回。
	go func() {
		select {
		case sig := <-sigCh:
			a.log.Info("收到退出信号，开始优雅停止", "signal", sig.String())
		case <-runCtx.Done():
		}
		a.shutdown()
	}()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				a.log.Error("更新循环 panic", "panic", fmt.Sprint(r))
			}
		}()
		a.router.Run()
	}()

	<-runCtx.Done()
	return nil
}

// shutdown 执行有序退出：先停止接收新任务，再排空在途任务，最后关资源。
func (a *App) shutdown() {
	timeout := time.Duration(a.cfg.Runtime.ShutdownTimeoutSeconds) * time.Second
	grace := time.Duration(a.cfg.Runtime.ShutdownGraceSeconds) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout+grace)
	defer cancel()

	a.log.Info("停止接收新更新")
	a.dispatcher.StopAdmission()

	a.log.Info("等待会话/副作用/删除 worker 结束")
	a.service.Stop()

	a.log.Info("排空分派队列")
	if err := a.dispatcher.Drain(ctx); err != nil {
		a.log.Warn("分派队列排空未完成", "err", err)
	}

	if err := a.stores.Close(); err != nil {
		a.log.Warn("关闭存储出错", "err", err)
	}
	if err := a.images.Close(); err != nil {
		a.log.Warn("关闭图片管线出错", "err", err)
	}
	if err := a.client.Close(); err != nil {
		a.log.Warn("关闭 Telegram 客户端出错", "err", err)
	}
	if err := a.auditor.Close(); err != nil {
		a.log.Warn("关闭审计日志出错", "err", err)
	}
	a.log.Info("已停止")
}

// CheckConfig 做启动前自检：配置、存储、题库、图片是否都能用。
func CheckConfig(ctx context.Context, cfg *config.Config) error {
	checks := make([]string, 0, 8)
	defer func() {
		for _, c := range checks {
			fmt.Println("  ✓", c)
		}
	}()

	stores, err := store.New(ctx, cfg)
	if err != nil {
		return fmt.Errorf("存储检查失败: %w", err)
	}
	defer func() { _ = stores.Close() }()
	checks = append(checks, fmt.Sprintf("存储可用（group=%s registry=%s deletion=%s）",
		cfg.Storage.GroupStore, cfg.Storage.SessionRegistry, cfg.Storage.DeletionQueue))

	groupCfg, err := stores.Groups.EnsureGroup(ctx, -1, defaultGroupForCheck(cfg))
	if err != nil {
		return fmt.Errorf("群配置读写检查失败: %w", err)
	}
	checks = append(checks, fmt.Sprintf("群配置读写可用（revision=%d）", groupCfg.Revision))

	src, err := provider.New(cfg)
	if err != nil {
		return fmt.Errorf("数据源构造失败: %w", err)
	}
	defer func() { _ = src.Close() }()
	snap, err := src.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("加载数据集失败: %w", err)
	}
	gen := challengeBuilder(snap)
	if err := gen.HealthCheck(cfg.Gatekeeper.OptionCount); err != nil {
		return fmt.Errorf("数据集健康检查失败: %w", err)
	}
	checks = append(checks, fmt.Sprintf("数据集可用（source=%s entries=%d buckets=%d pools=%v dropped=%d）",
		snap.SourceID, len(snap.Entries), gen.Buckets(), gen.PoolSizes(), snap.Dropped))

	ch, err := gen.Next(challengeOptions(cfg))
	if err != nil {
		return fmt.Errorf("抽题失败: %w", err)
	}
	if len(ch.Options) != cfg.Gatekeeper.OptionCount {
		fmt.Printf("  ! 实际选项数 %d，配置为 %d（可能是 shortage_policy=reduce 或归一化后重复）\n",
			len(ch.Options), cfg.Gatekeeper.OptionCount)
	}
	correctOpt, ok := ch.CorrectOption()
	if !ok || !ch.IsCorrect(correctOpt.Token) {
		return fmt.Errorf("抽题自检异常：正确答案不在选项内")
	}
	if !ch.ValidToken(ch.Options[0].Token) {
		return fmt.Errorf("抽题自检异常：选项 token 非法")
	}
	checks = append(checks, fmt.Sprintf("抽题自检通过（选项 %d 个，题库版本 %s）", len(ch.Options), ch.Revision))

	img, err := image.New(cfg.Images)
	if err != nil {
		return fmt.Errorf("图片管线构造失败: %w", err)
	}
	defer func() { _ = img.Close() }()
	resolved, err := img.Resolve(ctx, ch.Entry.ImageRef)
	if err != nil {
		return fmt.Errorf("样例图片解析失败（%s）: %w", ch.Entry.ImageRef, err)
	}
	if strings.Contains(resolved.Filename, ch.CorrectLabel) || strings.Contains(resolved.Filename, ch.Entry.ID) {
		return fmt.Errorf("图片文件名泄漏了答案（%s）", resolved.Filename)
	}
	checks = append(checks, fmt.Sprintf("图片管线可用（%s，%d 字节，重编码为 %s）",
		ch.Entry.ImageRef, len(resolved.Bytes), resolved.Filename))
	return nil
}

// defaultGroupForCheck 构造自检用的群配置默认值。
func defaultGroupForCheck(cfg *config.Config) domain.GroupConfig {
	mode := domain.VerifyMode(cfg.GroupDefaults.Mode)
	if !mode.Valid() {
		mode = domain.VerifyModeJoin
	}
	return domain.GroupConfig{
		Enabled:        cfg.GroupDefaults.Enabled,
		Mode:           mode,
		Welcome:        cfg.GroupDefaults.Welcome,
		RulesMessageID: cfg.GroupDefaults.RulesMessageID,
		AdWords:        cfg.GroupDefaults.AdWords,
	}
}

func challengeBuilder(snap domain.Snapshot) *challenge.Generator { return challenge.New(snap) }

func challengeOptions(cfg *config.Config) challenge.Options {
	return challenge.Options{
		OptionCount:    cfg.Gatekeeper.OptionCount,
		MinOptionCount: cfg.Gatekeeper.MinOptionCount,
		ShortagePolicy: cfg.Gatekeeper.ShortagePolicy,
		Strategy:       cfg.Gatekeeper.Difficulty.Strategy,
		DifficultyBand: cfg.Gatekeeper.Difficulty.Band(),
	}
}

// dispatchReporter 把分派器的告警转成结构化日志。
type dispatchReporter struct{ log *slog.Logger }

func (d dispatchReporter) Warn(msg string, fields map[string]any) {
	args := make([]any, 0, len(fields)*2)
	for k, v := range fields {
		args = append(args, k, v)
	}
	d.log.Warn(msg, args...)
}

func newLogger(cfg *config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.Runtime.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	return slog.New(handler)
}
