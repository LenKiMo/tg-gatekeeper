package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProviderDefinitionsExpandEnv provider.definitions 的值是结构体（map[string]ProviderDef），
// 里面的 ${ENV} 以前不会展开，token / dsn 会原样留在字面值上。
func TestProviderDefinitionsExpandEnv(t *testing.T) {
	t.Setenv("REVIEW_BEARER", "s3cr3t-value")
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	body := `schema_version: 1
bot:
  token: "123:abc"
provider:
  active: main
  definitions:
    main:
      type: http
      http:
        url: "https://example.com/items.json"
        bearer_token: "${REVIEW_BEARER}"
        allowed_hosts: ["example.com"]
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	got := cfg.Provider.Definitions["main"].HTTP.BearerToken
	if got != "s3cr3t-value" {
		t.Fatalf("provider.definitions 里的 ${ENV} 没有展开：%q", got)
	}
}
