// Package dataset 提供数据集导入与健康检查，用于把"任意来源的数据"变成
// tg-gatekeeper 能直接使用的标准格式。
//
// 标准格式（items.json）：
//
//	{"items": [{"id":"…","label":"…","image":"…","pool":"…","groups":["…"],
//	            "difficulty":1,"weight":1,"enabled":true,"alias":["…"]}]}
//
// image 可以是 http(s) URL、本地相对路径（用 base_dir 解析）或 file: 绝对路径。
package dataset

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Entry 是导入输出的一条记录（与标准格式一致）。
type Entry struct {
	ID         string   `json:"id,omitempty"`
	Label      string   `json:"label"`
	Image      string   `json:"image"`
	Pool       string   `json:"pool,omitempty"`
	Groups     []string `json:"groups,omitempty"`
	Difficulty int      `json:"difficulty,omitempty"`
	Weight     float64  `json:"weight,omitempty"`
	Enabled    *bool    `json:"enabled,omitempty"`
	Alias      []string `json:"alias,omitempty"`
}

// File 是 items.json 的结构。
type File struct {
	Source    string   `json:"source,omitempty"`
	Generated string   `json:"generated_at,omitempty"`
	Items     []Entry  `json:"items"`
	Labels    []string `json:"labels,omitempty"`
}

// ---------------------------------------------------------------- MediaWiki 导入

// WikiOptions 是"从 MediaWiki 抓取"的参数。
type WikiOptions struct {
	// API 是 MediaWiki api.php 地址，例如 https://<wiki-host>/api.php
	API string
	// Category 是条目分类，例如 "Category:Items"。
	Category string
	// LabelField 是信息框里作为显示标签的字段，例如 cnname（中文名）。
	LabelField string
	// ImageField 是信息框里的图片字段，例如 image。
	ImageField string
	// ID 是信息框里作为稳定 ID 的字段，例如 filename。
	IDField string
	// ThumbWidth > 0 时使用 wiki 缩略图（显著减小体积与上传耗时）。
	ThumbWidth int
	// Pool 写入所有条目的 pool。
	Pool string
	// MinRarity 为 0 时不过滤；>0 时要求 rarity 字段 >= 该值。
	MinRarity int
	// UserAgent 是抓取用的 UA（wiki 要求可识别的 UA）。
	UserAgent string
	// MaxPages 为 0 时抓取分类下全部页面。
	MaxPages int
	// RequireLabel 为 true 时丢弃没有标签的页面（NPC 页面通常没有 cnname）。
	RequireLabel bool
}

// ImportFromWiki 抓取一个 MediaWiki 分类下的条目并生成标准数据集。
func ImportFromWiki(ctx context.Context, opts WikiOptions) (*File, error) {
	if opts.API == "" || opts.Category == "" {
		return nil, fmt.Errorf("wiki 导入需要 api 与 category")
	}
	if opts.LabelField == "" {
		opts.LabelField = "cnname"
	}
	if opts.ImageField == "" {
		opts.ImageField = "image"
	}
	if opts.IDField == "" {
		opts.IDField = "filename"
	}
	c := &wikiClient{api: opts.API, ua: opts.UserAgent, http: &http.Client{Timeout: 60 * time.Second}}
	if c.ua == "" {
		c.ua = "tg-gatekeeper-dataset/1.0 (+https://github.com/LenKiMo/tg-gatekeeper)"
	}

	titles, err := c.categoryMembers(ctx, opts.Category)
	if err != nil {
		return nil, err
	}
	if len(titles) == 0 {
		return nil, fmt.Errorf("分类 %s 下没有页面（注意分类名区分大小写与空格）", opts.Category)
	}
	if opts.MaxPages > 0 && len(titles) > opts.MaxPages {
		titles = titles[:opts.MaxPages]
	}

	type page struct {
		title    string
		label    string
		file     string
		id       string
		rarity   int
		groups   []string
		class    string
		titleURL string
	}
	var pages []page
	const batch = 20
	for i := 0; i < len(titles); i += batch {
		end := i + batch
		if end > len(titles) {
			end = len(titles)
		}
		contents, err := c.pageContents(ctx, titles[i:end])
		if err != nil {
			return nil, err
		}
		for _, t := range titles[i:end] {
			text, ok := contents[t]
			if !ok {
				continue
			}
			label := wikiField(text, opts.LabelField)
			if label == "" && opts.RequireLabel {
				continue
			}
			image := normalizeWikiFile(wikiField(text, opts.ImageField))
			if image == "" {
				continue
			}
			rarity := 0
			if v := wikiField(text, "rarity"); v != "" {
				rarity, _ = strconv.Atoi(strings.TrimSpace(v))
			}
			if opts.MinRarity > 0 && rarity < opts.MinRarity {
				continue
			}
			var groups []string
			if class := wikiField(text, "class"); class != "" {
				groups = append(groups, class)
			}
			if faction := wikiField(text, "faction"); faction != "" {
				groups = append(groups, faction)
			}
			pages = append(pages, page{
				title:  t,
				label:  label,
				file:   image,
				id:     wikiField(text, opts.IDField),
				rarity: rarity,
				groups: groups,
			})
		}
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("没有解析出任何条目（检查 label_field/image_field 是否与信息框字段一致）")
	}

	files := make([]string, 0, len(pages))
	for _, p := range pages {
		files = append(files, p.file)
	}
	urls, err := c.imageURLs(ctx, files, opts.ThumbWidth)
	if err != nil {
		return nil, err
	}

	out := &File{Source: fmt.Sprintf("wiki:%s %s", opts.API, opts.Category), Generated: time.Now().Format(time.RFC3339)}
	sort.Slice(pages, func(i, j int) bool {
		if pages[i].rarity != pages[j].rarity {
			return pages[i].rarity > pages[j].rarity
		}
		return pages[i].title < pages[j].title
	})
	seen := map[string]bool{}
	for _, p := range pages {
		u, ok := urls[normFile(p.file)]
		if !ok {
			continue
		}
		label := p.label
		if label == "" {
			label = p.title
		}
		if seen[label] {
			continue
		}
		seen[label] = true
		e := Entry{
			ID:         p.id,
			Label:      label,
			Image:      u,
			Pool:       opts.Pool,
			Groups:     p.groups,
			Difficulty: rarityToDifficulty(p.rarity),
		}
		if e.ID == "" {
			e.ID = slug(p.title)
		}
		out.Items = append(out.Items, e)
	}
	return out, nil
}

