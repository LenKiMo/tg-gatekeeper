// Package gatekeeper 是验证流程的用例层：把 Telegram 事件翻译成状态机事件，
// 并把数据集、群配置、副作用与审计串起来。
//
// 这里只做编排：并发正确性交给注册表的 Claim，Telegram 细节交给 ports.Telegram，
// 抽题交给 challenge.Generator。因此本包可以在没有网络、没有数据库的情况下测试。
package gatekeeper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/challenge"
	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/filter"
	"github.com/LenKiMo/tg-gatekeeper/internal/image"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
	"github.com/LenKiMo/tg-gatekeeper/internal/telegram"
)

// Service 是验证流程的入口。
type Service struct {
	cfg      *config.Config
	api      ports.Telegram
	groups   ports.Store
	registry ports.SessionRegistry
	deletion ports.DeletionQueue
	dispatch ports.Dispatcher
	auditLog ports.Audit
	images   ports.ImageResolver
	adFilter *filter.AdFilter
	provider ports.Source
	log      *slog.Logger

	gen atomic.Pointer[challenge.Generator]

	refreshOnce sync.Once
	stopCh      chan struct{}
	wg          sync.WaitGroup
	startedAt   time.Time
}

// Deps 是构造 Service 需要的依赖。
type Deps struct {
	Config   *config.Config
	API      ports.Telegram
	Groups   ports.Store
	Registry ports.SessionRegistry
	Deletion ports.DeletionQueue
	Dispatch ports.Dispatcher
	Audit    ports.Audit
	Images   ports.ImageResolver
	Provider ports.Source
	Logger   *slog.Logger
}

// New 构造 Service。
func New(d Deps) (*Service, error) {
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	images := d.Images
	if images == nil {
		r, err := image.New(d.Config.Images)
		if err != nil {
			return nil, err
		}
		images = r
	}
	s := &Service{
		cfg:       d.Config,
		api:       d.API,
		groups:    d.Groups,
		registry:  d.Registry,
		deletion:  d.Deletion,
		dispatch:  d.Dispatch,
		auditLog:  d.Audit,
		images:    images,
		adFilter:  filter.New(d.Config.AdFilter),
		provider:  d.Provider,
		log:       log,
		stopCh:    make(chan struct{}),
		startedAt: time.Now(),
	}
	return s, nil
}

// Start 加载数据集快照并启动后台循环（数据集热更新、超时扫描、副作用 outbox）。
func (s *Service) Start(ctx context.Context) error {
	if err := s.RefreshDataset(ctx); err != nil {
		return err
	}
	s.wg.Add(3)
	go s.refreshLoop()
	go s.effectLoop()   // 超时扫描 + 副作用 outbox
	go s.deletionLoop() // 延时删除
	return nil
}

// Stop 停止后台循环（幂等）。
func (s *Service) Stop() {
	s.refreshOnce.Do(func() { close(s.stopCh) })
	s.wg.Wait()
}

// RefreshDataset 重新拉取数据集快照并原子替换。失败时保留旧快照。
func (s *Service) RefreshDataset(ctx context.Context) error {
	snap, err := s.provider.Snapshot(ctx)
	if err != nil {
		if cur := s.gen.Load(); cur != nil {
			s.log.Warn("数据集刷新失败，继续使用旧快照", "err", err, "revision", cur.Revision())
			return nil
		}
		return fmt.Errorf("加载数据集失败: %w", err)
	}
	gen := challenge.New(snap)
	if err := gen.HealthCheck(s.cfg.Gatekeeper.OptionCount); err != nil {
		if strings.EqualFold(s.cfg.Gatekeeper.ShortagePolicy, "deny") {
			if cur := s.gen.Load(); cur != nil {
				s.log.Warn("数据集不满足 option_count，继续使用旧快照", "err", err)
				return nil
			}
			return fmt.Errorf("数据集健康检查失败: %w", err)
		}
		s.log.Warn("数据集不足，按 shortage_policy=reduce 允许降级", "err", err)
	}
	s.gen.Store(gen)
	s.log.Info("数据集已加载",
		"source", snap.SourceID,
		"revision", gen.Revision(),
		"buckets", gen.Buckets(),
		"pools", gen.PoolSizes(),
		"dropped", snap.Dropped)
	return nil
}

