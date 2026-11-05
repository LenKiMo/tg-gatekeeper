package config

import (
	"path/filepath"
	"testing"
)

// TestAdminBanSecondsDefaultAndValidation 验证 admin_ban_seconds 默认 7 天；
// 只接受 0（永久）或不小于 60 的值。
func TestAdminBanSecondsDefaultAndValidation(t *testing.T) {
	const week = 7 * 24 * 3600
	if got := Default().Gatekeeper.AdminBanSeconds; got != week {
		t.Fatalf("默认值应为 %d，实际 %d", week, got)
	}
	cfg, err := Load(filepath.Join("..", "..", "configs", "gatekeeper.example.yaml"))
	if err != nil {
		t.Fatalf("示例配置加载失败: %v", err)
	}
	if cfg.Gatekeeper.AdminBanSeconds != week {
		t.Fatalf("示例配置应为 %d，实际 %d", week, cfg.Gatekeeper.AdminBanSeconds)
	}
	for _, bad := range []int{1, 30, 59, -1} {
		cfg.Gatekeeper.AdminBanSeconds = bad
		if err := cfg.Validate(); err == nil {
			t.Errorf("admin_ban_seconds=%d 应当校验失败", bad)
		}
	}
	for _, good := range []int{0, 60, week} {
		cfg.Gatekeeper.AdminBanSeconds = good
		if err := cfg.Validate(); err != nil {
			t.Errorf("admin_ban_seconds=%d 应当通过校验: %v", good, err)
		}
	}
}