// rarityToDifficulty 把稀有度映射成难度（稀有度越高越"不好认"）。
func rarityToDifficulty(rarity int) int {
	switch {
	case rarity >= 6:
		return 3
	case rarity == 5:
		return 2
	case rarity == 4:
		return 1
	default:
		return 0
	}
}

// ---------------------------------------------------------------- 导出与下载

// Save 把数据集写到 path（同时写出 labels.txt，供人工核对）。
func (f *File) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o640); err != nil {
		return err
	}
	labelsPath := filepath.Join(filepath.Dir(path), "labels.txt")
	labels := make([]string, 0, len(f.Items))
	for _, it := range f.Items {
		labels = append(labels, it.Label)
	}
	return os.WriteFile(labelsPath, []byte(strings.Join(labels, "\n")+"\n"), 0o640)
}

// DownloadImages 把 http(s) 图片下载到 dir，并把数据集里的 image 改写为相对路径。
// 返回下载成功/失败数量。
func (f *File) DownloadImages(ctx context.Context, dir, userAgent string) (int, int, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return 0, 0, err
	}
	client := &http.Client{Timeout: 90 * time.Second}
	if userAgent == "" {
		userAgent = "tg-gatekeeper-dataset/1.0"
	}
	okCount, failCount := 0, 0
	for i := range f.Items {
		ref := f.Items[i].Image
		if !strings.HasPrefix(ref, "http://") && !strings.HasPrefix(ref, "https://") {
			continue
		}
		name := f.Items[i].ID
		if name == "" {
			name = slug(f.Items[i].Label)
		}
		ext := filepath.Ext(strings.Split(ref, "?")[0])
		if ext == "" || len(ext) > 5 {
			ext = ".jpg"
		}
		dst := filepath.Join(dir, name+ext)
		if _, err := os.Stat(dst); err == nil {
			f.Items[i].Image = filepath.ToSlash(filepath.Join(filepath.Base(dir), name+ext))
			okCount++
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
		if err != nil {
			failCount++
			continue
		}
		req.Header.Set("User-Agent", userAgent)
		resp, err := client.Do(req)
		if err != nil {
			failCount++
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || len(body) == 0 {
			failCount++
			continue
		}
		tmp := dst + ".tmp"
		if err := os.WriteFile(tmp, body, 0o640); err != nil {
			failCount++
			continue
		}
		if err := os.Rename(tmp, dst); err != nil {
			failCount++
			continue
		}
		f.Items[i].Image = filepath.ToSlash(filepath.Join(filepath.Base(dir), name+ext))
		okCount++
	}
	return okCount, failCount, nil
}

// ---------------------------------------------------------------- MediaWiki 客户端

type wikiClient struct {
	api  string
	ua   string
	http *http.Client
}

// get 发起一次 MediaWiki API 调用。
//
// 注意：titles 参数里的空格必须编码为 %20（不能用表单编码的 '+'），否则
// "File:Chen Qianyu Splash Art.png" 这类标题会被当成字面加号而查不到。
func (c *wikiClient) get(ctx context.Context, params map[string]string) ([]byte, error) {
	params["format"] = "json"
	params["formatversion"] = "2"
	parts := make([]string, 0, len(params))
	for k, v := range params {
		parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
	}
	q := strings.Join(parts, "&")
	// QueryEscape 会把空格编成 '+'，MediaWiki 需要 %20。
	q = strings.ReplaceAll(q, "+", "%20")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.api+"?"+q, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.ua)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		_ = resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("wiki 返回 %d", resp.StatusCode)
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		return body, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("wiki 请求失败")
	}
	return nil, lastErr
}