// Generator 返回当前出题器（可能为 nil，调用方必须判空）。
func (s *Service) Generator() *challenge.Generator { return s.gen.Load() }

// DatasetRevision 返回当前题库版本。
func (s *Service) DatasetRevision() string {
	if g := s.gen.Load(); g != nil {
		return g.Revision()
	}
	return ""
}

func (s *Service) refreshLoop() {
	defer s.wg.Done()
	interval := time.Duration(s.cfg.Provider.RefreshIntervalSeconds) * time.Second
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			_ = s.RefreshDataset(ctx)
			cancel()
		}
	}
}

// ---------------------------------------------------------------- 群配置

// Group 读取群配置；不存在时用默认值创建。
func (s *Service) Group(ctx context.Context, chatID int64) (domain.GroupConfig, error) {
	defaults := domain.GroupConfig{
		Enabled:        s.cfg.GroupDefaults.Enabled,
		Mode:           domain.VerifyMode(s.cfg.GroupDefaults.Mode),
		Welcome:        s.cfg.GroupDefaults.Welcome,
		RulesMessageID: s.cfg.GroupDefaults.RulesMessageID,
		RulesLink:      s.cfg.GroupDefaults.RulesLink,
		AdWords:        s.cfg.GroupDefaults.AdWords,
	}
	if !defaults.Mode.Valid() {
		defaults.Mode = domain.VerifyModeJoin
	}
	return s.groups.EnsureGroup(ctx, chatID, defaults)
}

// ErrConfigConflict 表示群配置在读取之后被别人改过，本次写入没有生效。
var ErrConfigConflict = errors.New("配置刚被其他管理员改过，本次没有保存，请重试")

// SaveGroup 以调用方读到的 revision 做 CAS 保存群配置。
//
// 以前的实现先重读一次再拿"最新 revision"去写，等于把乐观锁关掉了：两个管理员同时
// 用命令改不同字段时，后写的人会连别人刚改的字段一起覆盖。现在用调用方手里那份
// revision，后到的人会拿到 ErrConfigConflict。
func (s *Service) SaveGroup(ctx context.Context, cfg domain.GroupConfig) error {
	if _, err := s.groups.SaveGroup(ctx, cfg, cfg.Revision); err != nil {
		if errors.Is(err, ports.ErrConflict) {
			return ErrConfigConflict
		}
		return err
	}
	return nil
}

// ---------------------------------------------------------------- 副作用计划

