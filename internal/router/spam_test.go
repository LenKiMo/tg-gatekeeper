package router

import (
	"testing"

	tgbotapi "github.com/ijnkawakaze/telegram-bot-api"

	"github.com/LenKiMo/tg-gatekeeper/internal/gatekeeper"
)

// button 构造一个链接按钮（库里 URL 是 *string）。
func button(text, url string) tgbotapi.InlineKeyboardButton {
	u := url
	return tgbotapi.InlineKeyboardButton{Text: text, URL: &u}
}

// TestDetectSpam 覆盖两类代发形态与白名单。
//
// 截图里的广告是**访客机器人**：用户在消息里 @ 一个非群成员的 bot，bot 以自身身份
// 在群里回一句（客户端渲染成 `Xxx Bot 来自 <用户>`），消息的 from 是机器人本身。
// 而成员自己用 `@某Bot` 内联发消息时 from 仍是本人——两者必须区分，否则会误封正常用户。
func TestDetectSpam(t *testing.T) {
	const self = int64(999)
	cases := []struct {
		name      string
		msg       *tgbotapi.Message
		allow     []string
		wantOK    bool
		wantCause string
	}{
		{
			name: "访客机器人以自身身份发言（from.is_bot + 召唤者）",
			msg: &tgbotapi.Message{
				MessageID: 6, Chat: &tgbotapi.Chat{ID: -100},
				From:               &tgbotapi.User{ID: 556, UserName: "Dhm_a46ff8d7bot", IsBot: true},
				GuestBotCallerUser: &tgbotapi.User{ID: 42, FirstName: "Shawn"},
			},
			wantOK: true, wantCause: gatekeeper.SpamReasonBotSender,
		},
		{
			name: "内联机器人代发（真人发送 + via_bot）",
			msg: &tgbotapi.Message{
				MessageID: 5, Chat: &tgbotapi.Chat{ID: -100},
				From:   &tgbotapi.User{ID: 42, FirstName: "Shawn"},
				ViaBot: &tgbotapi.User{ID: 555, UserName: "cvevthbot", IsBot: true},
			},
			wantOK: true, wantCause: gatekeeper.SpamReasonViaBot,
		},
		{
			name: "成员消息带内联键盘但不是 via_bot：不拦（原生客户端造不出来，属异常但不动）",
			msg: &tgbotapi.Message{
				MessageID: 7, Chat: &tgbotapi.Chat{ID: -100},
				From: &tgbotapi.User{ID: 43, FirstName: "某人"},
				ReplyMarkup: &tgbotapi.InlineKeyboardMarkup{
					InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
						{button("自助购买", "https://spam.example/buy")},
					},
				},
			},
			wantOK: false,
		},
		{
			name: "内联白名单（@gif）放行",
			msg: &tgbotapi.Message{
				MessageID: 8, Chat: &tgbotapi.Chat{ID: -100},
				From:   &tgbotapi.User{ID: 44, FirstName: "正常人"},
				ViaBot: &tgbotapi.User{ID: 111, UserName: "gif", IsBot: true},
			},
			allow:  []string{"gif"},
			wantOK: false,
		},
		{
			name: "访客机器人白名单（按机器人自身用户名）放行",
			msg: &tgbotapi.Message{
				MessageID: 12, Chat: &tgbotapi.Chat{ID: -100},
				From: &tgbotapi.User{ID: 557, UserName: "cvEuthBot", IsBot: true},
			},
			allow:  []string{"cveuthbot"},
			wantOK: false,
		},
		{
			name: "普通文本消息不拦",
			msg: &tgbotapi.Message{
				MessageID: 9, Chat: &tgbotapi.Chat{ID: -100},
				From: &tgbotapi.User{ID: 45, FirstName: "正常人"}, Text: "大家好",
			},
			wantOK: false,
		},
		{
			name: "自己发的消息不拦",
			msg: &tgbotapi.Message{
				MessageID: 10, Chat: &tgbotapi.Chat{ID: -100},
				From: &tgbotapi.User{ID: self, IsBot: true},
			},
			wantOK: false,
		},
	}
	for _, c := range cases {
		sig, ok := detectSpam(c.msg, self, c.allow)
		if ok != c.wantOK {
			t.Fatalf("%s: ok=%v，期望 %v", c.name, ok, c.wantOK)
		}
		if ok && sig.Reason != c.wantCause {
			t.Fatalf("%s: reason=%q，期望 %q", c.name, sig.Reason, c.wantCause)
		}
		if ok && sig.MessageID != c.msg.MessageID {
			t.Fatalf("%s: 取证字段 message_id 丢了", c.name)
		}
	}
}

// TestDetectSpamGuestBotCaller 访客机器人消息要带出召唤者 ID（封人要封这个真人）。
func TestDetectSpamGuestBotCaller(t *testing.T) {
	sig, ok := detectSpam(&tgbotapi.Message{
		MessageID: 13, Chat: &tgbotapi.Chat{ID: -100},
		From:               &tgbotapi.User{ID: 556, UserName: "adbot", IsBot: true},
		GuestBotCallerUser: &tgbotapi.User{ID: 42, FirstName: "Shawn"},
	}, 999, nil)
	if !ok || sig.CallerID != 42 || !sig.SenderIsBot || sig.ViaBotUsername != "adbot" {
		t.Fatalf("召唤者/机器人取证不对: %+v ok=%v", sig, ok)
	}
}

// TestDetectSpamKeepsEvidence 取证字段要能落审计：via_bot 用户名、按钮链接。
func TestDetectSpamKeepsEvidence(t *testing.T) {
	msg := &tgbotapi.Message{
		MessageID: 11, Chat: &tgbotapi.Chat{ID: -100},
		From:               &tgbotapi.User{ID: 556, UserName: "Dhm_6e1e687abot", IsBot: true},
		GuestBotCallerUser: &tgbotapi.User{ID: 42, FirstName: "Shawn"},
		ReplyMarkup: &tgbotapi.InlineKeyboardMarkup{
			InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
				{button("海外账号自助购买", "https://gpd.example/buy")},
				{button("补货通知", "https://gpd.example/ch")},
			},
		},
	}
	sig, ok := detectSpam(msg, 999, nil)
	if !ok {
		t.Fatal("应命中")
	}
	if sig.ViaBotUsername != "dhm_6e1e687abot" || sig.ViaBotID != 556 {
		t.Fatalf("机器人取证不对: %+v", sig)
	}
	if len(sig.ButtonURLs) != 2 || sig.ButtonURLs[0] != "https://gpd.example/buy" {
		t.Fatalf("按钮链接取证不对: %+v", sig.ButtonURLs)
	}
}
