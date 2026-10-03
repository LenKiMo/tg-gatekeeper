package challenge

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strings"

	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
)

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ErrShortage 表示数据集不足以组成满足要求的题目。
var ErrShortage = errors.New("可用标签不足")

// Options 是出题参数。
type Options struct {
	// OptionCount 期望选项数（含正确答案）。
	OptionCount int
	// MinOptionCount 降级时可接受的最小选项数。
	MinOptionCount int
	// ShortagePolicy：deny 表示不足即失败；reduce 表示允许降级到 MinOptionCount。
	ShortagePolicy string
	// Strategy：uniform | same_group | difficulty_band。
	Strategy string
	// DifficultyBand 供 difficulty_band 策略使用。
	DifficultyBand int
}

// Generated 是一道已生成的题。
type Generated struct {
	Entry            domain.Entry
	Options          []domain.Option
	CorrectTokenHash []byte
	CorrectLabel     string
	Revision         string
	Degraded         bool
	// BucketSize 是正确标签在当前数据集里拥有的图片数量（审计用）。
	BucketSize int
}

// CorrectOption 返回正确答案的选项。
func (g *Generated) CorrectOption() (domain.Option, bool) {
	for _, o := range g.Options {
		if domain.MatchesHash(o.Token, g.CorrectTokenHash) {
			return o, true
		}
	}
	return domain.Option{}, false
}

// ValidToken 判断 token 是否是本题的合法选项。
func (g *Generated) ValidToken(token string) bool {
	_, ok := g.findOption(token)
	return ok
}

func (g *Generated) findOption(token string) (domain.Option, bool) {
	for _, o := range g.Options {
		if o.Token == token {
			return o, true
		}
	}
	return domain.Option{}, false
}

// IsCorrect 判断 token 是否为正确答案。
func (g *Generated) IsCorrect(token string) bool {
	return domain.MatchesHash(token, g.CorrectTokenHash)
}

// bucket 是"同一 pool 下同一归一化标签"的条目集合（可能有多张图）。
type bucket struct {
	pool    string
	key     string
	label   string
	entries []domain.Entry
	groups  map[string]bool
	maxDiff int
	weight  float64
}

// Generator 从一份不可变快照出题。
type Generator struct {
	snapshot domain.Snapshot
	buckets  []bucket
	byPool   map[string][]int
	// aliasExclude 是全部条目别名的归一化集合：任何写在这里的字符串都不能当选项，
	// 否则用户选它其实也是对的（例如"甲"与它的别名"贾"同时出现在选项里）。
	aliasExclude map[string]bool
	// rand 允许测试注入确定性随机源。
	rand io.Reader
}

// New 构造出题器。snapshot 里缺 ID/标签/图片的条目不参与出题（计入 Dropped）。
func New(snapshot domain.Snapshot) *Generator {
	g := &Generator{
		snapshot:     snapshot,
		byPool:       map[string][]int{},
		aliasExclude: map[string]bool{},
		rand:         rand.Reader,
	}
	index := map[string]int{} // pool\x00label -> bucket idx
	seenID := map[string]bool{}
	for _, e := range snapshot.Entries {
		if !e.Enabled {
			g.snapshot.Dropped++
			continue
		}
		label := strings.TrimSpace(e.Label)
		image := strings.TrimSpace(e.ImageRef)
		if label == "" || image == "" {
			g.snapshot.Dropped++
			continue
		}
		entry := e
		entry.Label = label
		entry.ImageRef = image
		pool := strings.TrimSpace(entry.Pool)
		if pool == "" {
			pool = "default"
			entry.Pool = pool
		}
		if entry.ID == "" {
			entry.ID = EntryID(pool, label)
		}
		for _, a := range entry.Alias {
			if k := NormalizeLabel(a); k != "" {
				g.aliasExclude[k] = true
			}
		}
		if seenID[entry.ID] {
			// 重复 ID：保留第一个，其余丢弃（避免审计串号）。
			g.snapshot.Dropped++
			continue
		}
		seenID[entry.ID] = true

		k := LabelKey(pool, label)
		idx, ok := index[k]
		if !ok {
			idx = len(g.buckets)
			index[k] = idx
			g.buckets = append(g.buckets, bucket{pool: pool, key: k, label: label, groups: map[string]bool{}})
			g.byPool[pool] = append(g.byPool[pool], idx)
		}
		b := &g.buckets[idx]
		b.entries = append(b.entries, entry)
		for _, grp := range entry.Groups {
			if grp = strings.TrimSpace(grp); grp != "" {
				b.groups[grp] = true
			}
		}
		if entry.Difficulty > b.maxDiff {
			b.maxDiff = entry.Difficulty
		}
		// 同一标签下取"最不被压低"的权重，避免一票降权就把整个标签雪藏。
		w := entry.Weight
		if w <= 0 {
			w = 1
		}
		if w > b.weight {
			b.weight = w
		}
	}
	return g
}

