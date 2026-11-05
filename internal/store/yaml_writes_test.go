package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
)

// TestEnsureGroupRespectsRuntimeWrites runtime_writes=false 时，配置只从文件读：
// 遇到未配置的群也不能往盘上写（以前 EnsureGroup 没有这个检查，任何一条群消息都会写盘）。
func TestEnsureGroupRespectsRuntimeWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "groups.yaml")
	cfg := config.Default()
	cfg.Storage.YAML.Path = path
	cfg.Storage.YAML.RuntimeWrites = false

	st, err := NewYAMLStore(cfg)
	if err != nil {
		t.Fatalf("打开 YAML 存储失败: %v", err)
	}
	got, err := st.EnsureGroup(context.Background(), -100, domain.GroupConfig{Enabled: true, Mode: domain.VerifyModeJoin})
	if err != nil {
		t.Fatalf("EnsureGroup 失败: %v", err)
	}
	if got.ChatID != -100 {
		t.Fatalf("应当返回默认配置，实际 %+v", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("runtime_writes=false 仍然写了盘：err=%v", err)
	}
}