// effectPlan 是 EffectPlanner 的实现：根据终态决定要执行的 Telegram 动作。
//
// 所有参数都在这里定死（消息 ID、文案、延迟），因此副作用执行器不需要再读库、
// 也不可能因为"执行时状态又变了"而做出错误动作。
func (s *Service) effectPlan(group domain.GroupConfig, caption string) ports.EffectPlanner {
	cfg := s.cfg
	return func(before, after domain.Session, ev domain.Event) []domain.Effect {
		var out []domain.Effect
		add := func(kind domain.EffectKind, payload domain.EffectPayload, due time.Duration) {
			out = append(out, domain.Effect{Kind: kind, Payload: payload, DueAt: time.Now().Add(due)})
		}
		chatUser := domain.EffectPayload{
			"chat_id": itoa(before.Key.ChatID),
			"user_id": itoa(before.Key.UserID),
		}
		deleteChallenge := func() {
			if before.ChallengeMessage.Valid() {
				add(domain.EffectDeleteMessage, domain.EffectPayload{
					"chat_id":    itoa(before.ChallengeMessage.ChatID),
					"message_id": itoa(before.ChallengeMessage.MessageID),
					"delay":      "0",
				}, 0)
			}
		}
		deleteJoin := func() {
			if before.JoinMessage.Valid() && cfg.Gatekeeper.DeleteJoinMessage {
				add(domain.EffectDeleteMessage, domain.EffectPayload{
					"chat_id":    itoa(before.JoinMessage.ChatID),
					"message_id": itoa(before.JoinMessage.MessageID),
					"delay":      "0",
				}, 0)
			}
		}
		deleteAdminCard := func() {
			if before.AdminMessage.Valid() {
				add(domain.EffectDeleteMessage, domain.EffectPayload{
					"chat_id":    itoa(before.AdminMessage.ChatID),
					"message_id": itoa(before.AdminMessage.MessageID),
					"delay":      "0",
				}, 0)
			}
		}
		kick := func() {
			// 踢出 = 一条定时封禁：kick_unban_after_seconds 秒后由 Telegram 自动解除，
			// 效果是"请出去，稍后可以再来"。不再另排解封任务：封禁和解封是两条各自重试的
			// 副作用，封禁被重试拖过解封的到点时间时，解封先空跑、之后封禁成功就没人解了。
			// 到期时间在执行时才换算（见 applyTempBan）。配成 0 = 不解封（永久），仅限显式配置。
			add(domain.EffectBan, withText(chatUser, map[string]string{
				"seconds": itoa(int64(cfg.KickUnbanAfter().Seconds())),
				"reason":  string(banReasonKick),
			}), 0)
		}
		welcome := func() {
			// 有欢迎语或有群规才发欢迎消息。群规两种来源都算：/reg <链接> 设的显式链接、
			// /reg（回复消息）设的消息 ID——以前只认后者。
			if group.Welcome == "" && groupRulesLink(group, before.Key.ChatID) == "" {
				return
			}
			// 昵称取会话里记录的入群昵称：此刻事件里的 actor 是"点击按钮的人"或"管理员"，
			// 都不能用来当新成员的名字（否则 {mention} 会退化成"新成员"）。
			text := telegram.RenderCaption(group.Welcome, before.Key.UserID, before.DisplayName,
				cfg.Gatekeeper.TimeoutSeconds, groupRulesLink(group, before.Key.ChatID))
			// 模板已经自己给了链接（{rules} 占位或手写 t.me 链接）就不再追加，避免两条链接。
			if link := groupRulesLink(group, before.Key.ChatID); link != "" &&
				!strings.Contains(group.Welcome, "{rules}") && !strings.Contains(group.Welcome, "t.me/") {
				text = strings.TrimSpace(text + "\n" + telegram.EscapeMarkdownV2("群规：") +
					"[" + telegram.EscapeMarkdownV2("点击阅读") + "](" + link + ")")
			}
			add(domain.EffectWelcome, domain.EffectPayload{
				"chat_id":      itoa(before.Key.ChatID),
				"user_id":      itoa(before.Key.UserID),
				"text":         text,
				"delete_after": itoa(int64(cfg.Commands.WelcomeDeleteAfterSeconds)),
			}, 0)
		}
		// setTag 给通过验证的成员打标签。它不依赖欢迎语：以前写在 welcome() 里、排在
		// 提前返回之后，只设了 /tag 而没设欢迎语和群规的群永远打不上标签。
		setTag := func() {
			if group.Tag != "" {
				add(domain.EffectSetTag, map[string]string{
					"chat_id": itoa(before.Key.ChatID),
					"user_id": itoa(before.Key.UserID),
					"tag":     group.Tag,
				}, 0)
			}
		}

		switch after.State {
		case domain.StatePassed, domain.StateAdminPass:
			if before.Mode == domain.SessionRequest {
				add(domain.EffectApprove, chatUser, 0)
			} else {
				add(domain.EffectRestorePermissions, chatUser, 0)
			}
			welcome()
			setTag()
			deleteChallenge()
			deleteJoin()
			deleteAdminCard()
		case domain.StateWrong:
			// 答错即处置。申请模式下人还没进群，处置 = 拒绝申请（与超时一致），不封禁——
			// 封一个不在群里的人只会往黑名单里添东西；顺带清掉群里的管理员卡片，否则它
			// 会留在群里、按钮点了也没反应。入群模式下处置 = 踢出，允许重新入群尝试。
			if before.Mode == domain.SessionRequest {
				add(domain.EffectDecline, chatUser, 0)
			} else {
				kick()
			}
			deleteChallenge()
			deleteJoin()
			deleteAdminCard()
		case domain.StateTimedOut:
			if before.Mode == domain.SessionRequest {
				add(domain.EffectDecline, chatUser, 0)
			} else {
				kick()
			}
			deleteChallenge()
			deleteJoin()
			deleteAdminCard()
		case domain.StateSpoke:
			kick()
			if ev.Message.Valid() {
				add(domain.EffectDeleteMessage, domain.EffectPayload{
					"chat_id":    itoa(ev.Message.ChatID),
					"message_id": itoa(ev.Message.MessageID),
					"delay":      "0",
				}, 0)
			}
			deleteChallenge()
			deleteJoin()
		case domain.StateAdminBan:
			if before.Mode == domain.SessionRequest {
				add(domain.EffectDecline, chatUser, 0)
			}
			// 管理员手动封禁：admin_ban_seconds（默认 7 天）后由 Telegram 自动解除；
			// 配成 0 才是永久。申请模式下同样是"拒绝申请 + 这段时间内不能再申请"。
			add(domain.EffectBan, withText(chatUser, map[string]string{
				"seconds": itoa(int64(cfg.Gatekeeper.AdminBanSeconds)),
				"reason":  string(banReasonAdmin),
			}), 0)
			deleteChallenge()
			deleteJoin()
			deleteAdminCard()
		case domain.StateLeft, domain.StateCancelled:
			deleteChallenge()
		}
		return out
	}
}

