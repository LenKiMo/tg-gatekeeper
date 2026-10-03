package gatekeeper

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/challenge"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/image"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
	"github.com/LenKiMo/tg-gatekeeper/internal/telegram"
)

// JoinRequest 是一条入群申请。
type JoinRequest struct {
	ChatID      int64
	UserID      int64
	UserChatID  int64
	DisplayName string
	Bio         string
	UpdateID    int
}

// CallbackEvent 是一次按钮点击。
type CallbackEvent struct {
	ID          string
	ChatID      int64
	UserID      int64
	DisplayName string
	Data        string
}

// ---------------------------------------------------------------- 普通入群

// OnMembersJoined 处理一次"有人入群"事件（可能是多个成员）。
func (s *Service) OnMembersJoined(ctx context.Context, chatID int64, updateID int, members []Member) error {
	group, err := s.Group(ctx, chatID)
	if err != nil {
		return err
	}
	for _, m := range members {
		if err := s.verifyNewMember(ctx, group, m, updateID); err != nil {
			s.log.Warn("处理入群失败", "chat_id", chatID, "user_id", m.UserID, "err", err)
		}
	}
	return nil
}

func (s *Service) verifyNewMember(ctx context.Context, group domain.GroupConfig, m Member, updateID int) error {
	if m.IsBot && s.cfg.Gatekeeper.BotMemberPolicy == "allow" {
		s.auditSkip(ctx, group, m, "bot")
		return nil
	}
	if !group.Enabled {
		s.auditSkip(ctx, group, m, "group_disabled")
		return nil
	}
	if group.Mode != domain.VerifyModeJoin {
		// 申请模式下成员已经由 approve 流程控制，这里无需再验证。
		s.auditSkip(ctx, group, m, "mode_request")
		return nil
	}
	if s.cfg.Gatekeeper.AdminMemberPolicy == "allow" {
		if isAdmin, err := s.api.IsAdminWithPermissions(ctx, group.ChatID, m.UserID, ports.AdminCanRestrictMembers); err == nil && isAdmin {
			s.auditSkip(ctx, group, m, "admin")
			return nil
		}
	}
	if m.InvitedBy != 0 && s.cfg.Gatekeeper.InvitedMemberPolicy == "allow" {
		s.auditSkip(ctx, group, m, "invited")
		return nil
	}

	// 广告词检查：先看昵称，再看简介（简介失败按配置策略处理）。
	if word, hit := s.adFilter.Check(m.DisplayName, group.AdWords); hit {
		return s.rejectByFilter(ctx, group, m, word, "display_name")
	}
	if s.cfg.AdFilter.Enabled {
		bio, err := s.api.GetBio(ctx, m.UserID)
		switch {
		case err == nil:
			if word, hit := s.adFilter.Check(bio, group.AdWords); hit {
				return s.rejectByFilter(ctx, group, m, word, "bio")
			}
		case s.cfg.AdFilter.BioLookupErrorPolicy == "deny":
			s.audit(ctx, ports.AuditEvent{
				Event: "verify.bio_lookup_failed", ChatID: group.ChatID, UserID: m.UserID,
				Result: "deny", ErrorClass: classify(err), Mode: string(group.Mode),
			})
			return s.rejectByFilter(ctx, group, m, "", "bio_unavailable")
		default:
			s.audit(ctx, ports.AuditEvent{
				Event: "verify.bio_lookup_failed", ChatID: group.ChatID, UserID: m.UserID,
				Result: "audit_continue", ErrorClass: classify(err), Mode: string(group.Mode),
			})
		}
	}

	sess, err := s.startVerification(ctx, group, m.UserID, m.DisplayName, domain.SessionJoin, domain.MessageRef{}, updateID)
	if err != nil {
		return err
	}
	if sess.ID != "" {
		s.audit(ctx, ports.AuditEvent{
			Event: "verify.started", ChatID: group.ChatID, UserID: m.UserID,
			SessionID: sess.ID, Mode: string(domain.SessionJoin),
			NewState: string(domain.StatePending), Result: "pending",
			DatasetRevision: s.DatasetRevision(), DisplayName: m.DisplayName,
		})
	}
	return nil
}

