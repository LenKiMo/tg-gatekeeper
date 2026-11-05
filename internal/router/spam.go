package router

// 结构信号识别：不查库、不调 API，只看 update 本身。
//
// 判定依据（都拿不到"广告文案"才成立的捷径，所以换话术不影响）：
//  1. 发送者是机器人（is_bot）且不是自己 —— 访客机器人（guest bots）以自身身份
//     在群里发言，它并不是群成员；
//  2. 消息带 via_bot —— 用户用内联机器人（inline mode）代发；
//  3. 普通成员的消息带内联键盘 —— 内联键盘只有机器人能附带，普通客户端造不出来。
//
// 白名单（moderation.via_bot_allowlist）用来放过 @gif 这类正常娱乐机器人。

import (
	"strings"

	tgbotapi "github.com/ijnkawakaze/telegram-bot-api"

	"github.com/LenKiMo/tg-gatekeeper/internal/gatekeeper"
)

// detectSpam 返回命中原因与取证字段；ok=false 表示不是机器人代发类消息。
func detectSpam(m *tgbotapi.Message, selfID int64, allowlist []string) (sig gatekeeper.SpamSignal, ok bool) {
	if m == nil || m.From == nil {
		return sig, false
	}
	sig = gatekeeper.SpamSignal{
		MessageID:   m.MessageID,
		UserID:      m.From.ID,
		DisplayName: m.From.FullName(),
	}
	if m.Chat != nil {
		sig.ChatID = m.Chat.ID
	}

	// 1. 发送者本身是机器人：访客机器人代发。自己发的消息在调用点已过滤，这里再兜一次。
	if m.From.IsBot {
		if m.From.ID == selfID {
			return sig, false
		}
		sig.Reason = gatekeeper.SpamReasonBotSender
		sig.SenderIsBot = true
		return sig, true
	}

	// 2. 内联机器人代发：via_bot 非空。白名单按用户名（小写、不带 @）比对。
	if m.ViaBot != nil {
		name := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(m.ViaBot.UserName), "@"))
		if !allowedBot(name, allowlist) {
			sig.Reason = gatekeeper.SpamReasonViaBot
			sig.ViaBotID = m.ViaBot.ID
			sig.ViaBotUsername = name
			sig.ButtonURLs = collectButtonURLs(m)
			return sig, true
		}
	}

	// 3. 普通成员的消息带内联键盘（只有机器人能附带）。
	if urls := collectButtonURLs(m); len(urls) > 0 {
		sig.Reason = gatekeeper.SpamReasonInlineKeyboard
		sig.ButtonURLs = urls
		return sig, true
	}
	return gatekeeper.SpamSignal{}, false
}

// allowedBot 判断机器人用户名是否在允许列表里。
func allowedBot(name string, allowlist []string) bool {
	if name == "" {
		return false
	}
	for _, a := range allowlist {
		if strings.EqualFold(strings.TrimPrefix(strings.TrimSpace(a), "@"), name) {
			return true
		}
	}
	return false
}

// collectButtonURLs 取消息内联键盘里的链接按钮（最多 3 个，取证够用）。
func collectButtonURLs(m *tgbotapi.Message) []string {
	if m.ReplyMarkup == nil || m.ReplyMarkup.InlineKeyboard == nil {
		return nil
	}
	var urls []string
	for _, row := range m.ReplyMarkup.InlineKeyboard {
		for _, btn := range row {
			if btn.URL == nil || *btn.URL == "" {
				continue
			}
			urls = append(urls, *btn.URL)
			if len(urls) >= 3 {
				return urls
			}
		}
	}
	return urls
}
