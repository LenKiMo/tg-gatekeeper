package gatekeeper

// 消息级拦截：成员在群里借机器人投放广告。
//
// Telegram 上有两种形态，肉眼都是"群里冒出一条带按钮的机器人消息"，但语义完全不同，
// 处置也必须分开：
//
//   - **访客机器人（guest bots，Bot API 10.0）**：用户在消息里 @ 一个不是群成员的
//     机器人，机器人用 `answerGuestQuery` **以自己身份**在群里回一句。消息的
//     `from` 是那个机器人，`guest_bot_caller_user` 是召唤它的真人。截图里的广告
//     就是这一种（客户端渲染成 `Xxx Bot 来自 <用户>`）。
//   - **内联机器人（inline mode）**：成员自己发消息、消息带 `via_bot`，`from` 仍是
//     成员本人。群里用 `@某Bot` 查资料/贴链接是**正常用法**，不该封人。
//
// 所以分界线是"发送者是谁"，不是"消息长得像不像广告"：
//   发送者是机器人且不是本群成员 → 访客机器人，按 `bot_message_action`（默认 delete）；
//   发送者是真人但带 via_bot      → 内联代发，按 `inline_bot_action`（默认 off）。
//
// 判定放在 router（纯结构、不查库不调 API），处置放在这里（要查身份、删消息、封人），
// 保持"路由不改业务状态、业务层不认 tgbotapi"的分层。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
)

// 命中原因。
const (
	// SpamReasonBotSender 消息发送者本身是机器人且不是本群成员（访客机器人代发）。
	SpamReasonBotSender = "bot_sender"
	// SpamReasonViaBot 成员自己发的消息，但由内联机器人渲染（via_bot 非空）。
	SpamReasonViaBot = "via_bot"
	// SpamReasonAdText 消息正文命中广告词。
	SpamReasonAdText = "ad_text"
)

// SpamSignal 是 router 提取出的拦截信号（业务层不依赖 tgbotapi）。
type SpamSignal struct {
	ChatID      int64
	UserID      int64
	DisplayName string
	MessageID   int64
	Reason      string
	// ViaBotID / ViaBotUsername：内联代发时是那个机器人；访客机器人时是机器人自身。
	ViaBotID       int64
	ViaBotUsername string
	// SenderIsBot 表示发送者本身是机器人（reason=bot_sender）。
	SenderIsBot bool
	// CallerID 是召唤访客机器人的真人（guest_bot_caller_user）：要封就封他，封机器人没意义。
	CallerID int64
	// ButtonURLs 是消息内联键盘里的链接（取证用，最多取前 3 个）。
	ButtonURLs []string
	// MatchedWord 只在 reason=ad_text 时有值。
	MatchedWord string
}

// ModerationEnabled 返回消息级拦截是否开启（router 用它决定要不要白查结构）。
func (s *Service) ModerationEnabled() bool { return s.cfg.Moderation.Enabled }

// AdWordMatch 用全局广告词表检查消息正文（不查群级词表：那需要读库，这里是
// 每条消息的热路径）。ad_text_action=off 时直接返回未命中，零开销。
func (s *Service) AdWordMatch(text string) (string, bool) {
	if s.cfg.Moderation.AdTextAction == "" || s.cfg.Moderation.AdTextAction == "off" {
		return "", false
	}
	if !s.cfg.AdFilter.Enabled || strings.TrimSpace(text) == "" {
		return "", false
	}
	return s.adFilter.Check(text, nil)
}

