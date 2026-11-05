package gatekeeper

import (
	"context"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
)

// banReason 标明一次封禁的来由。它进审计，也决定要不要撤回对方的历史消息。
type banReason string

const (
	// banReasonSendFailed 题目发不出去（限流、网络、文案转义写错）。
	banReasonSendFailed banReason = "challenge_send_failed"
	// banReasonGenerateFailed 出不了题（题库为空/损坏、图片取不到）。
	banReasonGenerateFailed banReason = "challenge_generate_failed"
	// banReasonKick 自动踢出：答错、超时、没验证就发言。
	banReasonKick banReason = "kick"
	// banReasonAdmin 管理员在题目卡片/管理员卡片上点了"封禁"。
	banReasonAdmin banReason = "admin_ban"
)

// botFault 判断这次封禁是不是机器人自身故障造成的。这类封禁错不在用户，
// 不撤回对方的历史消息。
func (r banReason) botFault() bool {
	switch r {
	case banReasonSendFailed, banReasonGenerateFailed:
		return true
	}
	return false
}

// minTempBanSeconds 是定时封禁的最短时长。Telegram 把"距现在不足 30 秒"的到期时间
// 当成永久封禁；这里留一倍余量，容得下本机与 Telegram 之间的时钟偏差。
const minTempBanSeconds = 60

// tempBanUntil 把"封禁多少秒"换算成封禁到期时间（unix 秒）；seconds <= 0 表示永久，返回 0。
//
// 必须在真正调用封禁接口的那一刻才算：计划时算好的时间点到执行时可能已经过去，
// 而过去的到期时间等于永久封禁。
func tempBanUntil(seconds int64) int64 {
	if seconds <= 0 {
		return 0
	}
	if seconds < minTempBanSeconds {
		seconds = minTempBanSeconds
	}
	return time.Now().Unix() + seconds
}

// applyTempBan 是封禁的统一出口：发一条带到期时间的 Ban（Telegram 原生定时封禁），
// 到期由 Telegram 自己解除，不再另外安排解封任务。
//
// seconds 是封禁时长；不足 minTempBanSeconds 的按 minTempBanSeconds 算，<= 0 表示永久。
// 机器人自身故障导致的封禁（见 banReason.botFault）不撤回消息，其余按
// revoke_messages_on_ban 配置。
func (s *Service) applyTempBan(ctx context.Context, group domain.GroupConfig, userID, seconds int64, reason banReason) error {
	revoke := s.cfg.Gatekeeper.RevokeMessagesOnBan && !reason.botFault()
	return s.api.Ban(ctx, group.ChatID, userID, revoke, tempBanUntil(seconds))
}