// ---------------------------------------------------------------- 入群申请

// OnJoinRequest 处理入群申请：先做广告词检查，再私聊出题并给管理员一张处置卡片。
func (s *Service) OnJoinRequest(ctx context.Context, req JoinRequest) error {
	group, err := s.Group(ctx, req.ChatID)
	if err != nil {
		return err
	}
	if !group.Enabled || group.Mode != domain.VerifyModeRequest {
		return nil
	}
	if word, hit := s.adFilter.Check(req.DisplayName, group.AdWords); hit {
		s.audit(ctx, ports.AuditEvent{
			Event: "verify.ad_rejected", ChatID: req.ChatID, UserID: req.UserID,
			Result: "decline", Mode: string(domain.SessionRequest), Extra: map[string]string{"field": "display_name", "word": word},
		})
		return s.declineRequest(ctx, req.ChatID, req.UserID)
	}
	if s.cfg.AdFilter.Enabled {
		bio := req.Bio
		if bio == "" {
			if got, err := s.api.GetBio(ctx, req.UserID); err == nil {
				bio = got
			}
		}
		if word, hit := s.adFilter.Check(bio, group.AdWords); hit {
			s.audit(ctx, ports.AuditEvent{
				Event: "verify.ad_rejected", ChatID: req.ChatID, UserID: req.UserID,
				Result: "decline", Mode: string(domain.SessionRequest), Extra: map[string]string{"field": "bio", "word": word},
			})
			return s.declineRequest(ctx, req.ChatID, req.UserID)
		}
	}

	userChat := req.UserChatID
	if userChat == 0 {
		userChat = req.UserID
	}
	sess, err := s.startVerification(ctx, group, req.UserID, req.DisplayName, domain.SessionRequest, domain.MessageRef{},
		req.UpdateID, withUserChat(userChat))
	if err != nil {
		// 私聊发不出去（用户没点过 bot / 屏蔽了）：直接拒绝，避免申请悬空。
		s.audit(ctx, ports.AuditEvent{
			Event: "verify.request_failed", ChatID: req.ChatID, UserID: req.UserID,
			Result: "decline", ErrorClass: classify(err), Mode: string(domain.SessionRequest),
		})
		return s.declineRequest(ctx, req.ChatID, req.UserID)
	}
	if sess.ID != "" {
		s.audit(ctx, ports.AuditEvent{
			Event: "verify.started", ChatID: req.ChatID, UserID: req.UserID, SessionID: sess.ID,
			Mode: string(domain.SessionRequest), NewState: string(domain.StatePending),
			Result: "pending", DatasetRevision: s.DatasetRevision(), DisplayName: req.DisplayName,
		})
	}
	return nil
}

// ---------------------------------------------------------------- 按钮回调

