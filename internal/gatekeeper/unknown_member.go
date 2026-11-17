package gatekeeper

// 懒触发复查：已入群成员的"首次发言"是唯一能低成本拿到 user_id 的时机。
//
// 为什么需要它：入群事件的送达并不可靠（getUpdates 缓冲溢出、relay 丢更新、停机窗口），
// 一旦丢了，那个人就绕过了全部入群检查、静默留在群里——而 bot 的账本只知道"事件来过"
// 的人。补这个缺口只有一个不扫群的办法：**在它发言时认出来**。
//
// 判定很便宜：内存里的 known 集合（启动时预热 + 运行时观察）先挡掉 99% 的发言，
// 只有真正没见过的 user_id 才走一次 SQLite 查询，仍然没有才调 getChat。
//
// 三种策略（gatekeeper.unknown_member_policy）：
//   off    不做任何事（默认；baseline 没建好之前开它会把老成员全判成可疑）
//   audit  只写审计 member.unknown，并把这个人记进基线（下次不再查）
//   ban    按上面的检查，简介命中广告词就按广告号处置；否则记进基线
//
// 基线（member_baseline）的来源有两种：MTProto 导出的全量名单（一次性），以及本机制
// 运行时观察到的成员。没有全量基线时，bot 上线前就在群的老成员第一次发言会被判可疑——
// 所以要先导入基线再开 audit/ban。

import (
	"context"

	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
)

// unknownMemberPolicy 返回归一化后的策略。
func (s *Service) unknownMemberPolicy() string {
	switch p := s.cfg.Gatekeeper.UnknownMemberPolicy; p {
	case "audit", "ban":
		return p
	default:
		return "off"
	}
}

// knownMember 判断某个 user 是否已被认定为"已知成员"（内存命中即返回）。
func (s *Service) knownMember(userID int64) bool {
	s.knownMu.RLock()
	_, ok := s.known[userID]
	s.knownMu.RUnlock()
	return ok
}

// markKnownMember 把 user 记为已知（内存 + 库，source 区分来路）。
func (s *Service) markKnownMember(ctx context.Context, chatID, userID int64, source string) {
	s.knownMu.Lock()
	if s.known == nil {
		s.known = make(map[int64]struct{}, 2048)
	}
	s.known[userID] = struct{}{}
	s.knownMu.Unlock()
	if s.baseline != nil {
		if _, err := s.baseline.UpsertBaseline(ctx, chatID, []int64{userID}, source); err != nil {
			s.log.Warn("写入成员基线失败", "user_id", userID, "err", err)
		}
	}
}

// preloadKnown 在启动时把基线灌进内存，避免每条群消息都查库。
func (s *Service) preloadKnown(ctx context.Context, chatID int64) {
	if s.baseline == nil {
		return
	}
	ids, err := s.baseline.ListBaseline(ctx, chatID, 20000)
	if err != nil {
		s.log.Warn("读取成员基线失败", "err", err)
		return
	}
	s.knownMu.Lock()
	if s.known == nil {
		s.known = make(map[int64]struct{}, len(ids)+256)
	}
	for _, id := range ids {
		s.known[id] = struct{}{}
	}
	s.knownMu.Unlock()
	if len(ids) > 0 {
		s.log.Info("成员基线已载入", "count", len(ids), "chat_id", chatID)
	}
}

