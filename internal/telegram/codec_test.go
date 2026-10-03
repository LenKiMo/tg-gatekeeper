package telegram

import (
	"strings"
	"testing"

	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
)

// TestCallbackRoundTrip 验证按钮数据的编解码。
func TestCallbackRoundTrip(t *testing.T) {
	sessionID, err := domain.NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	token, err := domain.NewOptionToken()
	if err != nil {
		t.Fatal(err)
	}
	encoded := EncodeAnswer(sessionID, token)
	if len(encoded) > MaxCallbackBytes {
		t.Fatalf("callback_data 长度 %d 超过 Telegram 上限 %d", len(encoded), MaxCallbackBytes)
	}
	got, err := Decode(encoded)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if got.Kind != CallbackAnswer || got.SessionID != sessionID || got.Token != token {
		t.Fatalf("解码结果不符: %+v", got)
	}

	for _, ban := range []bool{false, true} {
		data := EncodeAdmin(sessionID, ban)
		parsed, err := Decode(data)
		if err != nil {
			t.Fatalf("解码管理员按钮失败: %v", err)
		}
		want := CallbackAdminPass
		if ban {
			want = CallbackAdminBan
		}
		if parsed.Kind != want || parsed.SessionID != sessionID {
			t.Fatalf("管理员按钮解码异常: %+v", parsed)
		}
	}
}

// TestCallbackRejectsGarbage 验证非法数据被拒绝（不能把别人的按钮当自己的题）。
func TestCallbackRejectsGarbage(t *testing.T) {
	cases := []string{
		"",
		"g1",
		"g1.",
		"g1.x.y",
		"g2.abcdefgh.abcdef",
		"v1.abcdefgh.abcdef",
		"g1.abcdefgh.abcdef.extra",
		strings.Repeat("a", 100),
		"g1.有中文.abcdef",
		"g1.abcdefgh.tok en",
	}
	for _, c := range cases {
		if _, err := Decode(c); err == nil {
			t.Errorf("非法 callback data %q 应当被拒绝", c)
		}
	}
}

// TestEscapeMarkdownV2 验证动态文本里的特殊字符全部转义（否则 sendMessage 会 400）。
func TestEscapeMarkdownV2(t *testing.T) {
	cases := map[string]string{
		"你好":          "你好",
		"1.5":         "1\\.5",
		"a_b":         "a\\_b",
		"*bold*":      "\\*bold\\*",
		"[链接](x)":     "\\[链接\\]\\(x\\)",
		"名字-甲":        "名字\\-甲",
		"a!b#c+d":     "a\\!b\\#c\\+d",
		"back\\slash": "back\\\\slash",
		"{a}":         "\\{a\\}",
	}
	for in, want := range cases {
		if got := EscapeMarkdownV2(in); got != want {
			t.Errorf("EscapeMarkdownV2(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestRenderCaptionEscapesUserInput 验证把昵称塞进模板后仍然安全。
func TestRenderCaptionEscapesUserInput(t *testing.T) {
	got := RenderCaption("欢迎 {mention}，请在 {timeout} 秒内作答。", 12345, "a.b_c", 60)
	if !strings.Contains(got, "[a\\.b\\_c](tg://user?id=12345)") {
		t.Fatalf("提及未正确转义: %q", got)
	}
	if !strings.Contains(got, "60") {
		t.Fatalf("超时未替换: %q", got)
	}
	// 未知占位保持原样，方便运维发现拼错。
	unknown := RenderCaption("hi {nope}", 1, "x", 5)
	if !strings.Contains(unknown, "{nope}") {
		t.Fatalf("未知占位应原样保留: %q", unknown)
	}
}

// TestOptionKeyboardLayout 验证按钮布局与数据来源。
func TestOptionKeyboardLayout(t *testing.T) {
	// token 长度必须与 domain.NewOptionToken 生成的一致（4 字节 base64url = 6 字符）。
	options := []domain.Option{
		{Token: "aaa111", Label: "甲"},
		{Token: "bbb222", Label: "乙"},
		{Token: "ccc333", Label: "丙"},
		{Token: "ddd444", Label: "丁"},
		{Token: "eee555", Label: "戊"},
	}
	sessionID, err := domain.NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	grid := OptionKeyboard(options, 2, sessionID)
	if len(grid) != 3 {
		t.Fatalf("行数 = %d，期望 3", len(grid))
	}
	if len(grid[2]) != 1 {
		t.Fatalf("最后一行按钮数 = %d，期望 1", len(grid[2]))
	}
	for i, row := range grid {
		for j, btn := range row {
			if btn.Text != options[i*2+j].Label {
				t.Errorf("按钮文本错位: %q", btn.Text)
			}
			cb, err := Decode(btn.Data)
			if err != nil {
				t.Fatalf("按钮数据非法: %v", err)
			}
			if cb.Token != options[i*2+j].Token || cb.SessionID != sessionID {
				t.Errorf("按钮数据与选项不匹配: %+v", cb)
			}
			if strings.Contains(btn.Data, btn.Text) {
				t.Errorf("按钮数据里不应包含明文标签: %q", btn.Data)
			}
		}
	}
}

// TestAdminKeyboard 验证管理员按钮。
func TestAdminKeyboard(t *testing.T) {
	sessionID, err := domain.NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	grid := AdminKeyboard(sessionID)
	if len(grid) != 1 || len(grid[0]) != 2 {
		t.Fatalf("管理员按钮布局异常: %+v", grid)
	}
	pass, err := Decode(grid[0][0].Data)
	if err != nil || pass.Kind != CallbackAdminPass || pass.SessionID != sessionID {
		t.Fatalf("放行按钮异常: %+v err=%v", pass, err)
	}
	ban, err := Decode(grid[0][1].Data)
	if err != nil || ban.Kind != CallbackAdminBan || ban.SessionID != sessionID {
		t.Fatalf("封禁按钮异常: %+v err=%v", ban, err)
	}
}
