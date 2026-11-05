package router

// 结构信号识别：不查库、不调 API，只看 update 本身。
//
// 分界线是"**发送者是谁**"，不是"消息长得像不像广告"——这两类在客户端看起来都是
// "一条带按钮的机器人消息"，但语义完全不同：
//
//  1. 发送者是机器人且不是自己 → **访客机器人**（guest bots，Bot API 10.0）：用户
//     在消息里 @ 一个非群成员的 bot，bot 以自身身份在群里回一句，`GuestBotCallerUser`
//     是那个真人。截图里的广告就是这一种（`Xxx Bot 来自 <用户>`）。
//  2. 发送者是真人、但消息带 `via_bot` → **内联机器人代发**（inline mode）。群里用
//     `@某Bot` 查资料/贴链接是正常用法，默认不拦（动作由 inline_bot_action 控制）。
//
// 内联键盘不作为独立判据：它只随机器人消息出现，两类在上面都已覆盖；单独拿它当信号
// 等于把正常内联机器人（多数会带按钮）也算广告——线上已因此误封过两个成员。
//
// 白名单（moderation.via_bot_allowlist）按用户名（小写、不带 @）放过指定机器人。

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
			return gatekeeper.SpamSignal{}, false
		}
		if sentOnBehalfOfChat(m) {
			return gatekeeper.SpamSignal{}, false
		}
		name := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(m.From.UserName), "@"))
		if allowedBot(name, allowlist) {
			return gatekeeper.SpamSignal{}, false
		}
		sig.Reason = gatekeeper.SpamReasonBotSender
		sig.SenderIsBot = true
		sig.ViaBotID = m.From.ID
		sig.ViaBotUsername = name
		if m.GuestBotCallerUser != nil {
			sig.CallerID = m.GuestBotCallerUser.ID
		}
		sig.ButtonURLs = collectButtonURLs(m)
		return sig, true
	}

	// 2. 真人发消息但由内联机器人渲染：默认不拦，交给 inline_bot_action 决定。
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
	return gatekeeper.SpamSignal{}, false
}

// Telegram 的占位发送者：消息以某个会话的身份发出时，from 为了向后兼容会填成它们。
const (
	groupAnonymousBotID int64 = 1087968824 // @GroupAnonymousBot：匿名管理员
	channelBotID        int64 = 136817688  // @Channel_Bot：以频道身份发言
)

// sentOnBehalfOfChat 判断这条"机器人发的"消息其实是以某个会话的身份发的：匿名管理员
// （sender_chat 是群自身）或"以频道身份发言"（sender_chat 是那个频道）。它们的 from
// 只是占位机器人，不是访客机器人——按访客机器人处置的话，匿名管理员的每条发言都会被删。
//
// 以 sender_chat 为准；占位账号 ID 作兜底，万一 sender_chat 缺失也能认出来。
// 带召唤者（guest_bot_caller_user）的一律仍按访客机器人算。
func sentOnBehalfOfChat(m *tgbotapi.Message) bool {
	if m.GuestBotCallerUser != nil {
		return false
	}
	return m.SenderChat != nil || m.From.ID == groupAnonymousBotID || m.From.ID == channelBotID
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
