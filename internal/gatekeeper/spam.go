package gatekeeper

// 消息级拦截：已通过验证的成员在群里借机器人投放广告。
//
// 这类广告在 Telegram 上有两种形态，业务上都表现为"普通用户的一条消息"：
//   - 内联机器人（inline mode）：用户自己发消息，但消息带 via_bot，正文与按钮
//     都是那个机器人的排版结果；
//   - 访客机器人（guest bots，Bot API 10.0）：用户 @ 一个非群成员的机器人，由
//     机器人以自己身份在群里回复，消息发送者是机器人（is_bot=true）。
//
// 两者都有一个普通用户无法伪造的特征：内联键盘（内联按钮）只有机器人能附带。
// 因此结构判定不依赖文案，比关键词稳得多——广告号换话术、换机器人、换链接都躲不开。
//
// 判定放在 router（纯结构、不查库不调 API），处置放在这里（要查管理员身份、
// 要删消息、要封人），保持"路由不改业务状态、业务层不认 tgbotapi"的分层。

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
	// SpamReasonViaBot 消息由内联机器人代发（via_bot 非空）。
	SpamReasonViaBot = "via_bot"
	// SpamReasonBotSender 消息发送者本身是机器人（访客机器人代发）。
	SpamReasonBotSender = "bot_sender"
	// SpamReasonInlineKeyboard 普通成员的消息带了内联键盘（只有机器人能做到）。
	SpamReasonInlineKeyboard = "inline_keyboard"
	// SpamReasonAdText 消息正文命中广告词（可选，默认关闭）。
	SpamReasonAdText = "ad_text"
)

// SpamSignal 是 router 提取出的拦截信号（业务层不依赖 tgbotapi）。
type SpamSignal struct {
	ChatID      int64
	UserID      int64
	DisplayName string
	MessageID   int64
	Reason      string
	// ViaBotID / ViaBotUsername 只在 reason=via_bot 时有值。
	ViaBotID       int64
	ViaBotUsername string
	// SenderIsBot 表示发送者本身是机器人（reason=bot_sender）。
	SenderIsBot bool
	// ButtonURLs 是消息内联键盘里的链接（取证用，最多取前 3 个）。
	ButtonURLs []string
	// MatchedWord 只在 reason=ad_text 时有值。
	MatchedWord string
}

// spamHit 是同一用户在一个窗口内的命中计数（用于"再犯即封"）。
type spamHit struct {
	count int
	first time.Time
}

// ModerationEnabled 返回消息级拦截是否开启（router 用它决定要不要白查结构）。
func (s *Service) ModerationEnabled() bool { return s.cfg.Moderation.Enabled }

// ModerationConfig 暴露拦截配置给 router（只读）。
func (s *Service) ModerationConfig() (botAction string, adTextAction string, allowlist []string) {
	m := s.cfg.Moderation
	return m.BotMessageAction, m.AdTextAction, m.ViaBotAllowlist
}

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
	action := m.BotMessageAction
	event := "spam.bot_message"
	if sig.Reason == SpamReasonAdText {
		action = m.AdTextAction
		event = "spam.ad_text"
	}
	if action == "" || action == "off" {
		return nil
	}

	// 命中原因之外还要看发送者的群内身份，两种情形要放过：
	//   - 访客机器人：**不是群成员**（getChatMember 会报错或返回 left/kicked）；
	//     群里的机器人成员则是自己拉进来的工具机器人，不该被当成代发广告；
	//   - 管理员/群主用 @gif 之类代发消息是正常行为（admin_exempt 可关掉）。
	status, statusErr := s.api.GetMemberStatus(ctx, sig.ChatID, sig.UserID)
	if statusErr != nil {
		status = "" // 查不到就按"非成员"处理，宁可拦不可漏
	}
	isAdmin := status == "administrator" || status == "creator"
	isMember := isAdmin || status == "member" || status == "restricted"
	// 访客机器人（非成员）封不了，只能删消息；真人号照常封禁。
	canBan := !(sig.Reason == SpamReasonBotSender && !isMember)

	if sig.Reason == SpamReasonBotSender && isMember {
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

	// 删消息：失败不阻断封禁（封禁才是止血的关键），只记日志。
	if err := s.api.DeleteMessage(ctx, domain.MessageRef{ChatID: sig.ChatID, MessageID: sig.MessageID}); err != nil {
		s.log.Warn("拦截：删除消息失败", "chat_id", sig.ChatID, "message_id", sig.MessageID,
			"reason", sig.Reason, "err", err)
	}

	result := action
	// 纯文本命中只删不封：正文词表是粗筛（"接单""出租"这类词在正常聊天里也会出现），
	// 误删一条消息的代价远小于误封一个真人。要封就显式把 ad_text_action 设成 ban。
	if action == "delete" && sig.Reason != SpamReasonAdText && s.escalate(sig) {
		result = "ban"
	}
	// 访客机器人不是群成员，封不了（banChatMember 会报错），只删消息。
	if result == "ban" && sig.UserID != 0 && canBan {
		if err := s.api.Ban(ctx, sig.ChatID, sig.UserID, s.cfg.Gatekeeper.RevokeMessagesOnBan, s.spamBanUntil()); err != nil {
			s.log.Warn("拦截：封禁失败", "chat_id", sig.ChatID, "user_id", sig.UserID, "err", err)
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
		"via_bot", sig.ViaBotUsername, "sender_is_bot", sig.SenderIsBot)
	s.alert(ctx, sig, result)
	return nil
}

// escalate 判断是否该把"删消息"升级成封禁：同一用户窗口内命中次数达标即升级。
func (s *Service) escalate(sig SpamSignal) bool {
	after := s.cfg.Moderation.RepeatBanAfter
	if after <= 0 || sig.UserID == 0 {
		return false
	}
	window := time.Duration(s.cfg.Moderation.RepeatWindowSec) * time.Second
	if window <= 0 {
		window = 10 * time.Minute
	}
	key := fmt.Sprintf("%d:%d", sig.ChatID, sig.UserID)
	now := time.Now()

	s.spamMu.Lock()
	defer s.spamMu.Unlock()
	if s.spamHits == nil {
		s.spamHits = make(map[string]spamHit)
	}
	// 顺手清理过期条目，避免长期运行内存无界增长。
	for k, h := range s.spamHits {
		if now.Sub(h.first) > window {
			delete(s.spamHits, k)
		}
	}
	h := s.spamHits[key]
	if h.first.IsZero() || now.Sub(h.first) > window {
		h = spamHit{first: now}
	}
	h.count++
	s.spamHits[key] = h
	if h.count < after {
		return false
	}
	delete(s.spamHits, key)
	return true
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
	fmt.Fprintf(&b, "🧹 拦截广告\n群: %d\n用户: %s (%d)\n类型: %s\n处置: %s", sig.ChatID, who, sig.UserID, sig.Reason, result)
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
	if sig.MatchedWord != "" {
		extra["word"] = sig.MatchedWord
	}
	if len(sig.ButtonURLs) > 0 {
		extra["button_urls"] = strings.Join(sig.ButtonURLs, " ")
	}
	return extra
}
