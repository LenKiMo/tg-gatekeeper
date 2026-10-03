package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExampleConfigLoads 保证仓库自带的示例配置一定能被真实加载器解析。
//
// 配置是 strict 模式（未知字段直接报错），示例里写错一个键名，用户复制粘贴后
// check-config 就会失败——CI 曾经就是这样抓到 rules_link 没在结构体里定义的。
func TestExampleConfigLoads(t *testing.T) {
	for _, rel := range []string{
		filepath.Join("..", "..", "configs", "gatekeeper.example.yaml"),
	} {
		if _, err := os.Stat(rel); err != nil {
			t.Fatalf("示例配置不存在: %v", err)
		}
		cfg, err := Load(rel)
		if err != nil {
			t.Fatalf("示例配置加载失败（%s）: %v", rel, err)
		}
		if cfg.Gatekeeper.OptionCount <= 0 {
			t.Fatalf("示例配置的选项数异常: %d", cfg.Gatekeeper.OptionCount)
		}
	}
}