// Revision 返回快照的内容指纹。
func (g *Generator) Revision() string {
	if g.snapshot.Revision != "" {
		return g.snapshot.Revision
	}
	keys := make([]string, 0, len(g.buckets))
	for _, b := range g.buckets {
		keys = append(keys, b.key)
	}
	sort.Strings(keys)
	return hashHex([]byte(strings.Join(keys, "\n")))[:16]
}

// PoolSizes 返回每个 pool 下可用标签数（健康检查用）。
func (g *Generator) PoolSizes() map[string]int {
	out := make(map[string]int, len(g.byPool))
	for pool, idxs := range g.byPool {
		out[pool] = len(idxs)
	}
	return out
}

// MaxPoolSize 返回最大的 pool 标签数。
func (g *Generator) MaxPoolSize() int {
	max := 0
	for _, n := range g.PoolSizes() {
		if n > max {
			max = n
		}
	}
	return max
}

// Buckets 返回可用标签桶数量。
func (g *Generator) Buckets() int { return len(g.buckets) }

// HealthCheck 判断数据集能否支撑配置的选项数与图片引用格式。
func (g *Generator) HealthCheck(need int) error {
	if g.Buckets() == 0 {
		return errors.New("数据集中没有任何可用条目（需要同时具备标签与图片）")
	}
	if max := g.MaxPoolSize(); max < need {
		return fmt.Errorf("数据集最大的选项池只有 %d 个不同标签，不足以生成 %d 个选项", max, need)
	}
	return nil
}

// Next 生成一道题。
func (g *Generator) Next(opt Options) (*Generated, error) {
	want := opt.OptionCount
	if want < 2 {
		want = 2
	}
	minCount := opt.MinOptionCount
	if minCount < 2 {
		minCount = 2
	}
	if minCount > want {
		minCount = want
	}
	if len(g.buckets) == 0 {
		return nil, fmt.Errorf("%w: 数据集为空", ErrShortage)
	}

	// 1) 选 pool：按"该 pool 的标签数够不够 OptionCount"优先。若没有任何 pool 够，
	//    再按 shortage policy 决定降级还是失败。
	pools := g.eligiblePools(want)
	degraded := false
	if len(pools) == 0 {
		if opt.ShortagePolicy != "reduce" {
			return nil, fmt.Errorf("%w: 没有任何选项池能提供 %d 个不同标签", ErrShortage, want)
		}
		pools = g.eligiblePools(minCount)
		if len(pools) == 0 {
			return nil, fmt.Errorf("%w: 没有任何选项池能提供 %d 个不同标签", ErrShortage, minCount)
		}
		degraded = true
	}

	pool := pools[g.intn(len(pools))]
	idxs := g.byPool[pool]
	count := want
	if degraded {
		count = minCount
		if len(idxs) < count {
			count = len(idxs)
		}
	}

	// 2) 选正确标签桶（等概率，与图片数量无关——避免"图多的标签更容易成为答案"）。
	correctIdx := idxs[g.intn(len(idxs))]
	correct := &g.buckets[correctIdx]

	// 3) 选该标签下的一张图（桶内等概率）。
	entry := correct.entries[g.intn(len(correct.entries))]

	// 4) 选干扰标签：同 pool、不同标签桶，按策略排序后取前若干，再随机抽样。
	distractorCount := count - 1
	candidates := g.rankDistractors(pool, correctIdx, opt)
	if len(candidates) < distractorCount {
		return nil, fmt.Errorf("%w: 池 %s 里可用于干扰的标签只有 %d 个，需要 %d 个",
			ErrShortage, pool, len(candidates), distractorCount)
	}
	picked := sampleIdx(candidates, distractorCount, g.rand)

	// 5) 生成随机 token、组选项、洗牌。
	options := make([]domain.Option, 0, count)
	correctToken, err := domain.NewOptionToken()
	if err != nil {
		return nil, err
	}
	options = append(options, domain.Option{Token: correctToken, Label: entry.Label})
	usedLabels := map[string]bool{normalizeKey(entry.Label): true}
	for _, idx := range picked {
		tok, err := domain.NewOptionToken()
		if err != nil {
			return nil, err
		}
		label := g.buckets[idx].label
		if usedLabels[normalizeKey(label)] {
			continue
		}
		usedLabels[normalizeKey(label)] = true
		options = append(options, domain.Option{Token: tok, Label: label})
	}
	// 干扰项因归一化冲突被跳过后，选项数可能不足；此时按 shortage policy 决定。
	if len(options) < minCount {
		return nil, fmt.Errorf("%w: 归一化后仅剩 %d 个不重复选项，低于下限 %d", ErrShortage, len(options), minCount)
	}
	if len(options) < want {
		degraded = true
	}
	shuffle(options, g.rand)

	return &Generated{
		Entry:            entry,
		Options:          options,
		CorrectTokenHash: domain.HashToken(correctToken),
		CorrectLabel:     entry.Label,
		Revision:         g.Revision(),
		Degraded:         degraded,
		BucketSize:       len(correct.entries),
	}, nil
}

