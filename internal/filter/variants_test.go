package filter

// 谐音/骨架/简介词表与"英文名形状"的回归测试。
//
// 用例里的昵称全部来自生产审计日志（2026-10 ~ 2026-11 的入群记录），包括：
//   - 命中了谐音但漏过旧词表的："看我主椰""山水看沃竹页""看莪曰 挣.5k"
//   - 不该被拦的正常昵称："秦琴音""沼泽惊魂""菜 心""Jack Smith"
// 断言"能拦下"的同时必须配"不误伤"的对照组：只断言前者等于把判定函数改成恒真是也能过。

import (
	"testing"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
)

// filterWith 构造一个只改指定字段的过滤器（其余走默认配置）。
func filterWith(mutate func(*config.AdFilter)) *AdFilter {
	cfg := config.Default().AdFilter
	mutate(&cfg)
	return New(cfg)
}

// TestHomophoneRestoresSourceForms 谐音层：实测到的谐音/拆字写法要能还原后命中。
func TestHomophoneRestoresSourceForms(t *testing.T) {
	f := filterWith(func(c *config.AdFilter) {
		c.GlobalWords = []string{"看我主页"}
		c.BioWords = []string{"日挣"}
	})
	cases := []struct {
		text string
		bio  bool
		want string
		why  string
	}{
		{"看我主椰", false, "看我主页", "主椰→主页"},
		{"山水看沃竹页", false, "看我主页", "沃→我 + 竹页→主页"},
		{"看莪曰 挣.5k", true, "日挣", "莪→我、曰→日 后构成\"日挣\""},
	}
	for _, c := range cases {
		got, hit := func() (string, bool) {
			if c.bio {
				return f.CheckBio(c.text, nil)
			}
			return f.Check(c.text, nil)
		}()
		if !hit || got != c.want {
			t.Errorf("%q 应命中 %q（%s），实际 hit=%v word=%q", c.text, c.want, c.why, hit, got)
		}
	}

	// 对照组：正常昵称不能被谐音层拖下水。
	for _, name := range []string{"秦琴音", "沼泽惊魂", "菜 心", "Jack Smith", "Malia Schwartz", "三年如梦 苏"} {
		if w, hit := f.Check(name, nil); hit {
			t.Errorf("正常昵称 %q 被误判为广告（命中 %q）", name, w)
		}
		if w, hit := f.CheckBio(name, nil); hit {
			t.Errorf("正常昵称 %q 在简介层被误判（命中 %q）", name, w)
		}
	}
}

// TestHomophoneIsIdempotentAndOrdered 长规则优先：先还原"沃竹页"再轮到"沃"，
// 否则"沃"先把字换掉，"沃竹页"这条规则永远匹配不上。
func TestHomophoneIsIdempotentAndOrdered(t *testing.T) {
	f := filterWith(func(c *config.AdFilter) {
		c.GlobalWords = []string{"我主页"}
		c.Homophones = []config.Homophone{
			{From: "沃", To: "我"}, // 故意把短的写在前面
			{From: "竹页", To: "主页"},
		}
	})
	if _, hit := f.Check("看沃竹页", nil); !hit {
		t.Fatal("长规则没有优先：\"看沃竹页\" 应还原成 \"看我主页\" 并命中")
	}
}

// TestSkeletonLayerIsIsolated 骨架层只认 skeleton_words，不能把 global_words 也放进去。
//
// 这条边界很重要：骨架会误伤（"快😘乐营💙业😻中" 的骨架是"快乐营业中"），所以它必须
// 是一张单独维护、默认空的表，而不是"给全局词表开一个更宽松的匹配模式"。
func TestSkeletonLayerIsIsolated(t *testing.T) {
	withSkeleton := filterWith(func(c *config.AdFilter) {
		c.GlobalWords = nil
		c.BioWords = nil
		c.Homophones = nil
		c.SkeletonWords = []string{"博彩"}
	})
	if _, hit := withSkeleton.Check("博😀彩", nil); !hit {
		t.Fatal("骨架层应命中词中被插入 emoji 的\"博😀彩\"")
	}
	if _, hit := withSkeleton.Check("博彩", nil); hit {
		t.Fatal("骨架层不该被原样文本命中（那是 global_words 的职责）")
	}

	noSkeleton := filterWith(func(c *config.AdFilter) {
		c.GlobalWords = []string{"博彩"}
		c.BioWords = nil
		c.Homophones = nil
		c.SkeletonWords = nil
	})
	if _, hit := noSkeleton.Check("博😀彩", nil); hit {
		t.Fatal("未配骨架词表时不应命中")
	}
	if _, hit := noSkeleton.Check("博😀彩博彩", nil); !hit {
		t.Fatal("emoji 夹在词**两侧**时子串匹配本来就能命中，不该依赖骨架层")
	}
}

