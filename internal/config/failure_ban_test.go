package config

import (
	"path/filepath"
	"testing"
)

// TestFailureBanSecondsDefaultAndFloor 验证 failure_ban_seconds 默认 600，且不接受小于 60 的值。
//
// Telegram 把"距现在不足 30 秒"的封禁到期时间当成永久封禁；这个配置管的是机器人自身
// 故障时的封禁，配得太小等于把故障变成永久拉黑。
func TestFailureBanSecondsDefaultAndFloor(t *testing.T) {
	if got := Default().Gatekeeper.FailureBanSeconds; got != 600 {
		t.Fatalf("默认值应为 600，实际 %d", got)
	}
	cfg, err := Load(filepath.Join("..", "..", "configs", "gatekeeper.example.yaml"))
	if err != nil {
		t.Fatalf("示例配置加载失败: %v", err)
	}
	if cfg.Gatekeeper.FailureBanSeconds != 600 {
		t.Fatalf("示例配置应为 600，实际 %d", cfg.Gatekeeper.FailureBanSeconds)
	}
	for _, bad := range []int{0, 30, 59, -1} {
		cfg.Gatekeeper.FailureBanSeconds = bad
		if err := cfg.Validate(); err == nil {
			t.Errorf("failure_ban_seconds=%d 应当校验失败", bad)
		}
	}
	cfg.Gatekeeper.FailureBanSeconds = 60
	if err := cfg.Validate(); err != nil {
		t.Errorf("failure_ban_seconds=60 应当通过校验: %v", err)
	}
}
