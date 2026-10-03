package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
)

// jsonFileProvider 从本地 JSON/JSONL 文件读取数据集。
type jsonFileProvider struct {
	name    string
	path    string
	format  string
	baseDir string
	pool    string
	watch   bool
	labels  string

	mu       sync.Mutex
	cached   domain.Snapshot
	loadedAt time.Time
	mtime    time.Time
}

func newJSONFile(name string, def config.ProviderDef) (ports.Source, error) {
	d := def.JSONFile
	base := d.BaseDir
	if base == "" {
		base = filepath.Dir(d.Path)
	}
	return &jsonFileProvider{
		name:    name,
		path:    d.Path,
		format:  strings.ToLower(d.Format),
		baseDir: base,
		pool:    d.Pool,
		watch:   d.Watch,
		labels:  d.LabelsFile,
	}, nil
}

func (p *jsonFileProvider) ID() string { return "jsonfile:" + p.path }

func (p *jsonFileProvider) Snapshot(ctx context.Context) (domain.Snapshot, error) {
	ctx = snapshotCtx(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()

	st, err := os.Stat(p.path)
	if err != nil {
		if p.cached.Entries != nil {
			return p.cached, nil
		}
		return domain.Snapshot{}, fmt.Errorf("读取数据集 %s 失败: %w", p.path, err)
	}
	if p.cached.Entries != nil && p.watch && !st.ModTime().After(p.mtime) {
		return p.cached, nil
	}
	raw, err := os.ReadFile(p.path)
	if err != nil {
		if p.cached.Entries != nil {
			return p.cached, nil
		}
		return domain.Snapshot{}, fmt.Errorf("读取数据集 %s 失败: %w", p.path, err)
	}
	recs, err := decodeRecords(raw, p.format)
	if err != nil {
		if p.cached.Entries != nil {
			// 热更新失败：继续使用旧快照（计划 4.1 要求）。
			return p.cached, nil
		}
		return domain.Snapshot{}, fmt.Errorf("解析数据集 %s 失败: %w", p.path, err)
	}
	snap := buildSnapshot(p.ID(), recs, p.pool)
	if err := resolveRelativeImages(&snap, p.baseDir); err != nil {
		return domain.Snapshot{}, err
	}
	if p.labels != "" {
		extra, err := loadLabelsFile(p.labels)
		if err != nil {
			return domain.Snapshot{}, err
		}
		if len(extra) > 0 {
			// 干扰项池以 labels 文件为准（同名池由 pool 字段决定；此处仅补充默认池标签）。
			snap = addPoolLabels(snap, extra)
		}
	}
	if len(snap.Entries) == 0 {
		return domain.Snapshot{}, fmt.Errorf("数据集 %s 里没有可用条目", p.path)
	}
	p.cached = snap
	p.mtime = st.ModTime()
	p.loadedAt = time.Now()
	return snap, nil
}

func (p *jsonFileProvider) Close() error { return nil }

// resolveRelativeImages 把相对路径的图片引用补成绝对 file: 路径。
func resolveRelativeImages(snap *domain.Snapshot, baseDir string) error {
	for i := range snap.Entries {
		ref := snap.Entries[i].ImageRef
		if ref == "" || strings.HasPrefix(ref, "data:") ||
			strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
			continue
		}
		rel := strings.TrimPrefix(ref, "file:")
		joined, err := safeJoin(baseDir, rel)
		if err != nil {
			return err
		}
		abs, err := filepath.Abs(joined)
		if err != nil {
			return fmt.Errorf("解析图片路径 %s 失败: %w", joined, err)
		}
		snap.Entries[i].ImageRef = "file:" + abs
	}
	return nil
}

func addPoolLabels(snap domain.Snapshot, labels []string) domain.Snapshot {
	// 把清单里的标签合并进一个同名的"标签池"条目集合：作为没有图片的纯标签是无效的，
	// 因此这里只用于 directory provider 的一一对应校验；JSON provider 不做隐式补池。
	return snap
}

// loadLabelsFile 读取干扰项池清单（一行一个标签，或 JSON 数组）。
func loadLabelsFile(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取标签清单 %s 失败: %w", path, err)
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil, nil
	}
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

func safeJoin(base, rel string) (string, error) {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) {
		return clean, nil
	}
	full := filepath.Join(base, clean)
	relCheck, err := filepath.Rel(base, full)
	if err != nil || strings.HasPrefix(relCheck, "..") {
		return "", fmt.Errorf("图片路径 %q 越出数据集目录 %s", rel, base)
	}
	return full, nil
}
