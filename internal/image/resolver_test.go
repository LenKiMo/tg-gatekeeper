package image

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
)

func testConfig() config.Images {
	cfg := config.Default().Images
	cfg.Cache.Enabled = true
	cfg.Cache.Dir = ""
	cfg.AllowedSchemes = []string{"file", "http", "https"}
	return cfg
}

func writePNG(t *testing.T, dir, name string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 7), G: uint8(y * 5), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func dataURL(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// TestResolveLocalFileAndStripFilename 验证本地图片被重编码，且输出文件名不泄漏答案。
func TestResolveLocalFileAndStripFilename(t *testing.T) {
	dir := t.TempDir()
	path := writePNG(t, dir, "真实的答案名字.png", 64, 64)

	cfg := testConfig()
	cfg.LocalRoots = []string{dir}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	img, err := r.Resolve(context.Background(), path)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if img.Filename != "challenge.jpg" {
		t.Fatalf("输出文件名 = %q，必须固定为 challenge.jpg（否则会泄漏答案）", img.Filename)
	}
	if img.MediaType != "image/jpeg" {
		t.Fatalf("媒体类型 = %q，期望 image/jpeg", img.MediaType)
	}
	if bytes.Contains(img.Bytes, []byte("真实的答案名字")) {
		t.Fatal("重编码后的图片里不应残留原文件名/文本元数据")
	}
	if len(img.Bytes) == 0 {
		t.Fatal("图片字节为空")
	}
}

// TestResolveRejectsPathTraversal 验证 file: 引用不能越出 local_roots。
func TestResolveRejectsPathTraversal(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := writePNG(t, outside, "secret.png", 8, 8)

	cfg := testConfig()
	cfg.LocalRoots = []string{root}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(context.Background(), secret); err == nil {
		t.Fatal("越出 local_roots 的图片必须被拒绝")
	}
	if _, err := r.Resolve(context.Background(), filepath.Join(root, "..", filepath.Base(outside), "secret.png")); err == nil {
		t.Fatal("带 .. 的路径必须被拒绝")
	}
}

// TestResolveRejectsOversizeAndNonImage 验证体积与像素上限。
func TestResolveRejectsOversizeAndNonImage(t *testing.T) {
	dir := t.TempDir()
	big := writePNG(t, dir, "big.png", 400, 400)

	cfg := testConfig()
	cfg.LocalRoots = []string{dir}
	cfg.MaxDecodedPixels = 1000 // 400*400 远超上限
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(context.Background(), big); err == nil {
		t.Fatal("超过像素上限的图片必须被拒绝")
	}

	notImage := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(notImage, []byte("我不是图片"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(context.Background(), notImage); err == nil {
		t.Fatal("非图片文件必须被拒绝")
	}
}

// TestResolveDataURL 验证 data: URL（便于自检与测试）。
func TestResolveDataURL(t *testing.T) {
	r, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	img, err := r.Resolve(context.Background(), dataURL(t, 16, 16))
	if err != nil {
		t.Fatalf("data URL 解析失败: %v", err)
	}
	if img.Filename != "challenge.jpg" || len(img.Bytes) == 0 {
		t.Fatalf("data URL 结果异常: %+v", img)
	}
	if _, err := r.Resolve(context.Background(), "data:image/png;notbase64,xxx"); err == nil {
		t.Fatal("非 base64 的 data URL 必须被拒绝")
	}
}

// TestHTTPImageAllowedHostAndSSRF 验证主机白名单与内网阻断。
func TestHTTPImageAllowedHostAndSSRF(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		img := image.NewRGBA(image.Rect(0, 0, 32, 32))
		w.Header().Set("Content-Type", "image/png")
		_ = png.Encode(w, img)
	}))
	defer srv.Close()

	// 默认策略：内网/环回地址被拒绝（防 SSRF）。
	r1, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r1.Resolve(context.Background(), srv.URL+"/a.png"); err == nil {
		t.Fatal("默认应当拒绝环回地址（SSRF 防护）")
	}

	// 显式允许任意主机/内网时可以取到（用于内网部署）。
	cfg := testConfig()
	cfg.HTTP.AllowPrivateIPs = true
	cfg.HTTP.AllowedHosts = []string{"127.0.0.1"}
	r2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	img, err := r2.Resolve(context.Background(), srv.URL+"/a.png")
	if err != nil {
		t.Fatalf("显式允许后应当成功: %v", err)
	}
	if len(img.Bytes) == 0 || img.Filename != "challenge.jpg" {
		t.Fatalf("HTTP 图片结果异常: %+v", img)
	}

	// 白名单之外的主机必须被拒绝。
	cfg3 := testConfig()
	cfg3.HTTP.AllowedHosts = []string{"example.com"}
	cfg3.HTTP.AllowPrivateIPs = true
	r3, err := New(cfg3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r3.Resolve(context.Background(), srv.URL+"/a.png"); err == nil {
		t.Fatal("不在白名单内的主机必须被拒绝")
	}
}

// TestCacheReusesBytes 验证缓存命中（避免每次出题都重新下载）。
func TestCacheReusesBytes(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	path := writePNG(t, dir, "a.png", 32, 32)

	cfg := testConfig()
	cfg.LocalRoots = []string{dir}
	cfg.Cache.Dir = cacheDir
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("缓存文件数 = %d，期望 1", len(entries))
	}
	// 删掉源文件后仍能从缓存取到（证明缓存生效）。
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	img, err := r.Resolve(context.Background(), path)
	if err != nil {
		t.Fatalf("缓存应当仍然可用: %v", err)
	}
	if len(img.Bytes) == 0 {
		t.Fatal("缓存内容为空")
	}
}

// TestSafeJoin 验证路径拼接防护。
func TestSafeJoin(t *testing.T) {
	base := filepath.Join(t.TempDir(), "root")
	if _, err := SafeJoin(base, "../escape.png"); err == nil {
		t.Fatal(".. 必须被拒绝")
	}
	if _, err := SafeJoin(base, "ok/child.png"); err != nil {
		t.Fatalf("正常相对路径不应报错: %v", err)
	}
	if got, err := ParseHost("https://images.example.com/a.png"); err != nil || got != "images.example.com" {
		t.Fatalf("ParseHost 结果 %q err=%v", got, err)
	}
	if !strings.Contains(filepath.ToSlash(base), "root") {
		t.Fatal("测试自身异常")
	}
}
