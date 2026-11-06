package image

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// tempDir 建临时目录并在结束时清理；清理失败只忽略，不让 Windows 上偶发的
// "目录非空/句柄未释放"把测试弄红（t.TempDir 会 fail 测试）。
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tg-gatekeeper-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestLocalFileReplacementInvalidatesCache 源图被原地替换（路径不变、内容变了）时，
// 旧缓存必须当场作废，而不是顶替新图直到 TTL 过期。
func TestLocalFileReplacementInvalidatesCache(t *testing.T) {
	dir := tempDir(t)
	cfg := testConfig()
	cfg.LocalRoots = []string{dir}
	cfg.Cache.Dir = filepath.Join(dir, "cache")
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := writePNG(t, dir, "same-name.png", 16, 16)

	first, err := r.Resolve(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	// 同名覆盖：尺寸不同，mtime 与大小都会变。
	writePNG(t, dir, "same-name.png", 40, 40)
	second, err := r.Resolve(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(first.Bytes, second.Bytes) {
		t.Fatal("源图被替换后仍然拿到旧缓存")
	}
	if w, h := decodeSize(t, second.Bytes); w != 40 || h != 40 {
		t.Fatalf("拿到的不是新图：%dx%d", w, h)
	}
}

// TestHTTPCacheRevalidatesWithETag 服务端给了 ETag 时每次条件请求：304 用缓存，200 换新图。
func TestHTTPCacheRevalidatesWithETag(t *testing.T) {
	var etag atomic.Value
	etag.Store("v1")
	var conditional, notModified int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		cur := etag.Load().(string)
		if inm := req.Header.Get("If-None-Match"); inm != "" {
			atomic.AddInt32(&conditional, 1)
			if inm == cur {
				atomic.AddInt32(&notModified, 1)
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		size := 16
		if cur == "v2" {
			size = 32
		}
		img := image.NewRGBA(image.Rect(0, 0, size, size))
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Errorf("编码失败: %v", err)
			return
		}
		w.Header().Set("ETag", cur)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.HTTP.AllowPrivateIPs = true
	cfg.HTTP.AllowedHosts = []string{"127.0.0.1"}
	cfg.Cache.Dir = tempDir(t)
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	first, err := r.Resolve(context.Background(), srv.URL+"/a.png")
	if err != nil {
		t.Fatal(err)
	}

	// 第二次：服务端内容未变，应当走 304 并复用缓存。
	second, err := r.Resolve(context.Background(), srv.URL+"/a.png")
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&notModified) != 1 {
		t.Fatalf("第二次应当触发 304，实际 notModified=%d conditional=%d", notModified, conditional)
	}
	if !bytes.Equal(first.Bytes, second.Bytes) {
		t.Fatal("304 之后应当复用缓存字节")
	}

	// 第三次：服务端换了图（ETag 变），必须换成新图。
	etag.Store("v2")
	third, err := r.Resolve(context.Background(), srv.URL+"/a.png")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(third.Bytes, second.Bytes) {
		t.Fatal("服务端换了图，缓存没有刷新")
	}
	if w, h := decodeSize(t, third.Bytes); w != 32 || h != 32 {
		t.Fatalf("拿到的不是新图：%dx%d", w, h)
	}
}

// TestAllowedSchemesIsPerScheme 只写 https 就不该放行 http。
func TestAllowedSchemesIsPerScheme(t *testing.T) {
	cfg := testConfig()
	cfg.AllowedSchemes = []string{"file", "https"}
	cfg.Cache.Dir = tempDir(t)
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Resolve(context.Background(), "http://example.com/a.png")
	if err == nil {
		t.Fatal("allowed_schemes 未列 http 时应当拒绝 http 引用")
	}
	if !strings.Contains(err.Error(), "未允许 http") {
		t.Fatalf("错误信息应当指出协议被拒，实际 %v", err)
	}
}

func decodeSize(t *testing.T, raw []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	return cfg.Width, cfg.Height
}