// OnCallback 处理用户答题与管理员处置按钮。
func (s *Service) OnCallback(ctx context.Context, cb CallbackEvent) error {
	parsed, err := telegram.Decode(cb.Data)
	if err != nil {
		s.audit(ctx, ports.AuditEvent{
			Event: "callback.invalid", ChatID: cb.ChatID, UserID: cb.UserID,
			Result: "ignored", ErrorClass: "bad_callback",
		})
		return nil
	}
	session, err := s.registry.GetByID(ctx, parsed.SessionID)
	if err != nil {
		// 过期/已终结的按钮：给个轻提示，不改任何状态。
		if s.cfg.Gatekeeper.StaleCallbackAlert {
			_ = s.api.AnswerCallback(ctx, cb.ID, "这道题已经结束了", true)
		}
		s.audit(ctx, ports.AuditEvent{
			Event: "callback.stale", ChatID: cb.ChatID, UserID: cb.UserID, SessionID: parsed.SessionID,
			Result: "ignored",
		})
		return nil
	}
	group, err := s.Group(ctx, session.Key.ChatID)
	if err != nil {
		return err
	}

	switch parsed.Kind {
	case telegram.CallbackAnswer:
		if cb.UserID != session.Key.UserID {
			_ = s.api.AnswerCallback(ctx, cb.ID, "这不是你的题目", true)
			s.audit(ctx, ports.AuditEvent{
				Event: "callback.wrong_actor", ChatID: session.Key.ChatID, UserID: session.Key.UserID,
				ActorID: cb.UserID, SessionID: session.ID, Result: "ignored",
			})
			return nil
		}
		at := time.Now()
		if at.After(session.ExpiresAt) {
			_ = s.api.AnswerCallback(ctx, cb.ID, "时间已到", true)
		} else if session.IsCorrectToken(parsed.Token) {
			_ = s.api.AnswerCallback(ctx, cb.ID, "回答正确", false)
		} else {
			text := "回答错误"
			_ = s.api.AnswerCallback(ctx, cb.ID, text, s.cfg.Gatekeeper.AnswerWrongAlert)
		}
		res, err := s.claimByID(ctx, session, domain.Event{
			Kind: domain.EventAnswer, SessionID: session.ID, ActorID: cb.UserID,
			OptionToken: parsed.Token, At: at,
		})
		return s.finishClaim(ctx, group, res, err, "callback.answer")

	case telegram.CallbackAdminPass, telegram.CallbackAdminBan:
		isAdmin, err := s.api.IsAdminWithPermissions(ctx, session.Key.ChatID, cb.UserID, ports.AdminCanRestrictMembers)
		if err != nil || !isAdmin {
			_ = s.api.AnswerCallback(ctx, cb.ID, "只有管理员可以操作", true)
			s.audit(ctx, ports.AuditEvent{
				Event: "callback.not_admin", ChatID: session.Key.ChatID, UserID: session.Key.UserID,
				ActorID: cb.UserID, SessionID: session.ID, Result: "ignored",
			})
			return nil
		}
		kind := domain.EventAdminPass
		if parsed.Kind == telegram.CallbackAdminBan {
			kind = domain.EventAdminBan
		}
		_ = s.api.AnswerCallback(ctx, cb.ID, "已处理", false)
		res, err := s.claimByID(ctx, session, domain.Event{
			Kind: kind, SessionID: session.ID, ActorID: cb.UserID, At: time.Now(),
		})
		return s.finishClaim(ctx, group, res, err, "callback.admin")
	}
	return nil
}

// IsCorrectToken 判断 token 是否命中正确答案（内存比较，只读）。
func (s *Service) isCorrectToken(sess domain.Session, token string) bool {
	return domain.MatchesHash(token, sess.CorrectTokenHash)
}

// ---------------------------------------------------------------- 待验证发言

// OnGroupMessage 处理群内消息。返回 true 表示这条消息属于"待验证用户发言"并被处置，
// 调用方不应再把它当作命令处理。
func (s *Service) OnGroupMessage(ctx context.Context, chatID, userID, messageID int64, displayName string) (bool, error) {
	if !s.cfg.Gatekeeper.BlockPendingMessages {
		return false, nil
	}
	key := domain.SessionKey{ChatID: chatID, UserID: userID}
	sess, err := s.registry.Get(ctx, key)
	if err != nil {
		return false, nil
	}
	group, gerr := s.Group(ctx, chatID)
	if gerr != nil {
		return false, gerr
	}
	res, err := s.registry.Claim(ctx, key, domain.Event{
		Kind: domain.EventSpoke, SessionID: sess.ID, ActorID: userID,
		Message: domain.MessageRef{ChatID: chatID, MessageID: messageID},
		At:      time.Now(),
	}, s.effectPlan(group, ""))
	if err := s.finishClaim(ctx, group, res, err, "verify.spoke"); err != nil {
		return true, err
	}
	return true, nil
}

// ---------------------------------------------------------------- 退群

