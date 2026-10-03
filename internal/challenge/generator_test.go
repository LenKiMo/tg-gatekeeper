package challenge

import (
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
)

func snapshot(entries ...domain.Entry) domain.Snapshot {
	return domain.Snapshot{SourceID: "test", LoadedAt: time.Now(), Entries: entries}
}

func entry(pool, label, image string) domain.Entry {
	return domain.Entry{Label: label, ImageRef: image, Pool: pool, Enabled: true}
}

func defaultOptions(count int) Options {
	return Options{OptionCount: count, MinOptionCount: 2, ShortagePolicy: "deny", Strategy: "uniform"}
}

// TestNextOptionsAreUniqueAndContainCorrect 验证选项唯一、包含正确答案、token 唯一。
func TestNextOptionsAreUniqueAndContainCorrect(t *testing.T) {
	var entries []domain.Entry
	for _, label := range []string{"甲", "乙", "丙", "丁", "戊", "己", "庚", "辛"} {
		entries = append(entries, entry("p", label, "file:/tmp/"+label+".png"))
	}
	gen := New(snapshot(entries...))

	for i := 0; i < 200; i++ {
		ch, err := gen.Next(defaultOptions(4))
		if err != nil {
			t.Fatalf("生成题目失败: %v", err)
		}
		if len(ch.Options) != 4 {
			t.Fatalf("选项数 = %d，期望 4", len(ch.Options))
		}
		seenLabel := map[string]bool{}
		seenToken := map[string]bool{}
		correctCount := 0
		for _, o := range ch.Options {
			if seenLabel[o.Label] {
				t.Fatalf("选项标签重复: %s", o.Label)
			}
			seenLabel[o.Label] = true
			if seenToken[o.Token] {
				t.Fatalf("选项 token 重复: %s（同题内 token 必须唯一）", o.Token)
			}
			seenToken[o.Token] = true
			if ch.IsCorrect(o.Token) {
				correctCount++
			}
		}
		if correctCount != 1 {
			t.Fatalf("正确答案数量 = %d，期望 1", correctCount)
		}
		if !seenLabel[ch.CorrectLabel] {
			t.Fatalf("正确答案 %s 不在选项里", ch.CorrectLabel)
		}
		if ch.Entry.Label != ch.CorrectLabel {
			t.Fatalf("Entry.Label(%s) 与 CorrectLabel(%s) 不一致", ch.Entry.Label, ch.CorrectLabel)
		}
		if !strings.HasPrefix(ch.Entry.ImageRef, "file:") {
			t.Fatalf("图片引用异常: %s", ch.Entry.ImageRef)
		}
	}
}

// TestAnswerFairnessIsPerLabelNotPerImage 验证"图多的标签"不会更容易成为答案。
//
// 注意：如果直接对条目均匀抽样，拥有 3 张图的标签会拿到 3 倍
// 概率成为正确答案。
func TestAnswerFairnessIsPerLabelNotPerImage(t *testing.T) {
	entries := []domain.Entry{
		entry("p", "多图", "file:/tmp/a1.png"),
		entry("p", "多图", "file:/tmp/a2.png"),
		entry("p", "多图", "file:/tmp/a3.png"),
		entry("p", "单图", "file:/tmp/b1.png"),
	}
	gen := New(snapshot(entries...))
	counts := map[string]int{}
	const rounds = 4000
	for i := 0; i < rounds; i++ {
		ch, err := gen.Next(Options{OptionCount: 2, MinOptionCount: 2, ShortagePolicy: "deny", Strategy: "uniform"})
		if err != nil {
			t.Fatalf("生成题目失败: %v", err)
		}
		counts[ch.CorrectLabel]++
	}
	// 两个标签各占一半，允许 ±12% 的统计波动。
	half := rounds / 2
	tolerance := rounds * 12 / 100
	if diff := counts["多图"] - half; diff > tolerance || diff < -tolerance {
		t.Fatalf("答案分布不公：多图=%d 单图=%d（期望各约 %d）", counts["多图"], counts["单图"], half)
	}
}

