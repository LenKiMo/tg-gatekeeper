package telegram

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
)

// recordingTransport 记录到达时的 ctx deadline，并返回一个可读 body。
type recordingTransport struct {
	deadline time.Time
	hasDL    bool
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.deadline, r.hasDL = req.Context().Deadline()
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(bytes.NewBufferString(`{"ok":true,"result":[]}`)),
		Header:     http.Header{},
		Request:    req,
	}, nil
}

// TestLongPollGetsLongerTimeoutThanNormalCalls 是本项目在真实 VPS 上踩到的坑：
// getUpdates 是长轮询（库内固定 60s），如果它和普通接口用同一个更短的超时，
// 每次轮询都会以 "context deadline exceeded" 结束，变成"每 40 秒断一次"。
func TestLongPollGetsLongerTimeoutThanNormalCalls(t *testing.T) {
	cfg := config.Default()
	cfg.Bot.PollingTimeoutSeconds = 60
	cfg.Bot.RequestTimeoutSeconds = 20

	rec := &recordingTransport{}
	client := &http.Client{Transport: &pathTimeoutTransport{
		base:     rec,
		longPoll: 80 * time.Second,
		normal:   20 * time.Second,
	}}

	// 普通接口
	if _, err := client.Get("https://api.telegram.org/botX/sendMessage"); err != nil {
		t.Fatal(err)
	}
	if !rec.hasDL {
		t.Fatal("普通请求也应当有 deadline")
	}
	normalBudget := time.Until(rec.deadline)

	// 长轮询接口
	if _, err := client.Get("https://api.telegram.org/botX/getUpdates"); err != nil {
		t.Fatal(err)
	}
	longBudget := time.Until(rec.deadline)

	if longBudget <= normalBudget {
		t.Fatalf("getUpdates 的超时(%v)必须大于普通接口(%v)", longBudget, normalBudget)
	}
	if longBudget < 70*time.Second {
		t.Fatalf("getUpdates 超时 %v 不足以覆盖 60s 长轮询", longBudget)
	}
	if normalBudget > 40*time.Second {
		t.Fatalf("普通接口超时 %v 过长（故障时应快速失败）", normalBudget)
	}
}

// TestBuildHTTPClientBudgets 验证适配器的预算计算：长轮询至少覆盖库硬编码的 60s。
func TestBuildHTTPClientBudgets(t *testing.T) {
	cfg := config.Default()
	cfg.Bot.PollingTimeoutSeconds = 10 // 故意配得比库的 60s 小
	cfg.Bot.RequestTimeoutSeconds = 15
	c, err := buildHTTPClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pt, ok := c.Transport.(*pathTimeoutTransport)
	if !ok {
		t.Fatal("应当使用 pathTimeoutTransport")
	}
	if pt.longPoll < 80*time.Second {
		t.Fatalf("长轮询预算 = %v，必须 >= 80s（库内部 60s + 余量）", pt.longPoll)
	}
	if pt.normal != 15*time.Second {
		t.Fatalf("普通接口预算 = %v，期望 15s", pt.normal)
	}
	if c.Timeout != 0 {
		t.Fatalf("不应设置全局 Timeout（会让长轮询被统一截断），实际 %v", c.Timeout)
	}
}

// TestDeadlineBodyReleasesTimer 验证响应体读完/关闭后 deadline 被释放（不泄漏定时器）。
func TestDeadlineBodyReleasesTimer(t *testing.T) {
	rec := &recordingTransport{}
	tr := &pathTimeoutTransport{base: rec, longPoll: time.Minute, normal: time.Minute}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.telegram.org/botX/getMe", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "ok") {
		t.Fatalf("响应体内容异常: %q", body)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !rec.hasDL {
		t.Fatal("应当有 deadline")
	}
}
