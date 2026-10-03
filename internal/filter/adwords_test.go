package filter

import (
	"testing"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
)

func testFilter(words ...string) *AdFilter {
	cfg := config.Default().AdFilter
	cfg.GlobalWords = words
	return New(cfg)
}

// TestAdFilterCatchesPlainWords 基础命中。
func TestAdFilterCatchesPlainWords(t *testing.T) {
	f := testFilter("代肝", "代充")
	if _, hit := f.Check("专业代肝，价格优惠", nil); !hit {
		t.Fatal("应当命中 代肝")
	}
	if _, hit := f.Check("普通玩家", nil); hit {
		t.Fatal("不应命中普通文本")
	}
}

// TestAdFilterDefeatsEvasion 验证常见绕过手法（全角、零宽、空格、分隔符、大小写）。
func TestAdFilterDefeatsEvasion(t *testing.T) {
	cases := []string{
		"代肝",       // 正常
		"代　肝",      // 全角空格
		"代\u200b肝", // 零宽空格
		"代-肝",      // 连字符
		"代_肝",      // 下划线
		"代·肝",      // 间隔号
		"代肝代肝",     // 重复
		"代 肝",      // 半角空格
	}
	f := testFilter("代肝")
	for _, c := range cases {
		if _, hit := f.Check(c, nil); !hit {
			t.Errorf("应当命中变体 %q", c)
		}
	}

	// 大小写与全角字母。
	f2 := testFilter("add vx")
	for _, c := range []string{"Please Add VX now", "ＡＤＤ VX", "addvx"} {
		if _, hit := f2.Check(c, nil); !hit {
			t.Errorf("应当命中英文变体 %q", c)
		}
	}
}

// TestAdFilterDoesNotMatchEverything 防止空词/空白词造成"拦截一切"。
func TestAdFilterDoesNotMatchEverything(t *testing.T) {
	f := New(config.AdFilter{
		Enabled: true, NormalizeNFKC: true, IgnoreCase: true,
		RemoveWhitespace: true, RemoveZeroWidth: true,
		GlobalWords: []string{"", "   ", "\u200b"},
	})
	if f.Words() != 0 {
		t.Fatalf("空词应当被丢弃，实际保留 %d 个", f.Words())
	}
	if word, hit := f.Check("完全正常的昵称", nil); hit {
		t.Fatalf("不应命中，实际命中 %q", word)
	}
}

// TestAdFilterGroupWords 验证群内追加词。
func TestAdFilterGroupWords(t *testing.T) {
	f := testFilter("全群通用词")
	if _, hit := f.Check("本群专属广告", []string{"专属广告"}); !hit {
		t.Fatal("群内追加词应当生效")
	}
	// include_group_words=false 时不生效。
	cfg := config.Default().AdFilter
	cfg.GlobalWords = []string{"全群通用词"}
	cfg.IncludeGroupWords = false
	f2 := New(cfg)
	if _, hit := f2.Check("本群专属广告", []string{"专属广告"}); hit {
		t.Fatal("include_group_words=false 时群内词不应生效")
	}
}

// TestAdFilterDisabled 关闭后一律放行。
func TestAdFilterDisabled(t *testing.T) {
	cfg := config.Default().AdFilter
	cfg.Enabled = false
	cfg.GlobalWords = []string{"代肝"}
	f := New(cfg)
	if _, hit := f.Check("专业代肝", nil); hit {
		t.Fatal("过滤器关闭后不应命中")
	}
}

// TestAdFilterReturnsOriginalWord 命中的词应当是原始词（便于审计阅读）。
func TestAdFilterReturnsOriginalWord(t *testing.T) {
	f := testFilter("代肝")
	word, hit := f.Check("代　肝", nil)
	if !hit || word != "代肝" {
		t.Fatalf("应当返回原始词，实际 %q hit=%v", word, hit)
	}
}