// OnMemberLeft 处理成员退群：作废会话并清理题目消息。
func (s *Service) OnMemberLeft(ctx context.Context, chatID, userID int64) error {
	key := domain.SessionKey{ChatID: chatID, UserID: userID}
	sess, err := s.registry.Get(ctx, key)
	if err != nil {
		return nil
	}
	group, err := s.Group(ctx, chatID)
	if err != nil {
		return err
	}
	res, err := s.registry.Claim(ctx, key, domain.Event{
		Kind: domain.EventLeft, SessionID: sess.ID, ActorID: userID, At: time.Now(),
	}, s.effectPlan(group, ""))
	return s.finishClaim(ctx, group, res, err, "verify.left")
}

// ---------------------------------------------------------------- 启动验证

type startOption func(*startOpts)

type startOpts struct {
	userChatID   int64
	joinMessage  domain.MessageRef
	skipRestrict bool
}

func withUserChat(id int64) startOption {
	return func(o *startOpts) { o.userChatID = id }
}

func withJoinMessage(ref domain.MessageRef) startOption {
	return func(o *startOpts) { o.joinMessage = ref }
}

// startVerification 生成题目、限制用户、发题并登记会话。
func (s *Service) startVerification(ctx context.Context, group domain.GroupConfig, userID int64,
	displayName string, mode domain.SessionMode, joinMessage domain.MessageRef, updateID int, opts ...startOption) (domain.Session, error) {

	o := startOpts{joinMessage: joinMessage}
	for _, fn := range opts {
		fn(&o)
	}

	gen := s.Generator()
	if gen == nil {
		return domain.Session{}, errors.New("题库尚未加载")
	}
	built, err := s.generate(ctx, gen, mode)
	if err != nil {
		return domain.Session{}, err
	}
	ch := built.Challenge

	sessionID, err := domain.NewSessionID()
	if err != nil {
		return domain.Session{}, err
	}
	now := time.Now()
	session := domain.Session{
		ID:                sessionID,
		Key:               domain.SessionKey{ChatID: group.ChatID, UserID: userID},
		UpdateID:          updateID,
		Mode:              mode,
		State:             domain.StatePreparing,
		Options:           ch.Options,
		CorrectTokenHash:  ch.CorrectTokenHash,
		DatasetRevision:   ch.Revision,
		JoinMessage:       o.joinMessage,
		RequestUserChatID: o.userChatID,
		StartedAt:         now,
		ExpiresAt:         now.Add(s.cfg.VerifyTimeout()),
		Version:           1,
	}

	begin, err := s.registry.Begin(ctx, session)
	if err != nil {
		return domain.Session{}, err
	}
	if begin.Duplicate {
		return domain.Session{}, nil // 同一条 update 被重复投递，幂等返回
	}
	session = begin.Session
	if begin.Replaced != nil {
		s.audit(ctx, ports.AuditEvent{
			Event: "session.replaced", ChatID: group.ChatID, UserID: userID,
			SessionID: begin.Replaced.ID, NewState: string(domain.StateCancelled),
			Result: "replaced", Mode: string(mode),
		})
	}

	// 普通入群模式：先禁言再出题，避免用户在题目发出前刷屏。
	if mode == domain.SessionJoin {
		if err := s.api.RestrictNoMessages(ctx, group.ChatID, userID); err != nil {
			s.audit(ctx, ports.AuditEvent{
				Event: "verify.restrict_failed", ChatID: group.ChatID, UserID: userID,
				SessionID: session.ID, Result: "failed", ErrorClass: classify(err), Mode: string(mode),
			})
			// 限制失败（常见原因：对方是管理员/创建者）→ 按"跳过验证"处理，避免误踢管理员。
			_, _ = s.registry.Claim(ctx, session.Key, domain.Event{
				Kind: domain.EventCancel, SessionID: session.ID, At: time.Now(),
			}, nil)
			return domain.Session{}, nil
		}
	}

	target := group.ChatID
	captionTemplate := s.cfg.Gatekeeper.CaptionJoin
	if mode == domain.SessionRequest {
		target = o.userChatID
		captionTemplate = s.cfg.Gatekeeper.CaptionRequest
	}
	caption := telegram.RenderCaption(captionTemplate, userID, displayName, s.cfg.Gatekeeper.TimeoutSeconds)
	buttons := telegram.OptionKeyboard(ch.Options, s.cfg.Gatekeeper.ButtonColumns, session.ID)

	ref, err := s.api.SendChallenge(ctx, target, built.Image, caption, buttons)
	if err != nil {
		// 题目发不出去：把会话作废，并按失败策略处置。
		_, _ = s.registry.Claim(ctx, session.Key, domain.Event{
			Kind: domain.EventCancel, SessionID: session.ID, At: time.Now(),
		}, nil)
		if mode == domain.SessionJoin {
			if s.cfg.Gatekeeper.FailurePolicy == "allow" {
				_ = s.api.RestorePermissions(ctx, group.ChatID, userID)
			} else {
				_ = s.api.Ban(ctx, group.ChatID, userID, s.cfg.Gatekeeper.RevokeMessagesOnBan)
			}
		}
		return domain.Session{}, fmt.Errorf("发送题目失败: %w", err)
	}

	attached, err := s.registry.AttachMessages(ctx, session.Key, session.ID, ref, domain.MessageRef{},
		now.Add(s.cfg.VerifyTimeout()), session.Version)
	if err != nil {
		// 会话在发题期间被替换：把这张多余的图删掉。
		_ = s.api.DeleteMessage(ctx, ref)
		if errors.Is(err, ports.ErrConflict) || errors.Is(err, ports.ErrNotFound) {
			return domain.Session{}, nil
		}
		return domain.Session{}, err
	}

	// 申请模式：额外在群里发一张管理员处置卡片。
	if mode == domain.SessionRequest {
		text := telegram.RenderCaption(s.cfg.Gatekeeper.AdminCardText, userID, displayName, s.cfg.Gatekeeper.TimeoutSeconds)
		cardRef, err := s.api.SendText(ctx, group.ChatID, text, true, telegram.AdminKeyboard(session.ID))
		if err == nil && cardRef.Valid() {
			if upd, aerr := s.registry.AttachMessages(ctx, session.Key, session.ID, ref, cardRef,
				attached.ExpiresAt, attached.Version); aerr == nil {
				attached = upd
			}
		}
	}
	return attached, nil
}

