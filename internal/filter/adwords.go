// Package filter 实现昵称/简介的广告词过滤。
//
// 归一化比"标签归一化"更激进：广告投放者会用全角、零宽字符、空格和分隔符来绕过
// 关键词。这里先做 NFKC，再去零宽字符、去空白、去可配置分隔符，最后大小写折叠，
// 然后做子串匹配（首版不做正则，避免运维写出回溯炸弹）。
package filter

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
)

// AdFilter 是广告词过滤器。
type AdFilter struct {
	cfg    config.AdFilter
	global []string
}

// New 构造过滤器；空词会被忽略（空词匹配一切，是危险的配置）。
func New(cfg config.AdFilter) *AdFilter {
	f := &AdFilter{cfg: cfg}
	for _, w := range cfg.GlobalWords {
		if norm2(w, cfg) != "" {
			f.global = append(f.global, w)
		}
	}
	return f
}

// Check 检查文本是否命中广告词；groupWords 是群自加的追加词。
// 返回命中的原始词（用于审计；不返回归一化后的结果，避免运维看不懂）。
func (f *AdFilter) Check(text string, groupWords []string) (string, bool) {
	if !f.cfg.Enabled || strings.TrimSpace(text) == "" {
		return "", false
	}
	hay := norm2(text, f.cfg)
	if hay == "" {
		return "", false
	}
	for _, w := range f.global {
		if n := norm2(w, f.cfg); n != "" && strings.Contains(hay, n) {
			return w, true
		}
	}
	if f.cfg.IncludeGroupWords {
		for _, w := range groupWords {
			if n := norm2(w, f.cfg); n != "" && strings.Contains(hay, n) {
				return w, true
			}
		}
	}
	return "", false
}

// Words 返回生效的全局广告词数量（供启动日志）。
func (f *AdFilter) Words() int { return len(f.global) }

// norm2 按配置做归一化。
func norm2(s string, cfg config.AdFilter) string {
	if cfg.NormalizeNFKC {
		s = norm.NFKC.String(s)
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if cfg.RemoveZeroWidth && isZeroWidth(r) {
			continue
		}
		if cfg.RemoveWhitespace && unicode.IsSpace(r) {
			continue
		}
		if len(cfg.RemovableSeparators) > 0 && isSeparator(r, cfg.RemovableSeparators) {
			continue
		}
		if cfg.IgnoreCase {
			r = unicode.ToLower(r)
		}
		// NFKC 之后全角已转半角；这里再折叠一次常见全角标点，避免 "代·肝" 之类绕过。
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func isZeroWidth(r rune) bool {
	switch r {
	case '\u200b', '\u200c', '\u200d', '\u2060', '\ufeff', '\u00ad', '\u180e':
		return true
	}
	return false
}

func isSeparator(r rune, seps []string) bool {
	for _, s := range seps {
		if s == "" {
			continue
		}
		for _, sr := range s {
			if sr == r {
				return true
			}
		}
	}
	return false
}