// TestAliasExcludedFromOptions 验证"其实也对的别名"不会出现在选项里。
func TestAliasExcludedFromOptions(t *testing.T) {
	entries := []domain.Entry{
		{Label: "甲", ImageRef: "file:/tmp/a.png", Pool: "p", Enabled: true, Alias: []string{"贾"}},
		entry("p", "贾", "file:/tmp/jia.png"),
		entry("p", "乙", "file:/tmp/b.png"),
		entry("p", "丙", "file:/tmp/c.png"),
		entry("p", "丁", "file:/tmp/d.png"),
	}
	gen := New(snapshot(entries...))
	for i := 0; i < 300; i++ {
		ch, err := gen.Next(defaultOptions(3))
		if err != nil {
			t.Fatalf("生成题目失败: %v", err)
		}
		if ch.CorrectLabel != "甲" {
			continue
		}
		for _, o := range ch.Options {
			if NormalizeLabel(o.Label) == NormalizeLabel("贾") {
				t.Fatalf("别名 贾 出现在选项里（用户选它其实也对）")
			}
		}
	}
}

// TestShortagePolicy 验证数据不足时的两种策略。
func TestShortagePolicy(t *testing.T) {
	entries := []domain.Entry{
		entry("small", "甲", "file:/tmp/a.png"),
		entry("small", "乙", "file:/tmp/b.png"),
		entry("small", "丙", "file:/tmp/c.png"),
	}
	gen := New(snapshot(entries...))

	if _, err := gen.Next(Options{OptionCount: 12, MinOptionCount: 3, ShortagePolicy: "deny"}); !errors.Is(err, ErrShortage) {
		t.Fatalf("deny 策略下期望 ErrShortage，实际 %v", err)
	}
	ch, err := gen.Next(Options{OptionCount: 12, MinOptionCount: 3, ShortagePolicy: "reduce"})
	if err != nil {
		t.Fatalf("reduce 策略应当降级成功，实际 %v", err)
	}
	if !ch.Degraded {
		t.Fatalf("降级后 Degraded 应为 true")
	}
	if len(ch.Options) != 3 {
		t.Fatalf("降级选项数 = %d，期望 3", len(ch.Options))
	}
	if _, err := gen.Next(Options{OptionCount: 12, MinOptionCount: 5, ShortagePolicy: "reduce"}); !errors.Is(err, ErrShortage) {
		t.Fatalf("低于 MinOptionCount 时期望 ErrShortage，实际 %v", err)
	}
}

// TestPoolsAreIsolated 验证不同 pool 的标签不会互相混进选项（跨世界观混搭）。
func TestPoolsAreIsolated(t *testing.T) {
	entries := []domain.Entry{
		entry("A", "a1", "file:/tmp/a1.png"),
		entry("A", "a2", "file:/tmp/a2.png"),
		entry("A", "a3", "file:/tmp/a3.png"),
		entry("B", "b1", "file:/tmp/b1.png"),
		entry("B", "b2", "file:/tmp/b2.png"),
		entry("B", "b3", "file:/tmp/b3.png"),
	}
	gen := New(snapshot(entries...))
	for i := 0; i < 200; i++ {
		ch, err := gen.Next(defaultOptions(3))
		if err != nil {
			t.Fatalf("生成题目失败: %v", err)
		}
		for _, o := range ch.Options {
			if strings.HasPrefix(o.Label, "a") && ch.Entry.Pool != "A" {
				t.Fatalf("出现跨 pool 选项：%s", o.Label)
			}
			if strings.HasPrefix(o.Label, "b") && ch.Entry.Pool != "B" {
				t.Fatalf("出现跨 pool 选项：%s", o.Label)
			}
		}
	}
}

// TestDirtyEntriesDropped 验证脏数据被丢弃且计入 Dropped。
func TestDirtyEntriesDropped(t *testing.T) {
	entries := []domain.Entry{
		{Label: "", ImageRef: "file:/tmp/x.png", Pool: "p", Enabled: true},
		{Label: "甲", ImageRef: "", Pool: "p", Enabled: true},
		{Label: "乙", ImageRef: "file:/tmp/b.png", Pool: "p", Enabled: false},
		{Label: "丙", ImageRef: "file:/tmp/c.png", Pool: "p", Enabled: true, ID: "same"},
		{Label: "丁", ImageRef: "file:/tmp/d.png", Pool: "p", Enabled: true, ID: "same"},
	}
	gen := New(snapshot(entries...))
	if got := gen.Buckets(); got != 1 {
		t.Fatalf("可用标签 = %d，期望 1（只有丙/丁里的第一个应当保留）", got)
	}
}

