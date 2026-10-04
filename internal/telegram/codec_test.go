package telegram

import (
	"strings"
	"testing"

	tgbotapi "github.com/ijnkawakaze/telegram-bot-api"

	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
)

// TestTextWithLinks 验证从实体还原链接：客户端会把 [文字](url) 变成 text_link 实体，
// 只取 Message.Text 会丢掉 URL（真实踩过：欢迎语里的群规链接变成光秃秃的"点击阅读"）。
func TestTextWithLinks(t *testing.T) {
	plain := &tgbotapi.Message{Text: "欢迎{mention}"}
	if got := TextWithLinks(plain); got != "欢迎{mention}" {
		t.Fatalf("无实体时应原样返回，实际 %q", got)
	}
	msg := &tgbotapi.Message{
		Text: "建议阅读群公约：点击阅读",
		Entities: []tgbotapi.MessageEntity{{
			// 偏移以 UTF-16 码元计："建议阅读群公约："占 8 个码元，链接文字从 8 开始。
			Type: "text_link", Offset: 8, Length: 4, URL: "https://t.me/ArknightsEndfieldCN/37057",
		}},
	}
	want := "建议阅读群公约：[点击阅读](https://t.me/ArknightsEndfieldCN/37057)"
	if got := TextWithLinks(msg); got != want {
		t.Fatalf("链接还原失败：\n got %q\nwant %q", got, want)
	}
	// emoji 超出 BMP、占 2 个 UTF-16 码元，偏移必须换算正确。
	msg2 := &tgbotapi.Message{
		Text: "🌙群规：点击阅读",
		Entities: []tgbotapi.MessageEntity{{
			Type: "text_link", Offset: 5, Length: 4, URL: "https://t.me/x/1",
		}},
	}
	want2 := "🌙群规：[点击阅读](https://t.me/x/1)"
	if got := TextWithLinks(msg2); got != want2 {
		t.Fatalf("emoji 偏移换算失败：\n got %q\nwant %q", got, want2)
	}
}

// TestRenderCaptionRulesPlaceholder 验证 {rules} 渲染成可点击的 MarkdownV2 链接。
func TestRenderCaptionRulesPlaceholder(t *testing.T) {
	got := RenderCaption("欢迎{mention}\n建议阅读群公约：{rules}", 7, "漠伦",
		60, "https://t.me/ArknightsEndfieldCN/37057")
	if !strings.Contains(got, "[漠伦](tg://user?id=7)") {
		t.Fatalf("提及未渲染：%q", got)
	}
	if !strings.Contains(got, "[点击阅读](https://t.me/ArknightsEndfieldCN/37057)") {
		t.Fatalf("群规链接未渲染：%q", got)
	}
	if got := RenderCaption("欢迎{mention}{rules}", 7, "漠伦", 60, ""); strings.Contains(got, "{rules}") {
		t.Fatalf("无链接时不应保留占位符：%q", got)
	}
}

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
	got := RenderCaption("欢迎 {mention}，请在 {timeout} 秒内作答。", 12345, "a.b_c", 60, "")
	if !strings.Contains(got, "[a\\.b\\_c](tg://user?id=12345)") {
		t.Fatalf("提及未正确转义: %q", got)
	}
	if !strings.Contains(got, "60") {
		t.Fatalf("超时未替换: %q", got)
	}
	// 未知占位保持原样，方便运维发现拼错。
	unknown := RenderCaption("hi {nope}", 1, "x", 5, "")
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
	// 顺序：放行在左、封禁在右下角（危险动作放右下角，避免误触）。
	pass, err := Decode(grid[0][0].Data)
	if err != nil || pass.Kind != CallbackAdminPass || pass.SessionID != sessionID {
		t.Fatalf("放行按钮异常: %+v err=%v", pass, err)
	}
	ban, err := Decode(grid[0][1].Data)
	if err != nil || ban.Kind != CallbackAdminBan || ban.SessionID != sessionID {
		t.Fatalf("封禁按钮异常: %+v err=%v", ban, err)
	}
}

// TestChallengeKeyboardAppendsAdminRow 验证入群模式的题目键盘 = 选项 + 末行管理员键，
// 且两种回调数据互不串味（选项是 answer，末行是 admin）。
func TestChallengeKeyboardAppendsAdminRow(t *testing.T) {
	sessionID, err := domain.NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	options := []domain.Option{
		{Label: "甲", Token: "aaa111"}, {Label: "乙", Token: "bbb222"},
		{Label: "丙", Token: "ccc333"}, {Label: "丁", Token: "ddd444"},
	}
	grid := ChallengeKeyboard(options, 2, sessionID)
	if len(grid) != 3 { // 2 行选项 + 1 行管理员键
		t.Fatalf("题目键盘行数应为 3，实际 %d: %+v", len(grid), grid)
	}
	if len(grid[2]) != 2 || grid[2][0].Text != "✅ 放行" || grid[2][1].Text != "🚫 封禁" {
		t.Fatalf("末行应为管理员键，实际 %+v", grid[2])
	}
	// 选项行仍然是答案回调
	for i, row := range grid[:2] {
		for j, btn := range row {
			cb, err := Decode(btn.Data)
			if err != nil || cb.Kind != CallbackAnswer || cb.SessionID != sessionID {
				t.Fatalf("选项[%d][%d] 应是答案回调: %+v err=%v", i, j, cb, err)
			}
		}
	}
	// 管理员行不是答案回调（否则谁点"封禁"就可能被当成答题）
	cb, err := Decode(grid[2][1].Data)
	if err != nil || cb.Kind != CallbackAdminBan {
		t.Fatalf("末行应是管理员回调: %+v err=%v", cb, err)
	}
}