// OnMemberSpoke 是"已入群成员发言"的钩子：只处理来路不明的人，其余零成本返回。
//
// 调用方应在消息热路径上同步调用它——它只做内存查表，慢的部分（数据库、getChat）
// 都在内部按"每人一次"的节奏发生。
func (s *Service) OnMemberSpoke(ctx context.Context, chatID, userID int64, displayName string) {
	policy := s.unknownMemberPolicy()
	if policy == "off" || userID == 0 {
		return
	}
	// 按群懒预热：第一次见到这个群的消息时灌一次基线，避免启动时扫描全部群。
	if !s.warmedChat(chatID) {
		s.preloadKnown(ctx, chatID)
		s.markChatWarmed(chatID)
	}
	if s.knownMember(userID) {
		return
	}

	// 二次确认：可能只是内存缓存还没热（重启后第一批消息）。
	if reg, ok := s.registry.(interface {
		HasPassedSession(context.Context, int64, int64) (bool, error)
	}); ok {
		if passed, err := reg.HasPassedSession(ctx, chatID, userID); err == nil && passed {
			s.markKnownMember(ctx, chatID, userID, "verified")
			return
		}
	}
	if s.baseline != nil {
		if has, err := s.baseline.HasBaseline(ctx, chatID, userID); err == nil && has {
			s.markKnownMember(ctx, chatID, userID, "baseline")
			return
		}
	}

	// 真的没见过：这个人要么是丢了入群事件进来的，要么是 bot 上线前的老成员。
	s.audit(ctx, ports.AuditEvent{
		Event: "member.unknown", ChatID: chatID, UserID: userID,
		DisplayName: displayName, Result: policy, Extra: map[string]string{
			"note": "既无验证通过记录、也不在成员基线里",
		},
	})
	if policy != "ban" {
		s.markKnownMember(ctx, chatID, userID, "observed")
		return
	}

	word, hit := s.bioHitFor(ctx, chatID, userID)
	if hit {
		days := s.cfg.Gatekeeper.AdWordBanDays
		s.noteProfile(ctx, domain.GroupConfig{ChatID: chatID}, userID, displayName,
			ports.UserProfile{}, profileDecisionAdBio, word, word)
		s.audit(ctx, ports.AuditEvent{
			Event: "verify.ad_rejected", ChatID: chatID, UserID: userID,
			Result: "ban", Mode: "join", DisplayName: displayName,
			Extra: map[string]string{"field": "bio_on_spoke", "word": word, "ban_days": itoa(int64(days))},
		})
		if err := s.api.Ban(ctx, chatID, userID, s.cfg.Gatekeeper.RevokeMessagesOnBan,
			adBanUntil(days)); err != nil {
			s.log.Warn("处置未知广告号失败", "user_id", userID, "err", err)
			s.audit(ctx, ports.AuditEvent{
				Event: "member.unknown_ban_failed", ChatID: chatID, UserID: userID,
				Result: "failed", ErrorClass: classify(err),
			})
			return // 不记基线：下一轮还有机会补
		}
		return
	}
	s.markKnownMember(ctx, chatID, userID, "observed")
}

// bioHitFor 读一次简介并判定（失败按"没命中"处理：宁可漏，不因机器人故障踢人）。
func (s *Service) bioHitFor(ctx context.Context, chatID, userID int64) (string, bool) {
	if !s.cfg.AdFilter.Enabled {
		return "", false
	}
	prof, err := s.api.GetUserProfile(ctx, userID)
	if err != nil {
		s.audit(ctx, ports.AuditEvent{
			Event: "verify.bio_recheck_failed", ChatID: chatID, UserID: userID,
			Result: "audit_continue", ErrorClass: classify(err),
		})
		return "", false
	}
	group, gerr := s.Group(ctx, chatID)
	words := []string(nil)
	if gerr == nil {
		words = group.AdWords
	}
	return s.adFilter.CheckBio(prof.Bio, words)
}

// warmedChat 判断某个群是否已经灌过基线。
func (s *Service) warmedChat(chatID int64) bool {
	s.knownMu.RLock()
	ok := s.warmed[chatID]
	s.knownMu.RUnlock()
	return ok
}

// markChatWarmed 记录某个群已灌过基线。
func (s *Service) markChatWarmed(chatID int64) {
	s.knownMu.Lock()
	if s.warmed == nil {
		s.warmed = make(map[int64]bool, 4)
	}
	s.warmed[chatID] = true
	s.knownMu.Unlock()
}