// generate 抽题；图片解析失败时换条目重试，避免单张坏图卡死整个验证。
func (s *Service) generate(ctx context.Context, gen *challenge.Generator, mode domain.SessionMode) (*generated, error) {
	attempts := s.cfg.Gatekeeper.MaxImageAttempts
	if attempts <= 0 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		ch, err := gen.Next(challenge.Options{
			OptionCount:    s.cfg.Gatekeeper.OptionCount,
			MinOptionCount: s.cfg.Gatekeeper.MinOptionCount,
			ShortagePolicy: s.cfg.Gatekeeper.ShortagePolicy,
			Strategy:       s.cfg.Gatekeeper.Difficulty.Strategy,
			DifficultyBand: s.cfg.Gatekeeper.Difficulty.Band(),
		})
		if err != nil {
			return nil, err
		}
		img, err := s.images.Resolve(ctx, ch.Entry.ImageRef)
		if err != nil {
			lastErr = err
			s.audit(ctx, ports.AuditEvent{
				Event: "image.resolve_failed", Result: "retry",
				ErrorClass: classify(err), Extra: map[string]string{"entry_id": ch.Entry.ID},
			})
			continue
		}
		return &generated{Challenge: ch, Image: img}, nil
	}
	if lastErr == nil {
		lastErr = errors.New("没有可用的图片")
	}
	return nil, fmt.Errorf("%w: %v", image.ErrImage, lastErr)
}