// TestBioWordsOnlyApplyToBio 简介专用词只在 CheckBio 生效。
func TestBioWordsOnlyApplyToBio(t *testing.T) {
	f := filterWith(func(c *config.AdFilter) {
		c.GlobalWords = []string{"代肝"}
		c.BioWords = []string{"日挣", "USDT"}
		c.Homophones = nil
	})
	const bio = "虚拟货币搬砖日挣1千U 招团队合作"
	if _, hit := f.Check(bio, nil); hit {
		t.Fatal("昵称层不该命中简介专用词（日挣 在昵称里可能是玩笑）")
	}
	if w, hit := f.CheckBio(bio, nil); !hit || w != "日挣" {
		t.Fatalf("简介层应命中 日挣，实际 hit=%v word=%q", hit, w)
	}
	// 全局词在两层都要生效。
	if _, hit := f.CheckBio("业余代肝", nil); !hit {
		t.Fatal("全局词在简介层也必须生效")
	}
}

// TestNegationStillWorksWithHomophone 免责声明豁免不能被谐音层绕过。
func TestNegationStillWorksWithHomophone(t *testing.T) {
	f := filterWith(func(c *config.AdFilter) {
		c.GlobalWords = []string{"私聊我"}
		c.Homophones = nil
	})
	if _, hit := f.Check("请勿私聊我", nil); hit {
		t.Fatal("紧邻否定标记时应豁免")
	}
	if _, hit := f.Check("谢绝砍价，私聊我下单", nil); !hit {
		t.Fatal("中间隔了别的字仍应判广告")
	}
}

// TestBioJoinLinkReject 简介里的私密邀请链接（t.me/+hash）判广告，公开频道链接不判。
func TestBioJoinLinkReject(t *testing.T) {
	on := filterWith(func(c *config.AdFilter) {
		c.GlobalWords = nil
		c.BioWords = nil
		c.Homophones = nil
		c.BioJoinLinkReject = true
	})
	cases := []struct {
		bio  string
		want bool
	}{
		{"https://t.me/+VUo9tejy8MY5ZWM1 进群了解一小时8000", true},
		{"手头紧，缺钱的兄弟🌈来，内部项目👇 https://t.me/+wxEkn1m7D_w4YTQ0", true},
		{"t.me/+abc", true},
		{"t.me/mychannel", false}, // 公开频道
		{"https://t.me/ArknightsEndfieldCN", false},
		{"热爱方舟，日常在群里摸鱼", false},
	}
	for _, c := range cases {
		if _, hit := on.CheckBio(c.bio, nil); hit != c.want {
			t.Errorf("CheckBio(%q) hit=%v，期望 %v", c.bio, hit, c.want)
		}
	}
	// 关掉时不判（默认保守）。
	off := filterWith(func(c *config.AdFilter) {
		c.GlobalWords = nil
		c.BioWords = nil
		c.Homophones = nil
		c.BioJoinLinkReject = false
	})
	if _, hit := off.CheckBio("t.me/+abc", nil); hit {
		t.Error("bio_join_link_reject=false 时不该命中")
	}
	// 只在简介层生效，昵称层不受影响。
	if _, hit := on.Check("t.me/+abc", nil); hit {
		t.Error("邀请链接判据不该作用于昵称层")
	}
}

