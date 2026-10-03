package dataset

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// FromRawJSON 把"尽量宽容"的原始 JSON 转成标准数据集。
//
// 支持根结构：[{...}]、{"items":[...]}、{"data":[...]}，以及每条的常见字段别名：
// label/name/cnname/title → label；image/illustration/cover/url → image；
// pool/class/faction → pool 与 groups。这样使用者不必先手写转换脚本。
func FromRawJSON(raw []byte, poolOverride string) (*File, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("输入为空")
	}
	var recs []map[string]any
	switch trimmed[0] {
	case '[':
		if err := json.Unmarshal([]byte(trimmed), &recs); err != nil {
			return nil, fmt.Errorf("解析数组失败: %w", err)
		}
	case '{':
		var wrapper struct {
			Items json.RawMessage `json:"items"`
			Data  json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(trimmed), &wrapper); err == nil && (len(wrapper.Items) > 0 || len(wrapper.Data) > 0) {
			payload := wrapper.Items
			if len(payload) == 0 {
				payload = wrapper.Data
			}
			if err := json.Unmarshal(payload, &recs); err != nil {
				return nil, fmt.Errorf("解析 items 失败: %w", err)
			}
		} else {
			var single map[string]any
			if err := json.Unmarshal([]byte(trimmed), &single); err != nil {
				return nil, fmt.Errorf("解析对象失败: %w", err)
			}
			recs = []map[string]any{single}
		}
	default:
		// JSONL
		for i, line := range strings.Split(trimmed, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				return nil, fmt.Errorf("第 %d 行不是合法 JSON 对象: %w", i+1, err)
			}
			recs = append(recs, rec)
		}
	}

	f := &File{Generated: time.Now().Format(time.RFC3339)}
	for _, rec := range recs {
		label := pickString(rec, "label", "name", "cnname", "tcname", "title", "text")
		image := pickString(rec, "image", "illustration", "cover", "url", "img", "picture")
		if label == "" || image == "" {
			continue
		}
		entry := Entry{
			ID:    pickString(rec, "id", "code", "filename", "itemId"),
			Label: label,
			Image: image,
			Pool:  pickString(rec, "pool"),
		}
		if poolOverride != "" {
			entry.Pool = poolOverride
		} else if entry.Pool == "" {
			entry.Pool = pickString(rec, "category", "type")
		}
		for _, key := range []string{"class", "faction", "type", "profession"} {
			if v := pickString(rec, key); v != "" {
				entry.Groups = append(entry.Groups, v)
			}
		}
		if v := pickString(rec, "rarity"); v != "" {
			for _, r := range v {
				if r >= '0' && r <= '9' {
					entry.Difficulty = rarityToDifficulty(int(r - '0'))
					break
				}
			}
		}
		f.Items = append(f.Items, entry)
	}
	if len(f.Items) == 0 {
		return nil, fmt.Errorf("没有解析出任何可用条目（需要 label/name 与 image/illustration 字段）")
	}
	return f, nil
}

func pickString(rec map[string]any, keys ...string) string {
	for _, k := range keys {
		v, ok := rec[k]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case string:
			if s := strings.TrimSpace(t); s != "" {
				return s
			}
		case float64:
			return strconv.FormatFloat(t, 'f', -1, 64)
		case bool:
			if t {
				return "true"
			}
		}
	}
	return ""
}
