// Package challenge 负责从数据集快照生成一道题。
//
// 与题材、存储、Telegram 完全无关，因此"正确项是否公平""选项是否唯一""数据不足
// 怎么办"这些问题都能用纯单元测试钉死。
package challenge

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// NormalizeLabel 把标签归一化为"身份 key"，用于判重与冲突检测。
//
// 只有真正等价的写法才会归一化到一起：做 NFKC（全角/兼容字符）、大小写折叠、
// 去掉零宽字符、折叠内部空白。刻意不删分隔符（"-"、"·"），否则会错把
// "A-B" 与 "AB" 当成同一个实体。
func NormalizeLabel(s string) string {
	s = norm.NFKC.String(s)
	var b strings.Builder
	b.Grow(len(s))
	lastSpace := false
	for _, r := range s {
		if isZeroWidth(r) {
			continue
		}
		if unicode.IsSpace(r) {
			if !lastSpace && b.Len() > 0 {
				b.WriteRune(' ')
				lastSpace = true
			}
			continue
		}
		lastSpace = false
		b.WriteRune(unicode.ToLower(r))
	}
	return strings.TrimSpace(b.String())
}

func isZeroWidth(r rune) bool {
	switch r {
	case '\u200b', '\u200c', '\u200d', '\u2060', '\ufeff', '\u00ad':
		return true
	}
	return false
}

// LabelKey 是带 pool 的完整身份 key。
func LabelKey(pool, label string) string {
	return pool + "\x00" + NormalizeLabel(label)
}

// EntryID 为缺少显式 ID 的条目派生稳定 ID：只依赖 pool + 归一化标签，
// 因此换图不会改变身份（这样"同一实体的历史审计"仍然连得上）。
func EntryID(pool, label string) string {
	k := LabelKey(pool, label)
	if len(k) <= 40 {
		return hashHex([]byte(k))[:16]
	}
	return hashHex([]byte(k))[:16]
}
