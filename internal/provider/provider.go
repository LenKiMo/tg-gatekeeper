// Package provider 实现四种数据集来源，并统一产出 domain.Snapshot。
//
// 新增一种来源只需实现 ports.Source 并在 Registry 里登记；核心代码不需要任何改动。
package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/challenge"
	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
)

// Factory 按配置构造数据源。
type Factory func(name string, def config.ProviderDef) (ports.Source, error)

var factories = map[string]Factory{}

// Register 登记一种 provider 类型。
func Register(kind string, f Factory) {
	factories[strings.ToLower(strings.TrimSpace(kind))] = f
}

func init() {
	Register("jsonfile", func(name string, def config.ProviderDef) (ports.Source, error) {
		return newJSONFile(name, def)
	})
	Register("directory", func(name string, def config.ProviderDef) (ports.Source, error) {
		return newDirectory(name, def)
	})
	Register("sql", func(name string, def config.ProviderDef) (ports.Source, error) {
		return newSQL(name, def)
	})
	Register("http", func(name string, def config.ProviderDef) (ports.Source, error) {
		return newHTTP(name, def)
	})
}

// New 按配置构造当前激活的数据源。
func New(cfg *config.Config) (ports.Source, error) {
	def := cfg.ActiveProvider()
	kind := strings.ToLower(strings.TrimSpace(def.Type))
	f, ok := factories[kind]
	if !ok {
		return nil, fmt.Errorf("未知的 provider 类型 %q（可选：jsonfile/directory/sql/http）", def.Type)
	}
	return f(cfg.Provider.Active, def)
}

// ---------------------------------------------------------------- 记录解析

// record 是数据集里的一条原始记录（标准字段 + 常见别名兜底）。
type record struct {
	ID         string   `json:"id"`
	Label      string   `json:"label"`
	Image      string   `json:"image"`
	Pool       string   `json:"pool"`
	Groups     []string `json:"groups"`
	Difficulty int      `json:"difficulty"`
	Weight     float64  `json:"weight"`
	Enabled    *bool    `json:"enabled"`
	Alias      []string `json:"alias"`

	// 常见别名：让"任意数据集"更容易接进来，而不必先做格式转换。
	Name         string   `json:"name"`
	CnName       string   `json:"cnname"`
	ImageURL     string   `json:"illustration"`
	Cover        string   `json:"cover"`
	URL          string   `json:"url"`
	Aliases      []string `json:"aliases"`
	OtherName    string   `json:"othername"`
	DifficultySp string   `json:"diff"`
}

func (r record) toEntry(defaultPool string) (domain.Entry, bool) {
	label := firstNonEmpty(r.Label, r.CnName, r.Name)
	image := firstNonEmpty(r.Image, r.ImageURL, r.Cover, r.URL)
	if label == "" || image == "" {
		return domain.Entry{}, false
	}
	pool := strings.TrimSpace(r.Pool)
	if pool == "" {
		pool = defaultPool
	}
	alias := append(append([]string{}, r.Alias...), r.Aliases...)
	if r.OtherName != "" {
		alias = append(alias, r.OtherName)
	}
	enabled := true
	if r.Enabled != nil {
		enabled = *r.Enabled
	}
	id := strings.TrimSpace(r.ID)
	diff := r.Difficulty
	if diff == 0 && r.DifficultySp != "" {
		if v, err := strconv.Atoi(strings.TrimSpace(r.DifficultySp)); err == nil {
			diff = v
		}
	}
	return domain.Entry{
		ID:         id,
		Label:      strings.TrimSpace(label),
		ImageRef:   strings.TrimSpace(image),
		Pool:       pool,
		Groups:     r.Groups,
		Difficulty: diff,
		Weight:     r.Weight,
		Enabled:    enabled,
		Alias:      alias,
	}, true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// decodeRecords 把 JSON 数组 / 单个对象 / JSONL 解析为记录列表。
func decodeRecords(raw []byte, format string) ([]record, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("数据集为空")
	}
	useJSONL := strings.EqualFold(format, "jsonl")
	if strings.EqualFold(format, "auto") || format == "" {
		useJSONL = trimmed[0] != '[' && trimmed[0] != '{'
	}
	if useJSONL {
		var out []record
		for i, line := range strings.Split(trimmed, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			var rec record
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				return nil, fmt.Errorf("第 %d 行不是合法 JSON 对象: %w", i+1, err)
			}
			out = append(out, rec)
		}
		return out, nil
	}
	if trimmed[0] == '{' {
		// 允许 {"items": [...]} / {"data": [...]} 包装。
		var wrapper struct {
			Items json.RawMessage `json:"items"`
			Data  json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(trimmed), &wrapper); err == nil {
			payload := wrapper.Items
			if len(payload) == 0 {
				payload = wrapper.Data
			}
			if len(payload) > 0 {
				var nested []record
				if err := json.Unmarshal(payload, &nested); err == nil {
					return nested, nil
				}
			}
		}
		var single record
		if err := json.Unmarshal([]byte(trimmed), &single); err != nil {
			return nil, fmt.Errorf("解析数据集失败: %w", err)
		}
		return []record{single}, nil
	}
	var out []record
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil, fmt.Errorf("解析数据集失败: %w", err)
	}
	return out, nil
}

// buildSnapshot 把记录整理成快照：补 ID、统计丢弃、计算 revision。
func buildSnapshot(sourceID string, recs []record, defaultPool string) domain.Snapshot {
	entries := make([]domain.Entry, 0, len(recs))
	dropped := 0
	seen := map[string]bool{}
	for _, rec := range recs {
		e, ok := rec.toEntry(defaultPool)
		if !ok {
			dropped++
			continue
		}
		if e.ID == "" {
			e.ID = challenge.EntryID(e.Pool, e.Label)
		}
		if seen[e.ID] {
			dropped++
			continue
		}
		seen[e.ID] = true
		entries = append(entries, e)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Pool != entries[j].Pool {
			return entries[i].Pool < entries[j].Pool
		}
		return entries[i].Label < entries[j].Label
	})
	return domain.Snapshot{
		SourceID: sourceID,
		Revision: revisionOf(entries),
		LoadedAt: time.Now(),
		Entries:  entries,
		Dropped:  dropped,
	}
}

func revisionOf(entries []domain.Entry) string {
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		keys = append(keys, e.Pool+"\x00"+e.Label+"\x00"+e.ImageRef)
	}
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return hex.EncodeToString(sum[:])[:16]
}

// snapshotCtx 只是把 context 传递得更显式一点，便于将来给 provider 加超时。
func snapshotCtx(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