// OnSpamSignal 处置一条命中拦截信号的消息：删消息、按配置封人、写审计。
func (s *Service) OnSpamSignal(ctx context.Context, sig SpamSignal) error {
	m := s.cfg.Moderation
	if !m.Enabled {
		return nil
	}

	action, event := m.BotMessageAction, "spam.guest_bot"
	switch sig.Reason {
	case SpamReasonViaBot:
		action, event = m.InlineBotAction, "spam.inline_bot"
	case SpamReasonAdText:
		action, event = m.AdTextAction, "spam.ad_text"
	}
	if action == "" || action == "off" {
		return nil
	}

	// 看发送者的群内身份：群里自己拉进来的工具机器人（成员身份）不该被当成代发广告；
	// 管理员用 @gif 之类发消息也属正常（admin_exempt 可关掉）。
	status, statusErr := s.api.GetMemberStatus(ctx, sig.ChatID, sig.UserID)
	if statusErr != nil {
		status = "" // 查不到就按"非成员"处理，宁可拦不可漏
	}
	isAdmin := status == "administrator" || status == "creator"
	isMember := isAdmin || status == "member" || status == "restricted"

	if sig.SenderIsBot && isMember {
		s.audit(ctx, ports.AuditEvent{
			Event: event, ChatID: sig.ChatID, UserID: sig.UserID,
			DisplayName: sig.DisplayName, Result: "skip_bot_member",
			Extra: spamExtra(sig),
		})
		return nil
	}
	if m.AdminExempt && isAdmin {
		s.audit(ctx, ports.AuditEvent{
			Event: event, ChatID: sig.ChatID, UserID: sig.UserID,
			DisplayName: sig.DisplayName, Result: "skip_admin",
			Extra: spamExtra(sig),
		})
		return nil
	}

	// 删消息：失败不阻断后续动作，只记日志。
	if err := s.api.DeleteMessage(ctx, domain.MessageRef{ChatID: sig.ChatID, MessageID: sig.MessageID}); err != nil {
		s.log.Warn("拦截：删除消息失败", "chat_id", sig.ChatID, "message_id", sig.MessageID,
			"reason", sig.Reason, "err", err)
	}

	result := action
	if action == "ban" {
		// 访客机器人本身不是群成员，封不了；要封的是召唤它的人。
		target := sig.UserID
		if sig.SenderIsBot {
			target = sig.CallerID
		}
		if target == 0 {
			result = "delete"
		} else if err := s.api.Ban(ctx, sig.ChatID, target, s.cfg.Gatekeeper.RevokeMessagesOnBan, s.spamBanUntil()); err != nil {
			s.log.Warn("拦截：封禁失败", "chat_id", sig.ChatID, "user_id", target, "err", err)
			result = "delete"
		}
	}

	s.audit(ctx, ports.AuditEvent{
		Event: event, ChatID: sig.ChatID, UserID: sig.UserID,
		DisplayName: sig.DisplayName, Result: result, Extra: spamExtra(sig),
	})
	s.log.Warn("拦截机器人代发广告",
		"chat_id", sig.ChatID, "user_id", sig.UserID, "message_id", sig.MessageID,
		"reason", sig.Reason, "result", result,
		"via_bot", sig.ViaBotUsername, "sender_is_bot", sig.SenderIsBot, "caller_id", sig.CallerID)
	s.alert(ctx, sig, result)
	return nil
}

// spamBanUntil 返回封禁到期时间；0 = 永久。
func (s *Service) spamBanUntil() int64 {
	days := s.cfg.Moderation.BanDays
	if days <= 0 {
		return 0
	}
	return time.Now().Add(time.Duration(days) * 24 * time.Hour).Unix()
}

// alert 在配置了 alert_chat_id 时把这次拦截发到指定会话，方便集中看垃圾广告的来路。
func (s *Service) alert(ctx context.Context, sig SpamSignal, result string) {
	if s.cfg.Moderation.AlertChatID == 0 {
		return
	}
	who := sig.DisplayName
	if who == "" {
		who = fmt.Sprintf("user:%d", sig.UserID)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🧹 拦截广告\n群: %d\n发送者: %s (%d)\n类型: %s\n处置: %s", sig.ChatID, who, sig.UserID, sig.Reason, result)
	if sig.CallerID != 0 {
		fmt.Fprintf(&b, "\n召唤者: %d", sig.CallerID)
	}
	if sig.ViaBotUsername != "" {
		fmt.Fprintf(&b, "\n机器人: @%s", sig.ViaBotUsername)
	}
	if sig.MatchedWord != "" {
		fmt.Fprintf(&b, "\n命中词: %s", sig.MatchedWord)
	}
	if _, err := s.api.SendText(ctx, s.cfg.Moderation.AlertChatID, b.String(), false, nil); err != nil {
		s.log.Warn("拦截：告警发送失败", "alert_chat_id", s.cfg.Moderation.AlertChatID, "err", err)
	}
}

func spamExtra(sig SpamSignal) map[string]string {
	extra := map[string]string{"reason": sig.Reason}
	if sig.ViaBotID != 0 {
		extra["via_bot_id"] = fmt.Sprintf("%d", sig.ViaBotID)
	}
	if sig.ViaBotUsername != "" {
		extra["via_bot"] = sig.ViaBotUsername
	}
	if sig.SenderIsBot {
		extra["sender_is_bot"] = "true"
	}
	if sig.CallerID != 0 {
		extra["caller_id"] = fmt.Sprintf("%d", sig.CallerID)
	}
	if sig.MatchedWord != "" {
		extra["word"] = sig.MatchedWord
	}
	if len(sig.ButtonURLs) > 0 {
		extra["button_urls"] = strings.Join(sig.ButtonURLs, " ")
	}
	return extra
}
