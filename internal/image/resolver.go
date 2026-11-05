// Package image 实现图片解析管线：取图、限制、重编码、缓存。
//
// 重编码的目的不只是省流量：它同时抹掉源文件名、EXIF、注释等旁路信息，避免用户
// 通过读元数据而不是看图来识别正确答案。
package image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	// 注册解码器。WebP 只解码（终端输出统一为 jpeg/png），GIF 取首帧。
	_ "golang.org/x/image/webp"
	_ "image/gif"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
)

// ErrImage 是图片处理失败的统一错误类型；调用方按"取图失败"处理（重试其它条目）。
var ErrImage = errors.New("图片处理失败")

// Resolver 是 ports.ImageResolver 的默认实现。
type Resolver struct {
	cfg    config.Images
	client *http.Client
	mu     sync.Mutex
	writes int
}

// New 构造 resolver。
func New(cfg config.Images) (*Resolver, error) {
	r := &Resolver{cfg: cfg}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          16,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: time.Duration(cfg.HTTP.TimeoutSeconds) * time.Second,
		DialContext:           dialGuard(cfg.HTTP.AllowedHosts, cfg.HTTP.AllowPrivateIPs),
	}
	r.client = &http.Client{
		Timeout:   time.Duration(cfg.HTTP.TimeoutSeconds) * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > cfg.HTTP.MaxRedirects {
				return fmt.Errorf("%w: 重定向超过 %d 次", ErrImage, cfg.HTTP.MaxRedirects)
			}
			return nil
		},
	}
	if cfg.Cache.Enabled && cfg.Cache.Dir != "" {
		if err := os.MkdirAll(cfg.Cache.Dir, 0o750); err != nil {
			return nil, fmt.Errorf("创建图片缓存目录失败: %w", err)
		}
	}
	return r, nil
}

// Resolve 实现 ports.ImageResolver。
func (r *Resolver) Resolve(ctx context.Context, imageRef string) (ports.ResolvedImage, error) {
	ref := strings.TrimSpace(imageRef)
	if ref == "" {
		return ports.ResolvedImage{}, fmt.Errorf("%w: 空图片引用", ErrImage)
	}
	key := refCacheKey(ref)
	if cached, ok := r.loadCache(key); ok {
		switch {
		case isHTTPRef(ref):
			// 服务端给了 ETag / Last-Modified 就每次做条件请求，304 才用缓存。
			// 只按 URL 做键、靠 TTL 过期，会让"原地换图"在 TTL 内一直拿到旧图。
			if fresh, ok := r.revalidate(ctx, ref, key, cached); ok {
				return fresh, nil
			}
			return cached, nil
		case strings.HasPrefix(ref, "data:"):
			return cached, nil
		default:
			// 本地文件看内容指纹：源图被原地替换时 mtime/大小会变，旧缓存当场作废。
			if !r.localCacheStale(ref, key) {
				return cached, nil
			}
			r.dropCache(key)
		}
	}

	res, err := r.fetch(ctx, ref)
	if err != nil {
		return ports.ResolvedImage{}, err
	}
	out, err := r.reencode(res.raw)
	if err != nil {
		return ports.ResolvedImage{}, err
	}
	out.CacheKey = key
	r.storeCache(key, out)
	meta := cacheMeta{URL: ref, ETag: res.etag, LastModified: res.lastModified}
	meta.SourceMTime, meta.SourceSize = r.sourceFingerprint(ref)
	r.storeCacheMeta(key, meta)
	return out, nil
}

// Close 释放资源（当前无长连接需要单独处理）。
func (r *Resolver) Close() error { return nil }

// fetchResult 是一次取图的结果。http 会把校验器带回来，供缓存做条件请求。
type fetchResult struct {
	raw          []byte
	etag         string
	lastModified string
	notModified  bool
}