// rejectByFilter 因广告词拒绝入群。
func (s *Service) rejectByFilter(ctx context.Context, group domain.GroupConfig, m Member, word, field string) error {
	extra := map[string]string{"field": field}
	if word != "" {
		extra["word"] = word
	}
	s.audit(ctx, ports.AuditEvent{
		Event: "verify.ad_rejected", ChatID: group.ChatID, UserID: m.UserID,
		Result: "ban", Mode: string(group.Mode), DisplayName: m.DisplayName, Extra: extra,
	})
	if group.Mode == domain.VerifyModeRequest {
		return s.declineRequest(ctx, group.ChatID, m.UserID)
	}
	// 广告词拦截按"永久封禁"处理：这类账号通常就是广告号；管理员可在群里手动解封。
	return s.api.Ban(ctx, group.ChatID, m.UserID, s.cfg.Gatekeeper.RevokeMessagesOnBan)
}

func (s *Service) declineRequest(ctx context.Context, chatID, userID int64) error {
	return s.api.DeclineJoinRequest(ctx, chatID, userID)
}

// auditSkip 记录"跳过验证"的原因。
func (s *Service) auditSkip(ctx context.Context, group domain.GroupConfig, m Member, reason string) {
	s.audit(ctx, ports.AuditEvent{
		Event: "verify.skipped", ChatID: group.ChatID, UserID: m.UserID,
		Result: reason, Mode: string(group.Mode), DisplayName: m.DisplayName,
		NewState: string(domain.StateSkipped),
	})
}

func (s *Service) audit(ctx context.Context, ev ports.AuditEvent) {
	if s.auditLog == nil {
		return
	}
	s.auditLog.Log(ctx, ev)
}

// finishClaim 在 Claim 之后统一记录审计与错误。
func (s *Service) finishClaim(ctx context.Context, group domain.GroupConfig, res ports.ClaimResult, err error, event string) error {
	if err != nil {
		s.audit(ctx, ports.AuditEvent{
			Event: event + ".error", Result: "error", ErrorClass: classify(err),
			ChatID: group.ChatID, Mode: string(group.Mode),
		})
		return err
	}
	if !res.Won {
		s.audit(ctx, ports.AuditEvent{
			Event: event + ".lost", Result: res.Reason, ChatID: group.ChatID,
			UserID: res.Before.Key.UserID, SessionID: res.Before.ID,
			OldState: string(res.Before.State), NewState: string(res.After.State),
			Mode: string(res.Before.Mode),
		})
		return nil
	}
	s.audit(ctx, ports.AuditEvent{
		Event: event, Result: "won", ChatID: group.ChatID, UserID: res.Before.Key.UserID,
		SessionID: res.Before.ID, OldState: string(res.Before.State), NewState: string(res.After.State),
		Mode: string(res.Before.Mode), DatasetRevision: res.Before.DatasetRevision,
	})
	return nil
}

// claimByID 用 session id 精确抢占终态（申请模式的私聊按钮、超时扫描都需要）。
func (s *Service) claimByID(ctx context.Context, sess domain.Session, ev domain.Event) (ports.ClaimResult, error) {
	group, err := s.Group(ctx, sess.Key.ChatID)
	if err != nil {
		return ports.ClaimResult{}, err
	}
	return s.registry.Claim(ctx, sess.Key, ev, s.effectPlan(group, ""))
}

// classify 把错误归类为便于告警的短标签。
func classify(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline"):
		return "timeout"
	case strings.Contains(msg, "403"), strings.Contains(msg, "forbidden"), strings.Contains(msg, "not enough rights"):
		return "forbidden"
	case strings.Contains(msg, "429"), strings.Contains(msg, "too many requests"):
		return "rate_limited"
	case strings.Contains(msg, "400"), strings.Contains(msg, "bad request"):
		return "bad_request"
	case strings.Contains(msg, "context canceled"):
		return "canceled"
	default:
		return "other"
	}
}

// generated 是"题目 + 已处理好的图片"。
type generated struct {
	Challenge *challenge.Generated
	Image     ports.ResolvedImage
}
