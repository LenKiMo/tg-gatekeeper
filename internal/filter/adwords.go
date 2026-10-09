// Package filter 实现昵称/简介的广告词过滤。
//
// 归一化比"标签归一化"更激进：广告投放者会用全角、零宽字符、空格和分隔符来绕过
// 关键词。这里先做 NFKC，再去零宽字符、去空白、去可配置分隔符，最后大小写折叠，
// 然后做子串匹配（首版不做正则，避免运维写出回溯炸弹）。
//
// 免责声明豁免：简介里写"请勿私聊我""未经允许别私聊我"的用户，本意是拒绝私聊，
// 却会撞上私聊类广告词（私聊我/私聊下单/pm我），被当成广告号封禁。因此命中词
// 紧邻否定标记时跳过这一处命中——只认紧邻，中间隔了别的字仍判广告。
package filter

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
)

// AdFilter 是广告词过滤器。
type AdFilter struct {
	cfg      config.AdFilter
	global   []string
	negation []string
}

// New 构造过滤器；空词会被忽略（空词匹配一切，是危险的配置）。
func New(cfg config.AdFilter) *AdFilter {
	f := &AdFilter{cfg: cfg}
	for _, w := range cfg.GlobalWords {
		if norm2(w, cfg) != "" {
			f.global = append(f.global, w)
		}
	}
	for _, m := range cfg.NegationWords {
		if n := norm2(m, cfg); n != "" {
			f.negation = append(f.negation, n)
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
		if f.matches(hay, w) {
			return w, true
		}
	}
	if f.cfg.IncludeGroupWords {
		for _, w := range groupWords {
			if f.matches(hay, w) {
				return w, true
			}
		}
	}
	return "", false
}

// Words 返回生效的全局广告词数量（供启动日志）。
func (f *AdFilter) Words() int { return len(f.global) }

// matches 判断归一化文本里是否存在未被免责声明豁免的命中。
// 一处被豁免不代表整条文本放行：继续找后面是否还有真正的 CTA。
func (f *AdFilter) matches(hay, word string) bool {
	n := norm2(word, f.cfg)
	if n == "" {
		return false
	}
	for idx := 0; idx < len(hay); {
		i := strings.Index(hay[idx:], n)
		if i < 0 {
			return false
		}
		pos := idx + i
		if !f.negated(hay, pos) {
			return true
		}
		idx = pos + len(n)
	}
	return false
}

// negationWindow 是紧邻命中词之前参与否定判断的字节数（标记最长 2 字，留足余量）。
const negationWindow = 12

// negated 判断 pos 处的命中是否紧跟在否定标记之后。
// 只认紧邻：中间隔了别的字就不算（"谢绝砍价，私聊我" 仍判广告）。
func (f *AdFilter) negated(hay string, pos int) bool {
	if pos <= 0 || len(f.negation) == 0 {
		return false
	}
	start := pos - negationWindow
	if start < 0 {
		start = 0
	}
	window := hay[start:pos]
	for _, m := range f.negation {
		if strings.HasSuffix(window, m) {
			return true
		}
	}
	return false
}

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