func (r *Resolver) fetch(ctx context.Context, ref string) (fetchResult, error) {
	switch {
	case strings.HasPrefix(ref, "data:"):
		raw, err := decodeDataURL(ref)
		return fetchResult{raw: raw}, err
	case isHTTPRef(ref):
		scheme := "http"
		if strings.HasPrefix(strings.ToLower(ref), "https://") {
			scheme = "https"
		}
		if !r.schemeAllowed(scheme) {
			return fetchResult{}, fmt.Errorf("%w: images.allowed_schemes 未允许 %s", ErrImage, scheme)
		}
		return r.fetchHTTP(ctx, ref, cacheMeta{})
	default:
		if !r.schemeAllowed("file") {
			return fetchResult{}, fmt.Errorf("%w: images.allowed_schemes 未允许 file", ErrImage)
		}
		raw, err := r.readLocal(localPath(ref))
		return fetchResult{raw: raw}, err
	}
}

// schemeAllowed 按实际协议逐个校验：只写 https 就不该放行 http（反之亦然）。
func (r *Resolver) schemeAllowed(scheme string) bool {
	return contains(r.cfg.AllowedSchemes, scheme)
}

func isHTTPRef(ref string) bool {
	l := strings.ToLower(ref)
	return strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://")
}

// localPath 去掉 file:/file:// 前缀，得到本地路径。
func localPath(ref string) string {
	return strings.TrimPrefix(strings.TrimPrefix(ref, "file://"), "file:")
}

func (r *Resolver) readLocal(p string) ([]byte, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, fmt.Errorf("%w: 解析本地路径失败: %v", ErrImage, err)
	}
	if len(r.cfg.LocalRoots) > 0 {
		ok := false
		for _, root := range r.cfg.LocalRoots {
			rootAbs, err := filepath.Abs(root)
			if err != nil {
				continue
			}
			rel, err := filepath.Rel(rootAbs, abs)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
				ok = true
				break
			}
		}
		if !ok {
			return nil, fmt.Errorf("%w: 本地图片 %s 不在 images.local_roots 允许的目录内", ErrImage, abs)
		}
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取本地图片失败: %v", ErrImage, err)
	}
	if st.IsDir() {
		return nil, fmt.Errorf("%w: %s 是目录", ErrImage, abs)
	}
	if st.Size() > r.cfg.MaxDownloadBytes {
		return nil, fmt.Errorf("%w: 图片 %d 字节超过上限 %d", ErrImage, st.Size(), r.cfg.MaxDownloadBytes)
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取本地图片失败: %v", ErrImage, err)
	}
	return raw, nil
}

