// 回放：用生产历史昵称对比"旧词表"与"新词表 + 谐音层"的命中差异。
//
// 目的不是跑测试，而是拿真实数据回答两个问题：
//  1. 新增规则到底多抓到了谁（漏检修复）；
//  2. 有没有本来正常的人被新规则判成广告（误伤）。
//
// 输入是 L7 导出的 names.tsv（state \t display_name），输出差异清单。
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/filter"
)

// prodWords 是 L7 生产配置里的 global_words（2026-11-15 读取）。
var prodWords = []string{
	"代肝", "代充", "代练", "加V", "加微", "加微信", "加QQ", "私聊我", "私我", "pm我",
	"私聊下单", "私聊咨询", "私聊了解", "私聊进群", "有意私聊", "详情私聊", "加我私聊",
	"点击链接", "免费领取", "博彩", "棋牌", "六合彩", "澳门", "赌场", "娱乐城", "菠菜",
	"开云", "亚博", "太阳城", "首存", "彩金", "包赢", "稳赚", "主页有群", "群发", "出租", "接单",
}

// newWords 是本次从审计日志里补的高精度 CTA 词。
var newWords = []string{
	"看我主页", "点我头像", "主页入口", "投稿爆料", "广告投放",
	"全网监控", "免费观看", "卖走私", "带赢", "云海联盟",
}

// buildCfg 组装词表。withDefaults=false 时严格还原"部署前"的生效词表（只有 YAML
// 里的 global_words），用来做基线；true 时按"默认词表 ∪ YAML ∪ 追加"组装新状态——
// 这才是现在生产实际生效的并集。
func buildCfg(withDefaults bool, words []string) config.AdFilter {
	cfg := config.Default().AdFilter
	if !withDefaults {
		cfg.GlobalWords = words
		cfg.BioWords = nil
		cfg.Homophones = nil
		return cfg
	}
	merged := append([]string{}, cfg.GlobalWords...)
	merged = append(merged, words...)
	cfg.GlobalWords = merged
	return cfg
}

func main() {
	oldF := filter.New(buildCfg(false, prodWords))
	newCfg := buildCfg(true, append(append([]string{}, prodWords...), newWords...))
	newF := filter.New(newCfg)

	f, err := os.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)

	var total, oldHit, newHit, onlyNew, lost int
	type row struct{ state, name, word string }
	var added []row
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), "\t", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[1]) == "" {
			continue
		}
		state, name := parts[0], parts[1]
		total++
		ow, oh := oldF.Check(name, nil)
		nw, nh := newF.Check(name, nil)
		if oh {
			oldHit++
		}
		if nh {
			newHit++
		}
		switch {
		case !oh && nh:
			onlyNew++
			added = append(added, row{state, name, nw})
		case oh && !nh:
			lost++
			_ = ow
		}
	}
	fmt.Printf("样本 %d 条：旧词表命中 %d，新规则命中 %d，新增捕获 %d，减少 %d\n\n",
		total, oldHit, newHit, onlyNew, lost)
	fmt.Println("=== 新增捕获（旧规则漏掉的） ===")
	for _, r := range added {
		fmt.Printf("  [%s] %-40s ← %s\n", r.state, r.name, r.word)
	}
}
