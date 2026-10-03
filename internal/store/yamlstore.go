package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
)

// yamlFile 是 YAML 群配置文件的磁盘结构。
type yamlFile struct {
	Groups []yamlGroup `yaml:"groups"`
}

type yamlGroup struct {
	ChatID         int64    `yaml:"chat_id"`
	Enabled        bool     `yaml:"enabled"`
	Mode           string   `yaml:"mode"`
	RulesMessageID int64    `yaml:"rules_message_id"`
	RulesLink      string   `yaml:"rules_link"`
	Welcome        string   `yaml:"welcome"`
	Tag            string   `yaml:"tag"`
	AdWords        []string `yaml:"ad_words"`
	Revision       uint64   `yaml:"revision"`
	UpdatedAt      int64    `yaml:"updated_at"`
}

// YAMLStore 是可选的无数据库群配置存储。
//
// 用进程内锁 + 临时文件 + fsync + 同目录原子 rename 保证不写坏文件；只能单实例运行
// （两个副本会丢更新，需要 HA 请用 MySQL）。
type YAMLStore struct {
	path     string
	writes   bool
	fileMode os.FileMode

	mu   sync.Mutex
	data map[int64]domain.GroupConfig
}

// NewYAMLStore 加载 YAML 群配置；文件不存在时视为空配置（首次 EnsureGroup 会写盘）。
func NewYAMLStore(cfg *config.Config) (*YAMLStore, error) {
	mode := os.FileMode(0o600)
	if cfg.Storage.YAML.FileMode != "" {
		if v, err := strconv.ParseUint(cfg.Storage.YAML.FileMode, 8, 32); err == nil {
			mode = os.FileMode(v)
		}
	}
	s := &YAMLStore{
		path:     cfg.Storage.YAML.Path,
		writes:   cfg.Storage.YAML.RuntimeWrites,
		fileMode: mode,
		data:     map[int64]domain.GroupConfig{},
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("读取 YAML 群配置 %s 失败: %w", s.path, err)
	}
	var f yamlFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("解析 YAML 群配置 %s 失败: %w", s.path, err)
	}
	for _, g := range f.Groups {
		mode := domain.VerifyMode(g.Mode)
		if !mode.Valid() {
			mode = domain.VerifyModeJoin
		}
		s.data[g.ChatID] = domain.GroupConfig{
			ChatID:         g.ChatID,
			Enabled:        g.Enabled,
			Mode:           mode,
			RulesMessageID: g.RulesMessageID,
			RulesLink:      g.RulesLink,
			Welcome:        g.Welcome,
			Tag:            g.Tag,
			AdWords:        g.AdWords,
			Revision:       g.Revision,
			UpdatedAt:      time.Unix(g.UpdatedAt, 0),
		}
	}
	return s, nil
}

// EnsureGroup 实现 ports.Store。
func (s *YAMLStore) EnsureGroup(_ context.Context, chatID int64, defaults domain.GroupConfig) (domain.GroupConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cfg, ok := s.data[chatID]; ok {
		return cfg, nil
	}
	defaults.ChatID = chatID
	defaults.Revision = 1
	defaults.UpdatedAt = time.Now()
	s.data[chatID] = defaults
	if err := s.flushLocked(); err != nil {
		return domain.GroupConfig{}, err
	}
	return defaults, nil
}

// GetGroup 实现 ports.Store。
func (s *YAMLStore) GetGroup(_ context.Context, chatID int64) (domain.GroupConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, ok := s.data[chatID]
	if !ok {
		return domain.GroupConfig{}, ports.ErrNotFound
	}
	return cfg, nil
}

// SaveGroup 实现 ports.Store（CAS + 原子写盘）。
func (s *YAMLStore) SaveGroup(_ context.Context, cfg domain.GroupConfig, expectedRevision uint64) (domain.GroupConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writes {
		return domain.GroupConfig{}, fmt.Errorf("storage.yaml.runtime_writes=false，禁止运行时改写 %s", s.path)
	}
	cur, ok := s.data[cfg.ChatID]
	if expectedRevision == 0 {
		if ok {
			return domain.GroupConfig{}, ports.ErrConflict
		}
	} else if !ok || cur.Revision != expectedRevision {
		return domain.GroupConfig{}, ports.ErrConflict
	}
	cfg.Revision = expectedRevision + 1
	cfg.UpdatedAt = time.Now()
	s.data[cfg.ChatID] = cfg
	if err := s.flushLocked(); err != nil {
		return domain.GroupConfig{}, err
	}
	return cfg, nil
}

// Probe 实现 ports.Store：验证配置文件可解析、目录可写，且不写入任何群配置。
func (s *YAMLStore) Probe(_ context.Context) error {
	if !s.writes {
		// 只读模式：只验证现有文件能被解析（构造函数已经做过一次，这里再确认一次可读）。
		if _, err := os.ReadFile(s.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("YAML 群配置不可读: %w", err)
		}
		return nil
	}
	dir := filepath.Dir(s.path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("创建 YAML 目录 %s 失败: %w", dir, err)
		}
	}
	f, err := os.CreateTemp(dir, ".gatekeeper-probe-*.yaml")
	if err != nil {
		return fmt.Errorf("目录不可写（%s）: %w", dir, err)
	}
	name := f.Name()
	_, werr := f.WriteString("groups: []\n")
	cerr := f.Close()
	_ = os.Remove(name) // 探针文件随即删除，不留残留
	if werr != nil {
		return fmt.Errorf("写入探针文件失败: %w", werr)
	}
	if cerr != nil {
		return fmt.Errorf("关闭探针文件失败: %w", cerr)
	}
	return nil
}

// Close 实现 ports.Store。
func (s *YAMLStore) Close() error { return nil }

func (s *YAMLStore) flushLocked() error {
	ids := make([]int64, 0, len(s.data))
	for id := range s.data {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	f := yamlFile{}
	for _, id := range ids {
		cfg := s.data[id]
		f.Groups = append(f.Groups, yamlGroup{
			ChatID:         cfg.ChatID,
			Enabled:        cfg.Enabled,
			Mode:           string(cfg.Mode),
			RulesMessageID: cfg.RulesMessageID,
			RulesLink:      cfg.RulesLink,
			Welcome:        cfg.Welcome,
			Tag:            cfg.Tag,
			AdWords:        cfg.AdWords,
			Revision:       cfg.Revision,
			UpdatedAt:      cfg.UpdatedAt.Unix(),
		})
	}
	raw, err := yaml.Marshal(&f)
	if err != nil {
		return fmt.Errorf("序列化群配置失败: %w", err)
	}
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".groups-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入群配置失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsync 群配置失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Chmod(tmpName, s.fileMode); err != nil {
		return fmt.Errorf("设置文件权限失败: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("原子替换群配置失败: %w", err)
	}
	return nil
}