func (r *Resolver) fetchHTTP(ctx context.Context, u string, cond cacheMeta) (fetchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fetchResult{}, fmt.Errorf("%w: 构造图片请求失败: %v", ErrImage, err)
	}
	ua := r.cfg.HTTP.UserAgent
	if ua == "" {
		ua = "tg-gatekeeper/1"
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "image/*")
	// 条件请求：上一轮拿到的校验器原样带回去，没变就让服务端回 304，省掉一次重编码。
	if cond.ETag != "" {
		req.Header.Set("If-None-Match", cond.ETag)
	}
	if cond.LastModified != "" {
		req.Header.Set("If-Modified-Since", cond.LastModified)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return fetchResult{}, fmt.Errorf("%w: 下载图片失败: %v", ErrImage, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && (cond.ETag != "" || cond.LastModified != "") {
		return fetchResult{notModified: true}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return fetchResult{}, fmt.Errorf("%w: 图片响应 %d", ErrImage, resp.StatusCode)
	}
	if resp.ContentLength > r.cfg.MaxDownloadBytes && resp.ContentLength > 0 {
		return fetchResult{}, fmt.Errorf("%w: 图片 %d 字节超过上限 %d", ErrImage, resp.ContentLength, r.cfg.MaxDownloadBytes)
	}
	// 多读 1 字节以便判断是否超限（Content-Length 可能缺失或撒谎）。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, r.cfg.MaxDownloadBytes+1))
	if err != nil {
		return fetchResult{}, fmt.Errorf("%w: 读取图片响应失败: %v", ErrImage, err)
	}
	if int64(len(raw)) > r.cfg.MaxDownloadBytes {
		return fetchResult{}, fmt.Errorf("%w: 图片超过 %d 字节上限", ErrImage, r.cfg.MaxDownloadBytes)
	}
	if len(raw) == 0 {
		return fetchResult{}, fmt.Errorf("%w: 图片响应为空", ErrImage)
	}
	return fetchResult{
		raw:          raw,
		etag:         resp.Header.Get("ETag"),
		lastModified: resp.Header.Get("Last-Modified"),
	}, nil
}

// reencode 解码并重新编码，顺带做像素与尺寸限制。
func (r *Resolver) reencode(raw []byte) (ports.ResolvedImage, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return ports.ResolvedImage{}, fmt.Errorf("%w: 不是可识别的图片: %v", ErrImage, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return ports.ResolvedImage{}, fmt.Errorf("%w: 图片尺寸非法", ErrImage)
	}
	if int64(cfg.Width)*int64(cfg.Height) > r.cfg.MaxDecodedPixels {
		return ports.ResolvedImage{}, fmt.Errorf("%w: 图片 %dx%d 像素数超过上限 %d",
			ErrImage, cfg.Width, cfg.Height, r.cfg.MaxDecodedPixels)
	}
	if cfg.Width > r.cfg.MaxWidth || cfg.Height > r.cfg.MaxHeight {
		return ports.ResolvedImage{}, fmt.Errorf("%w: 图片 %dx%d 超过尺寸上限 %dx%d",
			ErrImage, cfg.Width, cfg.Height, r.cfg.MaxWidth, r.cfg.MaxHeight)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return ports.ResolvedImage{}, fmt.Errorf("%w: 解码图片失败: %v", ErrImage, err)
	}

	var buf bytes.Buffer
	format := strings.ToLower(r.cfg.OutputFormat)
	switch format {
	case "png":
		if err := png.Encode(&buf, img); err != nil {
			return ports.ResolvedImage{}, fmt.Errorf("%w: 编码 PNG 失败: %v", ErrImage, err)
		}
		return ports.ResolvedImage{
			Bytes:     buf.Bytes(),
			Filename:  "challenge.png",
			MediaType: "image/png",
		}, nil
	default:
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: r.cfg.JPEGQuality}); err != nil {
			return ports.ResolvedImage{}, fmt.Errorf("%w: 编码 JPEG 失败: %v", ErrImage, err)
		}
		return ports.ResolvedImage{
			Bytes:     buf.Bytes(),
			Filename:  "challenge.jpg",
			MediaType: "image/jpeg",
		}, nil
	}
}

// ---------------------------------------------------------------- 缓存

// localCacheStale 判断本地源图是否已经变过：键仍是路径，靠指纹（mtime + 大小）发现替代。
// 没有指纹的老缓存、或源文件暂时读不到时，按原行为继续用缓存。
func (r *Resolver) localCacheStale(ref, key string) bool {
	meta, ok := r.loadCacheMeta(key)
	if !ok || meta.SourceSize == 0 {
		return false
	}
	st, err := os.Stat(localPath(ref))
	if err != nil {
		return false
	}
	return st.Size() != meta.SourceSize || st.ModTime().UnixNano() != meta.SourceMTime
}

// sourceFingerprint 取本地源图的 mtime 与大小；不是本地文件时返回 0,0。
func (r *Resolver) sourceFingerprint(ref string) (mtime, size int64) {
	if isHTTPRef(ref) || strings.HasPrefix(ref, "data:") {
		return 0, 0
	}
	st, err := os.Stat(localPath(ref))
	if err != nil || !st.Mode().IsRegular() {
		return 0, 0
	}
	return st.ModTime().UnixNano(), st.Size()
}

// dropCache 连图带 meta 一起丢掉（源图变了，旧缓存不能再用）。
func (r *Resolver) dropCache(key string) {
	if p := r.cachePath(key); p != "" {
		_ = os.Remove(p)
	}
	if p := r.metaPath(key); p != "" {
		_ = os.Remove(p)
	}
}

