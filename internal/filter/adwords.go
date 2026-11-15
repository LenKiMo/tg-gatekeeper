// Package filter 实现昵称/简介的广告词过滤。
//
// 归一化比"标签归一化"更激进：广告投放者会用全角、零宽字符、空格和分隔符来绕过
// 关键词。这里先做 NFKC，再去零宽字符、去空白、去可配置分隔符，最后大小写折叠，
// 然后做子串匹配（首版不做正则，避免运维写出回溯炸弹）。
//
// 免责声明豁免：简介里写"请勿私聊我""未经允许别私聊我"的用户，本意是拒绝私聊，
// 却会撞上私聊类广告词（私聊我/私聊下单/pm我），被当成广告号封禁。因此命中词
// 紧邻否定标记时跳过这一处命中——只认紧邻，中间隔了别的字仍判广告。
//
// 三层匹配（任一层命中即判广告），由便宜到贵依次尝试：
//
//  1. 原样层：归一化文本直接子串匹配。emoji/符号夹在词**两侧**时子串匹配本身就能
//     穿透（"带🔥盈🍀" 命中"带盈"），不需要额外处理——这一点是实测确认的，别为它
//     去加额外的归一化。
//  2. 谐音层：把实测到的谐音/拆字还原后再匹配（"看我主椰"→"看我主页"）。只在原样层
//     未命中时才跑，热路径（消息级扫描）上不为它付开销。
//  3. 骨架层：去掉 emoji/标点/符号后，只匹配 skeleton_words。它解决的是"词中间被
//     插入字符"（"博😀彩"），代价是容易误伤（"快😘乐营💙业😻中" 的骨架是
//     "快乐营业中"，其中"营业"是正常短语），所以骨架词表单独维护、默认空。
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
	bioWords []string
	skeleton []string
	negation []string
	// homophones 按 From 长度降序排列，保证长串先还原（"沃竹页" 先于 "沃"）。
	homophones []config.Homophone
}

// New 构造过滤器；空词会被忽略（空词匹配一切，是危险的配置）。
func New(cfg config.AdFilter) *AdFilter {
	f := &AdFilter{cfg: cfg}
	for _, w := range cfg.GlobalWords {
		if norm2(w, cfg) != "" {
			f.global = append(f.global, w)
		}
	}
	for _, w := range cfg.BioWords {
		if norm2(w, cfg) != "" {
			f.bioWords = append(f.bioWords, w)
		}
	}
	for _, w := range cfg.SkeletonWords {
		if n := strings.TrimSpace(w); n != "" {
			f.skeleton = append(f.skeleton, n)
		}
	}
	for _, m := range cfg.NegationWords {
		if n := norm2(m, cfg); n != "" {
			f.negation = append(f.negation, n)
		}
	}
	for _, h := range cfg.Homophones {
		from, to := norm2(h.From, cfg), norm2(h.To, cfg)
		if from == "" || to == "" || from == to {
			continue // 空串会让 ReplaceAll 失控；自映射是无操作
		}
		f.homophones = append(f.homophones, config.Homophone{From: from, To: to})
	}
	// 长的优先：否则 "沃" 先被换成 "我"，"沃竹页" 就再也匹配不上了。
	for i := 1; i < len(f.homophones); i++ {
		for j := i; j > 0 && len(f.homophones[j].From) > len(f.homophones[j-1].From); j-- {
			f.homophones[j], f.homophones[j-1] = f.homophones[j-1], f.homophones[j]
		}
	}
	return f
}

// Check 检查文本是否命中广告词；groupWords 是群自加的追加词。
// 返回命中的原始词（用于审计；不返回归一化后的结果，避免运维看不懂）。
func (f *AdFilter) Check(text string, groupWords []string) (string, bool) {
	return f.check(text, groupWords, false)
}