// TestBioWordsRejectFromRealBios 生产实测的真实 bio：该拦的拦下，正常简介不误伤。
func TestBioWordsRejectFromRealBios(t *testing.T) {
	f := filterWith(func(c *config.AdFilter) {
		c.BioJoinLinkReject = true // 生产已开启
	})
	spam := []string{
		"律师函 催收函 长期招实力快递车队价格高 结账效率。",
		"https://t.me/+VUo9tejy8MY5ZWM1 进群了解一小时8000",
		"手头紧，缺钱的兄弟🌈来，内部项目，一天5万随便拿 https://t.me/+wxEkn1m7D_w4YTQ0",
	}
	for _, b := range spam {
		if w, hit := f.CheckBio(b, nil); !hit {
			t.Errorf("真实 spam 简介未被命中：%q", b)
		} else {
			t.Logf("命中 %-12q ← %q", b[:12], w)
		}
	}
	normal := []string{
		"热爱方舟，日常在群里摸鱼",
		"玩游戏，勿扰",
		"请勿私聊我，有事群里@",
	}
	for _, b := range normal {
		if w, hit := f.CheckBio(b, nil); hit {
			t.Errorf("正常简介被误判：%q（命中 %q）", b, w)
		}
	}
}

// TestRealSpamNicknamesRejectedBeforeChallenge 四条真实入群昵称（用户直接给出的欢迎语原文）
// 必须在**昵称层**就命中，否则会先给它们发题目再等 60 秒超时。
//
// 其中第四条是重点：`𝐮𝘀𝐝𝐭` 用的是数学字母（U+1D624 / U+1D602…），只有 NFKC 折叠后
// 才会变成 "usdt"——关掉 normalize_nfkc 就漏。
func TestRealSpamNicknamesRejectedBeforeChallenge(t *testing.T) {
	f := filterWith(func(c *config.AdFilter) {}) // 纯默认词表（不含群级 ad_words）
	names := []string{
		"催收直盘 长期招快递车队合作",
		"重·生之*卖走私*苹果18*手机",
		"云海联盟|广告投放|投稿爆料|入口点头像 Ryltqngpuu",
		"黑u🔥𝐮𝘀𝐝𝐭項目🔨高仿U置换🎉高仿U置换 🎁最新获取U🎉技术U🎉虚拟币黑U🎁标记U項目",
	}
	for _, n := range names {
		w, hit := f.Check(n, nil)
		if !hit {
			t.Errorf("昵称应直接判广告（不出题）：%q", n)
		} else {
			t.Logf("命中 %-10q ← 词 %q", n, w)
		}
	}
	// 对照组：正常昵称不能被这 12 个新词误伤。
	for _, n := range []string{"秦琴音", "沼泽惊魂", "菜 心", "三年如梦 苏", "Jack Smith", "回血小王子", "手机跑分党"} {
		if w, hit := f.Check(n, nil); hit {
			t.Errorf("正常昵称被误判：%q（命中 %q）", n, w)
		}
	}
}

// TestIsPlainEnglishName 英文名形状判定（只判形状，不判 bot——叠加资料才是完整判据）。
func TestIsPlainEnglishName(t *testing.T) {
	yes := []string{
		"Malia Schwartz", "Adalyn James", "Katelin Elll", "Jsheg Syej",
		"Hope Chalie", "Jack Smith", "Yi Zuni", "Ella Rowe",
	}
	for _, n := range yes {
		if !IsPlainEnglishName(n) {
			t.Errorf("%q 应判为两词英文名", n)
		}
	}
	no := []string{
		"", "Malia", "Malia Schwartz Jr", "Malia  Schwartz", "Malia Schwartz1",
		"秦琴音", "菜 心", "Malia Schwartz🐂", "-100%", "🐂", "李 小龙",
		"Malia-Schwartz", "malia schwartz", // 小写/连字符都不算"首字母大写的名字"
	}
	for _, n := range no {
		if IsPlainEnglishName(n) {
			t.Errorf("%q 不该判为两词英文名", n)
		}
	}
}
