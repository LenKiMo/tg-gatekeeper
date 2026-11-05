package dataset

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestDownloadImagesUniqueNamesForNonASCIILabels 中文标签过 slug 之后是空串：
// 以前所有这类条目都落到同一个 ".jpg" 上，后到的还会因为"文件已存在"被当成成功。
func TestDownloadImagesUniqueNamesForNonASCIILabels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		img := image.NewRGBA(image.Rect(0, 0, 8, 8))
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Errorf("编码失败: %v", err)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	dir := t.TempDir()
	f := &File{Items: []Entry{
		{Label: "打印", Image: srv.URL + "/1.png"},
		{Label: "部署", Image: srv.URL + "/2.png"},
		{Label: "备份", Image: srv.URL + "/3.png"},
	}}
	ok, fail, err := f.DownloadImages(context.Background(), dir, "")
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	if ok != 3 || fail != 0 {
		t.Fatalf("应当成功 3 条，实际 ok=%d fail=%d", ok, fail)
	}

	seen := map[string]bool{}
	for _, it := range f.Items {
		if it.Image == "" {
			t.Fatal("条目没有写回图片路径")
		}
		if seen[it.Image] {
			t.Fatalf("两个条目指向了同一个文件：%s", it.Image)
		}
		seen[it.Image] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	pngs := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".png" {
			pngs++
		}
	}
	if pngs != 3 {
		t.Fatalf("应当落下 3 个不同文件，实际 %d", pngs)
	}
}