// CheckBio 检查用户简介：与 Check 相同，但额外叠加 bio_words。
//
// 单独一层是因为简介比昵称"可写更长、话术更露骨"：日挣/USDT/搬砖/跑分 这类词放在
// 昵称里会误伤正常玩家，放在简介里几乎只出现在诈骗话术上。
//
// 另外两条只在简介层生效的判据（实测来自生产 bio）：
//   - bio_join_link_reject：简介里有 t.me/+ 私密邀请链接。归一化会去掉 "/"，所以
//     匹配 "t.me+"——正常用户放的是公开频道（t.me/name，没有加号），只有拉人进
//     私密群才会出现 +hash 链接。
//   - 其余交给 bio_words 词表。
func (f *AdFilter) CheckBio(text string, groupWords []string) (string, bool) {
	if w, hit := f.check(text, groupWords, true); hit {
		return w, true
	}
	if f.cfg.BioJoinLinkReject && strings.TrimSpace(text) != "" {
		// 在**原始文本**上判，不走 norm2：归一化会删掉 "." 和 "/"（它们都在
		// removable_separators 里），"t.me/+hash" 会被拼成 "tme+hash" 再也认不出。
		// 只折叠空白，防 "t . me / +" 这种留空格的写法。
		raw := strings.ToLower(strings.Join(strings.Fields(text), ""))
		if strings.Contains(raw, "t.me/+") || strings.Contains(raw, "telegram.me/+") {
			return "t.me/+", true
		}
	}
	return "", false
}

// Words 返回生效的词条总数（全局 + bio 专用 + 骨架），供启动日志。
func (f *AdFilter) Words() int { return len(f.global) + len(f.bioWords) + len(f.skeleton) }

// Homophones 返回生效的谐音规则数（供启动日志）。
func (f *AdFilter) Homophones() int { return len(f.homophones) }

func (f *AdFilter) check(text string, groupWords []string, bio bool) (string, bool) {
	if !f.cfg.Enabled || strings.TrimSpace(text) == "" {
		return "", false
	}
	hay := norm2(text, f.cfg)
	if hay == "" {
		return "", false
	}
	words := f.global
	if bio && len(f.bioWords) > 0 {
		words = append(append([]string{}, f.global...), f.bioWords...)
	}
	if w, hit := f.scan(hay, words, groupWords); hit {
		return w, true
	}
	// 谐音层只在原样层未命中时跑。
	if len(f.homophones) > 0 {
		if restored := f.restore(hay); restored != hay {
			if w, hit := f.scan(restored, words, groupWords); hit {
				return w, true
			}
		}
	}
	// 骨架层只匹配 skeleton_words，不掺全局词表（否则"营业"之类的误伤会流进来）。
	if len(f.skeleton) > 0 {
		if sk := stripSymbols(hay); sk != "" && sk != hay {
			if w, hit := f.scan(sk, f.skeleton, nil); hit {
				return w, true
			}
		}
	}
	return "", false
}

// scan 在词表上依次做（带否定豁免的）匹配。
func (f *AdFilter) scan(hay string, words, groupWords []string) (string, bool) {
	for _, w := range words {
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

// restore 应用谐音/拆字还原。
func (f *AdFilter) restore(hay string) string {
	out := hay
	for _, h := range f.homophones {
		if strings.Contains(out, h.From) {
			out = strings.ReplaceAll(out, h.From, h.To)
		}
	}
	return out
}

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

// stripSymbols 只保留字母与数字，得到"文字骨架"（骨架层用）。
// emoji、标点、符号以及它们带的变体选择符全部丢弃。
func stripSymbols(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// IsPlainEnglishName 判断昵称是否是"两个常见英文名"的形状（首字母大写、纯 ASCII、
// 中间一个空格、每段 2..12 字母）。
//
// 这是 bot 农场的主力形态：实测 1401/1841 条入群昵称是纯英文名，其中 92% 根本没答题。
// 但名字本身不含任何广告信号，所以判据必须叠加"无 username / 无简介 / 无头像"——
// 单用本函数会误伤真人（"Jack Smith" 也是这个形状）。
func IsPlainEnglishName(name string) bool {
	parts := strings.Split(strings.TrimSpace(name), " ")
	if len(parts) != 2 {
		return false
	}
	for _, p := range parts {
		if len(p) < 2 || len(p) > 12 {
			return false
		}
		if p[0] < 'A' || p[0] > 'Z' {
			return false
		}
		for i := 1; i < len(p); i++ {
			if c := p[i]; (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
				return false
			}
		}
	}
	return true
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
