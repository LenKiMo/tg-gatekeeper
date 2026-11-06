package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
)

// Set 是一组装好的持久化实现。
type Set struct {
	Groups   ports.Store
	Registry ports.SessionRegistry
	Deletion ports.DeletionQueue
	// Closers 按逆序关闭。
	Closers []func() error
	// SQL 为 nil 表示用的是内存实现。
	SQL *SQL
	// Notes 是启动时需要记录/告警的说明（例如"YAML 模式只支持单实例"）。
	Notes []string
}

// New 按配置组装三种存储。
//
// 组合规则：
//   - group_store: sqlite|yaml
//   - session_registry / deletion_queue: sqlite|memory
//     YAML 群配置模式下，会话与队列仍然落在本地 SQLite（进程重启可恢复）。
func New(ctx context.Context, cfg *config.Config) (*Set, error) {
	set := &Set{}
	groupBackend := strings.ToLower(cfg.Storage.GroupStore)

	var sqlHandle *SQL
	needSQL := false
	for _, b := range []string{groupBackend, strings.ToLower(cfg.Storage.SessionRegistry), strings.ToLower(cfg.Storage.DeletionQueue)} {
		if b == "sqlite" {
			needSQL = true
		}
	}
	if needSQL {
		// 所有 SQL 状态统一落在一个 SQLite 文件里，避免一个部署出现两套状态库。
		h, err := Open(ctx, cfg, "sqlite")
		if err != nil {
			return nil, err
		}
		sqlHandle = h
		set.SQL = h
		set.Closers = append(set.Closers, h.Close)
	}

	switch groupBackend {
	case "sqlite":
		set.Groups = NewGroupStore(sqlHandle)
	case "yaml":
		ys, err := NewYAMLStore(cfg)
		if err != nil {
			return nil, err
		}
		set.Groups = ys
		set.Notes = append(set.Notes, "storage.group_store=yaml：只支持单实例运行，多副本会丢更新")
	default:
		return nil, fmt.Errorf("不支持的 group_store %q", cfg.Storage.GroupStore)
	}

	switch strings.ToLower(cfg.Storage.SessionRegistry) {
	case "sqlite":
		set.Registry = NewRegistry(sqlHandle)
	case "memory":
		set.Registry = NewMemRegistry()
		set.Notes = append(set.Notes, "storage.session_registry=memory：进程重启会丢失待验证会话（仅限测试）")
	default:
		return nil, fmt.Errorf("不支持的 session_registry %q", cfg.Storage.SessionRegistry)
	}

	switch strings.ToLower(cfg.Storage.DeletionQueue) {
	case "sqlite":
		set.Deletion = NewDeletionQueue(sqlHandle)
	case "memory":
		set.Deletion = NewMemDeletionQueue()
	default:
		return nil, fmt.Errorf("不支持的 deletion_queue %q", cfg.Storage.DeletionQueue)
	}

	return set, nil
}

// Close 逆序关闭全部资源，聚合错误。
func (s *Set) Close() error {
	var errs []string
	for i := len(s.Closers) - 1; i >= 0; i-- {
		if err := s.Closers[i](); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("关闭存储失败: %s", strings.Join(errs, "; "))
	}
	return nil
}