func (c *wikiClient) categoryMembers(ctx context.Context, category string) ([]string, error) {
	body, err := c.get(ctx, map[string]string{
		"action": "query", "list": "categorymembers", "cmtitle": category, "cmlimit": "500",
	})
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Query struct {
			CategoryMembers []struct {
				Title string `json:"title"`
				NS    int    `json:"ns"`
			} `json:"categorymembers"`
		} `json:"query"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("解析分类成员失败: %w", err)
	}
	var out []string
	for _, m := range parsed.Query.CategoryMembers {
		if m.NS == 0 {
			out = append(out, m.Title)
		}
	}
	return out, nil
}

func (c *wikiClient) pageContents(ctx context.Context, titles []string) (map[string]string, error) {
	body, err := c.get(ctx, map[string]string{
		"action": "query", "prop": "revisions", "rvslots": "main", "rvprop": "content",
		"titles": strings.Join(titles, "|"),
	})
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Query struct {
			Pages []struct {
				Title     string `json:"title"`
				Revisions []struct {
					Slots struct {
						Main struct {
							Content string `json:"content"`
						} `json:"main"`
					} `json:"slots"`
				} `json:"revisions"`
			} `json:"pages"`
		} `json:"query"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("解析页面内容失败: %w", err)
	}
	out := make(map[string]string, len(parsed.Query.Pages))
	for _, p := range parsed.Query.Pages {
		if len(p.Revisions) == 0 {
			continue
		}
		out[p.Title] = p.Revisions[0].Slots.Main.Content
	}
	return out, nil
}

func (c *wikiClient) imageURLs(ctx context.Context, files []string, thumbWidth int) (map[string]string, error) {
	uniq := map[string]bool{}
	var list []string
	for _, f := range files {
		if f == "" || uniq[f] {
			continue
		}
		uniq[f] = true
		list = append(list, f)
	}
	out := map[string]string{}
	const batch = 20
	for i := 0; i < len(list); i += batch {
		end := i + batch
		if end > len(list) {
			end = len(list)
		}
		titles := make([]string, 0, end-i)
		for _, f := range list[i:end] {
			titles = append(titles, "File:"+f)
		}
		params := map[string]string{
			"action": "query", "prop": "imageinfo", "iiprop": "url|size",
			"titles": strings.Join(titles, "|"),
		}
		if thumbWidth > 0 {
			params["iiurlwidth"] = strconv.Itoa(thumbWidth)
		}
		body, err := c.get(ctx, params)
		if err != nil {
			return nil, err
		}
		var parsed struct {
			Query struct {
				Pages []struct {
					Title     string `json:"title"`
					ImageInfo []struct {
						URL      string `json:"url"`
						ThumbURL string `json:"thumburl"`
					} `json:"imageinfo"`
				} `json:"pages"`
			} `json:"query"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("解析图片信息失败: %w", err)
		}
		for _, p := range parsed.Query.Pages {
			if len(p.ImageInfo) == 0 {
				continue
			}
			u := p.ImageInfo[0].ThumbURL
			if u == "" {
				u = p.ImageInfo[0].URL
			}
			out[normFile(strings.TrimPrefix(p.Title, "File:"))] = u
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- 文本解析

var (
	fieldLine = regexp.MustCompile(`(?m)^\|[ \t]*([A-Za-z_][A-Za-z0-9_ -]*)[ \t]*=[ \t]*([^\r\n]*)`)
	nextLine  = regexp.MustCompile(`^[\r\n]+[ \t]*([^\r\n]+)`)
)

// wikiField 从信息框 wikitext 里取字段值。
// 值可能写在下一行（例如 |image = \nFoo Splash Art.png），因此空值要继续看下一行，
// 但下一行如果以 "|" 开头就说明该字段确实是空的。
func wikiField(text, key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, m := range fieldLine.FindAllStringSubmatchIndex(text, -1) {
		name := strings.ToLower(strings.TrimSpace(text[m[2]:m[3]]))
		if name != key {
			continue
		}
		value := strings.TrimSpace(text[m[4]:m[5]])
		if value != "" {
			return value
		}
		rest := text[m[5]:]
		if nm := nextLine.FindStringSubmatch(rest); nm != nil {
			cand := strings.TrimSpace(nm[1])
			if !strings.HasPrefix(cand, "|") {
				return cand
			}
		}
		return ""
	}
	return ""
}

// normalizeWikiFile 清理图片字段：可能是 "A.png"、"[[File:A.png]]" 或 "A.png;B.png"。
func normalizeWikiFile(v string) string {
	if v == "" {
		return ""
	}
	v = strings.TrimSpace(strings.Split(v, ";")[0])
	v = strings.TrimPrefix(v, "[[")
	v = strings.TrimPrefix(v, "File:")
	v = strings.TrimSuffix(v, "]]")
	return strings.TrimSpace(v)
}

func normFile(name string) string {
	return strings.ReplaceAll(strings.TrimSpace(name), "_", " ")
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	out := slugRe.ReplaceAllString(strings.ToLower(s), "_")
	return strings.Trim(out, "_")
}
