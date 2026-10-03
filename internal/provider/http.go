package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
)

// httpProvider 从一个返回 JSON/JSONL 的 HTTP 接口读取数据集。
//
// 用途：宿主系统自己维护数据（游戏数据服务、内部工单系统…），通过 HTTP 暴露给 bot。
// 安全默认：必须显式给出 allowed_hosts，且默认拒绝内网/环回地址；支持 ETag/304 与
// 磁盘缓存，冷启动时可用最近一次有效快照。
type httpProvider struct {
	name       string
	url        string
	method     string
	format     string
	headers    map[string]string
	bearer     string
	itemsPath  string
	timeout    time.Duration
	maxBytes   int64
	cacheFile  string
	staleAfter time.Duration
	client     *http.Client

	mu      sync.Mutex
	etag    string
	lastMod string
	cached  domain.Snapshot
}

func newHTTP(name string, def config.ProviderDef) (ports.Source, error) {
	d := def.HTTP
	timeout := time.Duration(d.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	method := strings.ToUpper(strings.TrimSpace(d.Method))
	if method == "" {
		method = http.MethodGet
	}
	maxBytes := d.MaxResponseBytes
	if maxBytes <= 0 {
		maxBytes = 8 << 20
	}
	p := &httpProvider{
		name:       name,
		url:        d.URL,
		method:     method,
		format:     strings.ToLower(d.Format),
		headers:    d.Headers,
		bearer:     d.BearerToken,
		itemsPath:  d.ResponseItemsPath,
		timeout:    timeout,
		maxBytes:   maxBytes,
		cacheFile:  d.CacheFile,
		staleAfter: time.Duration(d.StaleIfErrorSeconds) * time.Second,
	}
	p.client = &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           guardDial(d.AllowedHosts, d.AllowPrivateIPs),
			ResponseHeaderTimeout: timeout,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > d.MaxRedirects {
				return fmt.Errorf("数据集请求重定向超过 %d 次", d.MaxRedirects)
			}
			return nil
		},
	}
	return p, nil
}

func (p *httpProvider) ID() string { return "http:" + p.url }

func (p *httpProvider) Snapshot(ctx context.Context) (domain.Snapshot, error) {
	ctx = snapshotCtx(ctx)
	body, notModified, err := p.fetch(ctx)
	if err != nil {
		if snap, ok := p.loadCache(); ok {
			return snap, nil
		}
		return domain.Snapshot{}, err
	}
	if notModified {
		if snap, ok := p.loadCache(); ok {
			return snap, nil
		}
		p.mu.Lock()
		cached := p.cached
		p.mu.Unlock()
		if cached.Entries != nil {
			return cached, nil
		}
		return domain.Snapshot{}, fmt.Errorf("数据集返回 304 但本地没有可用快照")
	}
	if p.itemsPath != "" {
		trimmed, err := extractPath(body, p.itemsPath)
		if err != nil {
			return domain.Snapshot{}, err
		}
		body = trimmed
	}
	recs, err := decodeRecords(body, p.format)
	if err != nil {
		if snap, ok := p.loadCache(); ok {
			return snap, nil
		}
		return domain.Snapshot{}, fmt.Errorf("解析远端数据集失败: %w", err)
	}
	snap := buildSnapshot(p.ID(), recs, "default")
	if len(snap.Entries) == 0 {
		return domain.Snapshot{}, fmt.Errorf("远端数据集里没有可用条目")
	}
	p.mu.Lock()
	p.cached = snap
	p.mu.Unlock()
	p.saveCache(body)
	return snap, nil
}

func (p *httpProvider) Close() error { return nil }

func (p *httpProvider) fetch(ctx context.Context) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, p.method, p.url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("构造数据集请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	if p.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+p.bearer)
	}
	p.mu.Lock()
	if p.etag != "" {
		req.Header.Set("If-None-Match", p.etag)
	}
	if p.lastMod != "" {
		req.Header.Set("If-Modified-Since", p.lastMod)
	}
	p.mu.Unlock()

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("请求数据集失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("数据集响应 %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, p.maxBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("读取数据集响应失败: %w", err)
	}
	if int64(len(raw)) > p.maxBytes {
		return nil, false, fmt.Errorf("数据集响应超过 %d 字节", p.maxBytes)
	}
	p.mu.Lock()
	p.etag = resp.Header.Get("ETag")
	p.lastMod = resp.Header.Get("Last-Modified")
	p.mu.Unlock()
	return raw, false, nil
}

func (p *httpProvider) loadCache() (domain.Snapshot, bool) {
	if p.cacheFile == "" {
		return domain.Snapshot{}, false
	}
	st, err := os.Stat(p.cacheFile)
	if err != nil {
		return domain.Snapshot{}, false
	}
	if p.staleAfter > 0 && time.Since(st.ModTime()) > p.staleAfter {
		return domain.Snapshot{}, false
	}
	raw, err := os.ReadFile(p.cacheFile)
	if err != nil {
		return domain.Snapshot{}, false
	}
	recs, err := decodeRecords(raw, p.format)
	if err != nil {
		return domain.Snapshot{}, false
	}
	snap := buildSnapshot(p.ID(), recs, "default")
	if len(snap.Entries) == 0 {
		return domain.Snapshot{}, false
	}
	return snap, true
}

func (p *httpProvider) saveCache(body []byte) {
	if p.cacheFile == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p.cacheFile), 0o750); err != nil {
		return
	}
	tmp := p.cacheFile + ".tmp"
	if err := os.WriteFile(tmp, body, 0o640); err != nil {
		return
	}
	_ = os.Rename(tmp, p.cacheFile)
}

// extractPath 取简单点路径，如 "data.items"。
func extractPath(body []byte, dotted string) ([]byte, error) {
	var cur any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&cur); err != nil {
		return nil, fmt.Errorf("数据集不是合法 JSON: %w", err)
	}
	for _, part := range strings.Split(dotted, ".") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("response_items_path 在 %q 处不是对象", part)
		}
		cur, ok = m[part]
		if !ok {
			return nil, fmt.Errorf("response_items_path 缺少字段 %q", part)
		}
	}
	return json.Marshal(cur)
}

// guardDial 校验目标 IP，阻止 SSRF。
func guardDial(allowedHosts []string, allowPrivate bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("地址 %q 非法", addr)
		}
		if len(allowedHosts) > 0 && !hostInList(host, allowedHosts) {
			return nil, fmt.Errorf("主机 %s 不在 allowed_hosts 内", host)
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("解析主机 %s 失败: %w", host, err)
		}
		var lastErr error
		for _, ip := range ips {
			if !allowPrivate && blockedIP(ip) {
				lastErr = fmt.Errorf("主机 %s 解析到受限地址 %s", host, ip)
				continue
			}
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("主机 %s 无可用的解析结果", host)
		}
		return nil, lastErr
	}
}

func hostInList(host string, allowed []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if strings.HasPrefix(a, "*.") && strings.HasSuffix(host, a[1:]) {
			return true
		}
		if host == a {
			return true
		}
	}
	return false
}

func blockedIP(ip netip.Addr) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
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