func refCacheKey(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(sum[:])[:32]
}

// cacheMeta 是缓存的校验信息：http 存 ETag / Last-Modified，本地文件存源图指纹。
type cacheMeta struct {
	URL          string `json:"url"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	SourceMTime  int64  `json:"source_mtime,omitempty"`
	SourceSize   int64  `json:"source_size,omitempty"`
}

func (r *Resolver) metaPath(key string) string {
	if p := r.cachePath(key); p != "" {
		return p + ".meta"
	}
	return ""
}

func (r *Resolver) loadCacheMeta(key string) (cacheMeta, bool) {
	p := r.metaPath(key)
	if p == "" {
		return cacheMeta{}, false
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return cacheMeta{}, false
	}
	var m cacheMeta
	if err := json.Unmarshal(raw, &m); err != nil || m.URL == "" {
		return cacheMeta{}, false
	}
	return m, true
}

func (r *Resolver) storeCacheMeta(key string, m cacheMeta) {
	p := r.metaPath(key)
	if p == "" {
		return
	}
	if m.ETag == "" && m.LastModified == "" && m.SourceSize == 0 {
		_ = os.Remove(p) // 既没有 HTTP 校验器也没有源图指纹：不留空 meta
		return
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o640); err != nil {
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
	}
}

// revalidate 对 HTTP 缓存做条件请求：304 继续用缓存，200 换成新图。
func (r *Resolver) revalidate(ctx context.Context, ref, key string, cached ports.ResolvedImage) (ports.ResolvedImage, bool) {
	meta, ok := r.loadCacheMeta(key)
	if !ok || (meta.ETag == "" && meta.LastModified == "") {
		return ports.ResolvedImage{}, false // 服务端没给校验器：沿用 TTL
	}
	res, err := r.fetchHTTP(ctx, ref, meta)
	if err != nil {
		// 校验失败（网络抖动、服务端 5xx）：继续用缓存，别让一次校验失败就出不了题。
		return ports.ResolvedImage{}, false
	}
	if res.notModified {
		r.touchCache(key)
		return cached, true
	}
	out, err := r.reencode(res.raw)
	if err != nil {
		return ports.ResolvedImage{}, false
	}
	out.CacheKey = key
	r.storeCache(key, out)
	r.storeCacheMeta(key, cacheMeta{URL: ref, ETag: res.etag, LastModified: res.lastModified})
	return out, true
}

// touchCache 刷新缓存文件 mtime：304 之后别让它被 TTL / 容量回收当成旧文件删掉。
func (r *Resolver) touchCache(key string) {
	if p := r.cachePath(key); p != "" {
		now := time.Now()
		_ = os.Chtimes(p, now, now)
	}
}

func (r *Resolver) cachePath(key string) string {
	if !r.cfg.Cache.Enabled || r.cfg.Cache.Dir == "" {
		return ""
	}
	ext := ".jpg"
	if strings.EqualFold(r.cfg.OutputFormat, "png") {
		ext = ".png"
	}
	return filepath.Join(r.cfg.Cache.Dir, key+ext)
}

func (r *Resolver) loadCache(key string) (ports.ResolvedImage, bool) {
	p := r.cachePath(key)
	if p == "" {
		return ports.ResolvedImage{}, false
	}
	st, err := os.Stat(p)
	if err != nil {
		return ports.ResolvedImage{}, false
	}
	if ttl := time.Duration(r.cfg.Cache.TTLSeconds) * time.Second; ttl > 0 && time.Since(st.ModTime()) > ttl {
		_ = os.Remove(p)
		return ports.ResolvedImage{}, false
	}
	raw, err := os.ReadFile(p)
	if err != nil || len(raw) == 0 {
		return ports.ResolvedImage{}, false
	}
	media, name := "image/jpeg", "challenge.jpg"
	if strings.HasSuffix(p, ".png") {
		media, name = "image/png", "challenge.png"
	}
	return ports.ResolvedImage{Bytes: raw, Filename: name, MediaType: media, CacheKey: key}, true
}

func (r *Resolver) storeCache(key string, img ports.ResolvedImage) {
	p := r.cachePath(key)
	if p == "" {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, img.Bytes, 0o640); err != nil {
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return
	}
	r.mu.Lock()
	r.writes++
	shouldPrune := r.writes%64 == 0
	r.mu.Unlock()
	if shouldPrune {
		r.prune()
	}
}

// prune 按容量上限回收最旧的缓存文件。失败不影响主流程。
func (r *Resolver) prune() {
	dir := r.cfg.Cache.Dir
	if dir == "" || r.cfg.Cache.MaxBytes <= 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type fileInfo struct {
		path string
		size int64
		mod  time.Time
	}
	files := make([]fileInfo, 0, len(entries))
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{filepath.Join(dir, e.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	if total <= r.cfg.Cache.MaxBytes {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files {
		if total <= r.cfg.Cache.MaxBytes {
			break
		}
		if err := os.Remove(f.path); err == nil {
			total -= f.size
		}
	}
}

// ---------------------------------------------------------------- 辅助

func decodeDataURL(ref string) ([]byte, error) {
	idx := strings.Index(ref, ",")
	if idx < 0 {
		return nil, fmt.Errorf("%w: data URL 缺少逗号", ErrImage)
	}
	meta, payload := ref[:idx], ref[idx+1:]
	if !strings.Contains(meta, ";base64") {
		return nil, fmt.Errorf("%w: 仅支持 base64 的 data URL", ErrImage)
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: data URL 解码失败: %v", ErrImage, err)
	}
	return raw, nil
}

// dialGuard 在建立连接前校验目标 IP，防止 SSRF（含 DNS rebinding 与重定向）。
func dialGuard(allowedHosts []string, allowPrivate bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("%w: 地址 %q 非法", ErrImage, addr)
		}
		if len(allowedHosts) > 0 && !hostAllowed(host, allowedHosts) {
			return nil, fmt.Errorf("%w: 主机 %s 不在 images.http.allowed_hosts 内", ErrImage, host)
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("%w: 解析主机 %s 失败: %v", ErrImage, host, err)
		}
		var lastErr error
		for _, ip := range ips {
			if !allowPrivate && isBlockedIP(ip) {
				lastErr = fmt.Errorf("%w: 主机 %s 解析到受限地址 %s（内网/环回/链路本地）", ErrImage, host, ip)
				continue
			}
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("%w: 主机 %s 无可用的解析结果", ErrImage, host)
		}
		return nil, lastErr
	}
}

func hostAllowed(host string, allowed []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if strings.HasPrefix(a, "*.") {
			suffix := a[1:] // ".example.com"
			if strings.HasSuffix(host, suffix) {
				return true
			}
			continue
		}
		if host == a {
			return true
		}
	}
	return false
}

func isBlockedIP(ip netip.Addr) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	// 100.64.0.0/10（CGNAT）与 169.254.169.254 属于云元数据风险面。
	if ip.Is4() {
		b := ip.As4()
		if b[0] == 100 && b[1] >= 64 && b[1] <= 127 {
			return true
		}
		if b[0] == 169 && b[1] == 254 {
			return true
		}
		if b[0] == 0 {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(x), v) {
			return true
		}
	}
	return false
}

// SafeJoin 在 base 目录下安全拼接 rel，阻止路径穿越。
func SafeJoin(base, rel string) (string, error) {
	clean := path.Clean(strings.ReplaceAll(rel, "\\", "/"))
	if strings.HasPrefix(clean, "..") || path.IsAbs(clean) {
		return "", fmt.Errorf("非法相对路径 %q", rel)
	}
	full := filepath.Join(base, filepath.FromSlash(clean))
	relCheck, err := filepath.Rel(base, full)
	if err != nil || strings.HasPrefix(relCheck, "..") {
		return "", fmt.Errorf("路径 %q 越出目录 %s", rel, base)
	}
	return full, nil
}

// ParseHost 取 URL 的 host（无端口），供白名单校验使用。
func ParseHost(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	return u.Hostname(), nil
}