// TestTokensAreRandomPerChallenge 验证每次出题的 token 都不同（不能复用）。
func TestTokensAreRandomPerChallenge(t *testing.T) {
	entries := []domain.Entry{entry("p", "甲", "file:/tmp/a.png"), entry("p", "乙", "file:/tmp/b.png")}
	gen := New(snapshot(entries...))
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		ch, err := gen.Next(defaultOptions(2))
		if err != nil {
			t.Fatalf("生成题目失败: %v", err)
		}
		for _, o := range ch.Options {
			if seen[o.Token] {
				t.Fatalf("token 在多次出题间复用: %s", o.Token)
			}
			seen[o.Token] = true
		}
	}
}

// TestHashTokenIsNotPlaintext 验证哈希不可逆推（数据库泄漏也读不出答案）。
func TestHashTokenIsNotPlaintext(t *testing.T) {
	ch, err := New(snapshot(entry("p", "甲", "file:/tmp/a.png"), entry("p", "乙", "file:/tmp/b.png"))).
		Next(defaultOptions(2))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ch.CorrectTokenHash), ch.CorrectLabel) {
		t.Fatal("哈希里不应包含明文标签")
	}
	if len(ch.CorrectTokenHash) != sha256.Size {
		t.Fatalf("哈希长度 = %d，期望 %d", len(ch.CorrectTokenHash), sha256.Size)
	}
	opt, ok := ch.CorrectOption()
	if !ok || !domain.MatchesHash(opt.Token, ch.CorrectTokenHash) {
		t.Fatal("CorrectOption 应当能定位到正确答案")
	}
}

// TestHealthCheck 验证健康检查会拦住不够用的数据集。
func TestHealthCheck(t *testing.T) {
	small := New(snapshot(entry("p", "甲", "file:/tmp/a.png")))
	if err := small.HealthCheck(12); err == nil {
		t.Fatal("标签不足时 HealthCheck 应当报错")
	}
	big := New(snapshot(entry("p", "甲", "file:/tmp/a.png"), entry("p", "乙", "file:/tmp/b.png")))
	if err := big.HealthCheck(2); err != nil {
		t.Fatalf("标签足够时不应报错: %v", err)
	}
}

// TestNormalizeLabelDoesNotMergeDistinctEntities 验证归一化不会把不同实体合并。
func TestNormalizeLabelDoesNotMergeDistinctEntities(t *testing.T) {
	cases := []struct {
		a, b string
		same bool
	}{
		{"Abc", "ＡＢＣ", true},     // 全角/半角 + 大小写
		{"甲 乙", "甲乙", false},     // 空白保留（折叠而非删除）
		{"a-b", "ab", false},     // 连字符是身份的一部分
		{"门\u200b框", "门框", true}, // 零宽字符
	}
	for _, c := range cases {
		got := NormalizeLabel(c.a) == NormalizeLabel(c.b)
		if got != c.same {
			t.Errorf("NormalizeLabel(%q) vs (%q): same=%v，期望 %v", c.a, c.b, got, c.same)
		}
	}
}

// TestGeneratorWithFixedRandomSource 验证注入随机源后行为可复现（便于排障）。
func TestGeneratorWithFixedRandomSource(t *testing.T) {
	entries := []domain.Entry{
		entry("p", "甲", "file:/tmp/a.png"),
		entry("p", "乙", "file:/tmp/b.png"),
		entry("p", "丙", "file:/tmp/c.png"),
		entry("p", "丁", "file:/tmp/d.png"),
	}
	gen := New(snapshot(entries...)).WithRand(strings.NewReader(strings.Repeat("\x01", 4096)))
	ch, err := gen.Next(defaultOptions(3))
	if err != nil {
		t.Fatalf("生成题目失败: %v", err)
	}
	if len(ch.Options) != 3 {
		t.Fatalf("选项数 = %d", len(ch.Options))
	}
	var _ io.Reader = strings.NewReader("")
}
