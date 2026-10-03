package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/LenKiMo/tg-gatekeeper/internal/challenge"
	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
)

// directoryProvider 把一个图片目录当成数据集：文件名（去扩展名）就是标签。
//
// 这是零门槛的落地方案：把"准确标签.jpg"丢进目录即可部署。若提供 labels_file，
// 则强制"清单与文件一一对应"——多一个、少一个、名字不一致都直接报错，避免
// 静默地把错标签挂到图上。
type directoryProvider struct {
	name            string
	root            string
	labelsFile      string
	labelsFormat    string
	filenameIsLabel bool
	pool            string
	recursive       bool
	extensions      []string
}

func newDirectory(name string, def config.ProviderDef) (ports.Source, error) {
	d := def.Directory
	exts := d.Extensions
	if len(exts) == 0 {
		exts = []string{".jpg", ".jpeg", ".png", ".webp"}
	}
	for i := range exts {
		exts[i] = strings.ToLower(strings.TrimSpace(exts[i]))
	}
	format := strings.ToLower(d.LabelsFormat)
	if format == "" {
		format = "lines"
	}
	return &directoryProvider{
		name:            name,
		root:            d.Root,
		labelsFile:      d.LabelsFile,
		labelsFormat:    format,
		filenameIsLabel: d.FilenameIsLabel,
		pool:            d.Pool,
		recursive:       d.Recursive,
		extensions:      exts,
	}, nil
}

func (p *directoryProvider) ID() string { return "directory:" + p.root }

func (p *directoryProvider) Snapshot(ctx context.Context) (domain.Snapshot, error) {
	ctx = snapshotCtx(ctx)
	root, err := filepath.Abs(p.root)
	if err != nil {
		return domain.Snapshot{}, fmt.Errorf("解析图片目录失败: %w", err)
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return domain.Snapshot{}, fmt.Errorf("图片目录 %s 不存在或不是目录", root)
	}

	overrides, err := p.loadLabelOverrides()
	if err != nil {
		return domain.Snapshot{}, err
	}

	var recs []record
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if path != root && !p.recursive {
				return fs.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if !containsString(p.extensions, ext) {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = d.Name()
		}
		stem := strings.TrimSuffix(d.Name(), filepath.Ext(d.Name()))
		label := ""
		switch {
		case overrides != nil:
			// 清单模式：文件名必须出现在清单里，否则报错（不做静默兜底）。
			v, ok := overrides[rel]
			if !ok {
				v, ok = overrides[filepath.ToSlash(rel)]
			}
			if !ok {
				v, ok = overrides[d.Name()]
			}
			if !ok {
				v, ok = overrides[stem]
			}
			if !ok {
				return fmt.Errorf("图片 %s 不在标签清单里（清单模式下必须一一对应）", filepath.ToSlash(rel))
			}
			label = strings.TrimSpace(v)
		default:
			label = strings.TrimSpace(stem)
		}
		if label == "" {
			return fmt.Errorf("图片 %s 解析不出标签（文件名或清单为空）", filepath.ToSlash(rel))
		}
		enabled := true
		recs = append(recs, record{
			ID:      challenge.EntryID(p.pool, label),
			Label:   label,
			Image:   "file:" + path,
			Pool:    p.pool,
			Enabled: &enabled,
		})
		return nil
	})
	if err != nil {
		return domain.Snapshot{}, err
	}
	if len(recs) == 0 {
		return domain.Snapshot{}, fmt.Errorf("图片目录 %s 里没有找到允许的图片（扩展名 %s）", root, strings.Join(p.extensions, "/"))
	}
	if overrides != nil {
		if err := p.verifyOneToOne(root, recs, overrides); err != nil {
			return domain.Snapshot{}, err
		}
	}
	snap := buildSnapshot(p.ID(), recs, p.pool)
	if snap.Dropped > 0 {
		return domain.Snapshot{}, fmt.Errorf("图片目录 %s 解析出 %d 条重复/无效条目（同名标签或同名文件冲突）", root, snap.Dropped)
	}
	return snap, nil
}

func (p *directoryProvider) Close() error { return nil }

// loadLabelOverrides 读取标签映射：lines 为"一行一个标签"，json 为 {"文件名": "标签"}。
func (p *directoryProvider) loadLabelOverrides() (map[string]string, error) {
	if strings.TrimSpace(p.labelsFile) == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(p.labelsFile)
	if err != nil {
		return nil, fmt.Errorf("读取标签清单 %s 失败: %w", p.labelsFile, err)
	}
	switch p.labelsFormat {
	case "json":
		out := map[string]string{}
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("解析标签清单 %s 失败（应为 {\"文件名\": \"标签\"}）: %w", p.labelsFile, err)
		}
		return out, nil
	default:
		out := map[string]string{}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			// "文件名<TAB>标签" 或 "标签"（此时文件名=标签）。
			if idx := strings.IndexAny(line, "\t"); idx > 0 {
				out[strings.TrimSpace(line[:idx])] = strings.TrimSpace(line[idx+1:])
				continue
			}
			out[line] = line
		}
		return out, nil
	}
}

// verifyOneToOne 校验清单与图片一一对应（多/少都报错）。
func (p *directoryProvider) verifyOneToOne(root string, recs []record, overrides map[string]string) error {
	seen := map[string]bool{}
	for _, r := range recs {
		ref := strings.TrimPrefix(r.Image, "file:")
		if rel, err := filepath.Rel(root, ref); err == nil {
			seen[rel] = true
			seen[filepath.ToSlash(rel)] = true
			seen[filepath.Base(ref)] = true
			seen[strings.TrimSuffix(filepath.Base(ref), filepath.Ext(ref))] = true
		}
	}
	var missing []string
	for key := range overrides {
		if !seen[key] {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("标签清单里有 %d 项在图片目录中找不到对应图片：%s", len(missing), strings.Join(missing, ", "))
	}
	return nil
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