// ---------------------------------------------------------------- 小工具

func itoa(v int64) string { return fmt.Sprintf("%d", v) }

func withText(base domain.EffectPayload, extra map[string]string) domain.EffectPayload {
	out := domain.EffectPayload{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// groupRulesLink 解析群规链接：运营用 /reg <url> 设的显式链接优先，其次由群规消息 ID 推导。
func groupRulesLink(group domain.GroupConfig, chatID int64) string {
	if group.RulesLink != "" {
		return group.RulesLink
	}
	if group.RulesMessageID > 0 {
		return rulesLink(chatID, group.RulesMessageID)
	}
	return ""
}

// rulesLink 生成群规消息的 MarkdownV2 链接。
//
// 用 t.me/c/<内部ID>/<消息ID>：公群也能拼 t.me/<username>/<id>，但机器人要额外 getChat
// 才拿得到 username；运营想让链接更好看，直接在欢迎语模板里写完整 URL 即可（模板里
// 出现 t.me/ 时不再追加这条）。
func rulesLink(chatID, messageID int64) string {
	return fmt.Sprintf("https://t.me/c/%s/%d", chatPathID(chatID), messageID)
}

// chatPathID 把 -100xxxxxxxxxx 转成 t.me/c 链接里用的短 id。
func chatPathID(chatID int64) string {
	s := fmt.Sprintf("%d", chatID)
	if strings.HasPrefix(s, "-100") {
		return s[4:]
	}
	return strings.TrimPrefix(s, "-")
}

// Member 是一个入群成员。
type Member struct {
	UserID      int64
	DisplayName string
	IsBot       bool
	IsPremium   bool
	InvitedBy   int64
}

// ParseInt 读取 payload 里的整数字段（副作用执行器用）。
func ParseInt(v string) int64 {
	var out int64
	neg := false
	for i, r := range v {
		if i == 0 && r == '-' {
			neg = true
			continue
		}
		if r < '0' || r > '9' {
			return out
		}
		out = out*10 + int64(r-'0')
	}
	if neg {
		return -out
	}
	return out
}