// eligiblePools 返回标签数 >= need 的 pool（按 pool 名排序，保证可重复）。
func (g *Generator) eligiblePools(need int) []string {
	var out []string
	for pool, idxs := range g.byPool {
		if len(idxs) >= need {
			out = append(out, pool)
		}
	}
	sort.Strings(out)
	return out
}

// rankDistractors 按难度策略返回干扰候选（全部是同一 pool、且与正确项不同标签的桶）。
func (g *Generator) rankDistractors(pool string, correctIdx int, opt Options) []int {
	idxs := g.byPool[pool]
	correct := g.buckets[correctIdx]
	type cand struct {
		idx       int
		shared    int
		diffDelta int
	}
	shuffled := make([]int, 0, len(idxs))
	// 排除：正确项自己；以及"写成了别名"的标签（用户选它其实也是对的）。
	for _, i := range idxs {
		if i == correctIdx {
			continue
		}
		key := NormalizeLabel(g.buckets[i].label)
		if g.aliasExclude[key] {
			continue
		}
		if key == "" {
			continue
		}
		shuffled = append(shuffled, i)
	}
	shuffleInts(shuffled, g.rand)

	switch opt.Strategy {
	case "same_group", "difficulty_band":
		cands := make([]cand, 0, len(shuffled))
		for _, i := range shuffled {
			b := g.buckets[i]
			shared := 0
			for grp := range b.groups {
				if correct.groups[grp] {
					shared++
				}
			}
			delta := b.maxDiff - correct.maxDiff
			if delta < 0 {
				delta = -delta
			}
			cands = append(cands, cand{idx: i, shared: shared, diffDelta: delta})
		}
		sort.SliceStable(cands, func(i, j int) bool {
			if opt.Strategy == "same_group" {
				return cands[i].shared > cands[j].shared
			}
			return cands[i].diffDelta < cands[j].diffDelta
		})
		out := make([]int, 0, len(cands))
		for _, c := range cands {
			out = append(out, c.idx)
		}
		return out
	default: // uniform
		return shuffled
	}
}

func normalizeKey(s string) string { return NormalizeLabel(s) }

// sampleIdx 无放回随机取 n 个（部分洗牌）。
func sampleIdx(src []int, n int, r io.Reader) []int {
	if n <= 0 {
		return nil
	}
	if n > len(src) {
		n = len(src)
	}
	cp := make([]int, len(src))
	copy(cp, src)
	for i := 0; i < n; i++ {
		j := i + intn(r, len(cp)-i)
		cp[i], cp[j] = cp[j], cp[i]
	}
	return cp[:n]
}

func shuffle[T any](s []T, r io.Reader) {
	for i := len(s) - 1; i > 0; i-- {
		j := intn(r, i+1)
		s[i], s[j] = s[j], s[i]
	}
}

func shuffleInts(s []int, r io.Reader) {
	for i := len(s) - 1; i > 0; i-- {
		j := intn(r, i+1)
		s[i], s[j] = s[j], s[i]
	}
}

// intn 返回 [0,n) 的均匀随机数，使用注入的随机源（生产为 crypto/rand）。
func intn(r io.Reader, n int) int {
	if n <= 1 {
		return 0
	}
	v, err := rand.Int(r, big.NewInt(int64(n)))
	if err != nil {
		// crypto/rand 失败是极端异常（系统熵源不可用）；退化为 0 会偏向第一项，
		// 因此这里直接 panic 由上层 recover 捕获并记为基础设施错误。
		panic(fmt.Errorf("随机源不可用: %w", err))
	}
	return int(v.Int64())
}

func (g *Generator) intn(n int) int { return intn(g.rand, n) }

// WithRand 返回一个使用指定随机源的副本（测试用）。
func (g *Generator) WithRand(r io.Reader) *Generator {
	cp := *g
	cp.rand = r
	return &cp
}
