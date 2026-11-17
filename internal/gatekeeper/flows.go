package gatekeeper

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/challenge"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/filter"
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

	// 先落账（"各层互相兜底"的支点）：从这一刻起，任何失败都不会变成静默放行。
	// 空壳会话会在下面的分支里被替换或收进终态；如果进程恰好在这里被杀，它到期后
	// 由 scanExpired 按超时处置（入群模式 = 踢出）。
	ledger, err := s.beginJoinLedger(ctx, group, m.UserID, m.DisplayName)
	if err != nil {
		return err
	}

	// 广告词检查：先看昵称，再读一次资料（简介 + username + 头像）。
	//
	// 资料是一次 getChat 拿全的：广告词要简介、bot 农场规则要 username 与头像，
	// 分开调就是把同一个接口打两三遍（每次入群都付一次 RTT）。
	if word, hit := s.adFilter.Check(m.DisplayName, group.AdWords); hit {
		s.noteProfile(ctx, group, m.UserID, m.DisplayName, ports.UserProfile{},
			profileDecisionAdName, word, word)
		return s.rejectJoinByAdWord(ctx, group, ledger, m, word, "display_name")
	}

	var prof ports.UserProfile
	var profErr error
	if s.cfg.AdFilter.Enabled || s.englishNamePolicy() != "off" || s.collectProfiles() {
		prof, profErr = s.api.GetUserProfile(ctx, m.UserID)
	}

	if s.cfg.AdFilter.Enabled {
		switch {
		case profErr == nil:
			// 简介用 CheckBio：多一层 bio_words（日挣/USDT/搬砖/跑分 这类只在简介里
			// 才高精度的词）。命中同样不出题，直接按广告词处置。
			if word, hit := s.adFilter.CheckBio(prof.Bio, group.AdWords); hit {
				s.noteProfile(ctx, group, m.UserID, m.DisplayName, prof,
					profileDecisionAdBio, word, word)
				return s.rejectJoinByAdWord(ctx, group, ledger, m, word, "bio")
			}
		case s.cfg.AdFilter.BioLookupErrorPolicy == "deny":
			s.audit(ctx, ports.AuditEvent{
				Event: "verify.bio_lookup_failed", ChatID: group.ChatID, UserID: m.UserID,
				Result: "deny", ErrorClass: classify(profErr), Mode: string(group.Mode),
			})
			return s.rejectJoinByAdWord(ctx, group, ledger, m, "", "bio_unavailable")
		default:
			s.audit(ctx, ports.AuditEvent{
				Event: "verify.bio_lookup_failed", ChatID: group.ChatID, UserID: m.UserID,
				Result: "audit_continue", ErrorClass: classify(profErr), Mode: string(group.Mode),
			})
		}
	}

	// bot 农场特征链：两词英文名 **且** 无 username **且** 无简介 **且** 无头像。
	// 三个"无"缺一不可——"Jack Smith" 也可能是真人，只有资料全空才是有意义的信号。
	//
	// auditedBot 记录这条入群已经写过一条 english_name_bot 快照：audit 模式下命中了
	// 也照常出题，但那时不该再补一条 verify——同一次入群两条 decision 互相矛盾，
	// 事后统计"命中 bot 农场特征的有多少人最终答题通过"会被这批重复行污染。
	auditedBot := false
	if policy := s.englishNamePolicy(); policy != "off" && profErr == nil &&
		filter.IsPlainEnglishName(m.DisplayName) &&
		prof.Bio == "" && prof.Username == "" && !prof.HasPhoto {
		s.noteProfile(ctx, group, m.UserID, m.DisplayName, prof,
			profileDecisionEnglishName, "english_name", "")
		auditedBot = true
		s.audit(ctx, ports.AuditEvent{
			Event: "verify.bot_profile", ChatID: group.ChatID, UserID: m.UserID,
			Result: policy, Mode: string(group.Mode), DisplayName: m.DisplayName,
			NewState: string(domain.StateSkipped),
			Extra:    map[string]string{"rule": "english_name", "name": m.DisplayName},
		})
		if policy == "kick" {
			return s.kickJoinBot(ctx, group, ledger, m)
		}
	}

	// 预检通过：startVerification 用真实 updateID 重新 Begin，把空壳原子替换掉
	// （旧的进 session.replaced 审计）。这一步失败也不会丢账——空壳还在，到期会踢。
	sess, err := s.startVerification(ctx, group, m.UserID, m.DisplayName, domain.SessionJoin, domain.MessageRef{}, updateID)
	if err != nil {
		return err
	}
	if !auditedBot {
		s.noteProfile(ctx, group, m.UserID, m.DisplayName, prof, profileDecisionVerify, "", "")
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
		return s.rejectRequestByAdWord(ctx, group, req.UserID, req.DisplayName, word, "display_name")
	}
	if s.cfg.AdFilter.Enabled {
		bio := req.Bio
		if bio == "" {
			if got, err := s.api.GetBio(ctx, req.UserID); err == nil {
				bio = got
			}
		}
		if word, hit := s.adFilter.Check(bio, group.AdWords); hit {
			return s.rejectRequestByAdWord(ctx, group, req.UserID, req.DisplayName, word, "bio")
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
			// 放行前复查简介：入群时的 bio 查询可能失败；管理员也可能在看不到简介的
			// 情况下点了放行；答题窗口够长，足够改一次简介。命中就按广告号处置
			// （ad_word_ban_days），不给他放行。
			if word, hit := s.adRecheckOnPass(ctx, group, session.Key.UserID, session.DisplayName); hit {
				_ = s.api.AnswerCallback(ctx, cb.ID, "简介命中广告词「"+word+"」，按广告号处置", true)
				res, cerr := s.claimByID(ctx, session, domain.Event{
					Kind: domain.EventAdBan, SessionID: session.ID, At: at,
				})
				return s.finishClaim(ctx, group, res, cerr, "callback.ad_recheck")
			}
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
		// 管理员点"放行"时看不到对方简介：这里补一次检查，命中就改走广告号处置。
		// 管理员点"封禁"不复查——已经要封了，多一次 API 调用没有意义。
		if kind == domain.EventAdminPass {
			if word, hit := s.adRecheckOnPass(ctx, group, session.Key.UserID, session.DisplayName); hit {
				_ = s.api.AnswerCallback(ctx, cb.ID, "简介命中广告词「"+word+"」，已按广告号处置", true)
				kind = domain.EventAdBan
			} else {
				_ = s.api.AnswerCallback(ctx, cb.ID, "已处理", false)
			}
		} else {
			_ = s.api.AnswerCallback(ctx, cb.ID, "已处理", false)
		}
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

// beginJoinLedger 在入群第一刻落一条"空壳"会话，作为本次入群的账本。
//
// 这是"各层互相兜底"的支点。以前会话是在"决定出题"时才建（startVerification 里），
// 于是预检阶段的任何失败——getChat 挂、禁言 429、进程被杀——都既没有会话、也没有
// 副作用，等于静默放行（生产实例：2026-11-17 01:55 的 Judy Bach 就这样进了群）。
//
// 现在入群第一件事就是写这条记录。它只保证一件事：**这个人有人在等结论**。之后：
//   - 预检命中 → settleJoinLedger 收进 Cancel，再按广告词处置；
//   - 预检通过 → startVerification 用真实 updateID 重新 Begin，原子的"替换"掉空壳；
//   - 中途失败/进程被杀 → 空壳留在 Preparing，scanExpired 到期后按超时处置（踢出）。
//
// UpdateID 刻意写 0：Begin 的幂等判断是 `cur.UpdateID == s.UpdateID && cur.UpdateID != 0`，
// 写 0 既不会和"同一 update 重复投递"混淆，也让后面那次 Begin 一定走替换路径。
func (s *Service) beginJoinLedger(ctx context.Context, group domain.GroupConfig, userID int64,
	displayName string) (domain.Session, error) {
	// 已有进行中的会话（另一条入群通道先到，或用户还没走完验证）：不再建账本，
	// 直接当作重复信号返回。这与 startVerification 里原来的"先查后建"同义。
	if cur, err := s.registry.Get(ctx, domain.SessionKey{ChatID: group.ChatID, UserID: userID}); err == nil &&
		!cur.State.Terminal() {
		return domain.Session{}, nil
	}
	id, err := domain.NewSessionID()
	if err != nil {
		return domain.Session{}, err
	}
	now := time.Now()
	sess := domain.Session{
		ID:          id,
		Key:         domain.SessionKey{ChatID: group.ChatID, UserID: userID},
		UpdateID:    0,
		Mode:        domain.SessionJoin,
		State:       domain.StatePreparing,
		DisplayName: displayName,
		StartedAt:   now,
		ExpiresAt:   now.Add(s.cfg.VerifyTimeout() + s.cfg.PrecheckGrace()),
		Version:     1,
		// 空切片而不是 nil：sessions.correct_hash 是 NOT NULL，账本阶段还没抽题、
		// 自然没有答案摘要。写 nil 会让 INSERT 直接违反约束——生产上表现为"每次入群
		// 都报 constraint failed 然后整条流程中断"（2026-11-17 17:48 三个号漏网）。
		CorrectTokenHash: []byte{},
	}
	begin, err := s.registry.Begin(ctx, sess)
	if err != nil {
		return domain.Session{}, err
	}
	if begin.Duplicate {
		// 同一用户已有进行中的会话（两条入群通道到达），账本不必再建。
		return domain.Session{}, nil
	}
	return begin.Session, nil
}

// settleJoinLedger 把账本会话收进 Cancel 终态，避免它在处置之后又被当成超时踢一次。
func (s *Service) settleJoinLedger(ctx context.Context, group domain.GroupConfig, sess domain.Session) {
	if sess.ID == "" {
		return
	}
	_, _ = s.registry.Claim(ctx, sess.Key, domain.Event{
		Kind: domain.EventCancel, SessionID: sess.ID, At: time.Now(),
	}, s.effectPlan(group, ""))
}

// startVerification 生成题目、限制用户、发题并登记会话。
func (s *Service) startVerification(ctx context.Context, group domain.GroupConfig, userID int64,
	displayName string, mode domain.SessionMode, joinMessage domain.MessageRef, updateID int, opts ...startOption) (domain.Session, error) {

	o := startOpts{joinMessage: joinMessage}
	for _, fn := range opts {
		fn(&o)
	}

	// 同一个入群事件可能从两条通道到达（new_chat_members 服务消息 + chat_member 状态跃迁），
	// 同一用户已有进行中的会话时不再重复出题；否则会出现两道并存的题、旧题无人清理。
	// 分派器对同一群/用户是严格 FIFO，因此这里"先查后建"不会与并发出题打架。
	if cur, err := s.registry.Get(ctx, domain.SessionKey{ChatID: group.ChatID, UserID: userID}); err == nil {
		// UpdateID==0 是 beginJoinLedger 落的账本空壳（预检前那条 "这个人在等结论" 的记录）。
		// 它不是"两条入群通道"的重复信号，要继续往下走 Begin——那次 Begin 会原子替换它。
		if !cur.State.Terminal() && cur.UpdateID != 0 {
			s.audit(ctx, ports.AuditEvent{
				Event: "verify.duplicate_signal", ChatID: group.ChatID, UserID: userID,
				DisplayName: displayName, Mode: string(group.Mode),
				Result: "ignored", NewState: string(cur.State),
			})
			// 两条入群通道的到达顺序不定。chat_member 先到时会话已经建好、没挂上入群消息；
			// 随后到的服务消息会被当成重复信号，若就此丢掉 ref，那句"X 加入了群组"永远不删。
			if o.joinMessage.Valid() && s.cfg.Gatekeeper.DeleteJoinMessage {
				if derr := s.enqueueDeletion(ctx, o.joinMessage, 0); derr != nil {
					s.log.Warn("删除重复入群信号的服务消息失败", "chat_id", group.ChatID,
						"err", s.scrubToken(derr.Error()))
				}
			}
			return domain.Session{}, nil
		}
	}

	built, err := s.nextChallenge(ctx, mode)
	if err != nil {
		// 出不了题（题库为空/损坏、图片取不到）。入群模式下人已经在群里、还没被禁言、
		// 也没有会话：直接返回就等于不经验证放行，failure_policy=deny 形同虚设。
		// 所以在这里按失败策略处置；不建会话、不禁言，不留半成品。申请模式不用管：
		// 人还没进群，调用方会拒绝申请。
		if mode == domain.SessionJoin {
			// 题库整体损坏时每个入群的人都会走到这里，单独打一条 Warn，便于发现
			// "基础设施故障引发批量封禁"（审计里按 reason 聚合 verify.challenge_failed 即可计数）。
			s.log.Warn("出不了题，按 failure_policy 处置新成员",
				"chat_id", group.ChatID, "user_id", userID,
				"failure_policy", s.cfg.Gatekeeper.FailurePolicy, "err", s.scrubToken(err.Error()))
			s.challengeFailed(ctx, group, userID, displayName, banReasonGenerateFailed, err, false)
		}
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
		DisplayName:       displayName,
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
			// 禁言失败 ≠ 对方是管理员。这里必须先确认身份：只有确实是管理员/创建者才
			// 跳过验证（否则会把群主踢出去），其余失败（限流、网络、权限不足）说明这次
			// 验证根本做不了。
			//
			// 以前这里一律按"跳过验证"处理，于是"getChat 失败 + 禁言失败"两次 API 抖动
			// 会让一个 spam 号静默留在群里（生产实例：2026-11-17 01:55 的 Judy Bach，
			// bio 是 t.me/+ 私聊链接 + 一天挣九千）。禁言失败后按 failure_policy 处置。
			isAdmin := false
			if v, aerr := s.api.IsAdminWithPermissions(ctx, group.ChatID, userID, ports.AdminCanRestrictMembers); aerr == nil {
				isAdmin = v
			}
			if isAdmin {
				_, _ = s.registry.Claim(ctx, session.Key, domain.Event{
					Kind: domain.EventCancel, SessionID: session.ID, At: time.Now(),
				}, nil)
				return domain.Session{}, nil
			}
			_, _ = s.registry.Claim(ctx, session.Key, domain.Event{
				Kind: domain.EventCancel, SessionID: session.ID, At: time.Now(),
			}, nil)
			s.challengeFailed(ctx, group, userID, displayName, banReasonRestrictFailed, err, false)
			return domain.Session{}, fmt.Errorf("禁言失败: %w", err)
		}
	}

	target := group.ChatID
	captionTemplate := s.cfg.Gatekeeper.CaptionJoin
	if mode == domain.SessionRequest {
		target = o.userChatID
		captionTemplate = s.cfg.Gatekeeper.CaptionRequest
	}
	caption := telegram.RenderCaption(captionTemplate, userID, displayName, s.cfg.Gatekeeper.TimeoutSeconds, "")
	// 入群模式的题目发在群里，管理员能直接看到，放行/封禁就并在这张卡片的最后一行
	// （选项 + 2 个管理员键），不再另发一条管理员消息。
	buttons := telegram.ChallengeKeyboard(ch.Options, s.cfg.Gatekeeper.ButtonColumns, session.ID)
	if mode == domain.SessionRequest {
		// 申请模式的题目发在私聊，管理员看不到，所以题目本身只放选项。
		buttons = telegram.OptionKeyboard(ch.Options, s.cfg.Gatekeeper.ButtonColumns, session.ID)
	}

	ref, err := s.api.SendChallenge(ctx, target, built.Image, caption, buttons)
	if err != nil {
		// 申请模式最常见的失败原因：申请人从没和机器人说过话，Telegram 不允许
		// 机器人主动私聊他（"bot can't initiate conversation with a user"）。
		// 这时不该直接拒绝申请，而是在群里 @ 他、给一个直达机器人的按钮，
		// 等他按下「开始」后由 OnBotContact 重新出题。
		if mode == domain.SessionRequest {
			if nerr := s.awaitContact(ctx, group, session, userID, displayName); nerr == nil {
				return session, nil
			}
		}
		// 其他失败：把会话作废，并按失败策略处置。
		_, _ = s.registry.Claim(ctx, session.Key, domain.Event{
			Kind: domain.EventCancel, SessionID: session.ID, At: time.Now(),
		}, nil)
		if mode == domain.SessionJoin {
			s.challengeFailed(ctx, group, userID, displayName, banReasonSendFailed, err, true)
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

	// 管理员处置卡片：只有申请模式需要（题目在私聊，群里得有一张能点按钮的卡片）。
	// 入群模式的放行/封禁已经并进题目卡片，不再另发消息。
	if mode == domain.SessionRequest {
		text := telegram.RenderCaption(s.cfg.Gatekeeper.AdminCardText, userID, displayName, s.cfg.Gatekeeper.TimeoutSeconds, "")
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

// challengeFailed 按 failure_policy 处置"入群模式下题目没能发到新成员面前"。
//
// deny：封禁 failure_ban_seconds 秒，到期由 Telegram 自动解除。这是机器人这边的故障
// （集中入群触发限流、网络抖动、题目文案转义写错），所以不是永久封禁，也不撤回对方的
// 历史消息。封禁只发一次、失败不重试：目标是管理员这类封不了的人时每次都会 403，
// 重试只会变成请求风暴。
// allow：放行；restricted 为 true 表示此前已经禁言，需要恢复权限。
//
// 审计只记错误分类，不记错误原文（网络错误的原文里带着含 bot token 的请求地址）。
func (s *Service) challengeFailed(ctx context.Context, group domain.GroupConfig, userID int64,
	displayName string, reason banReason, cause error, restricted bool) {
	ev := ports.AuditEvent{
		Event: "verify.challenge_failed", ChatID: group.ChatID, UserID: userID,
		Mode: string(domain.SessionJoin), DisplayName: displayName,
		ErrorClass: classify(cause), Extra: map[string]string{"reason": string(reason)},
	}
	if s.cfg.Gatekeeper.FailurePolicy == "allow" {
		if restricted {
			_ = s.api.RestorePermissions(ctx, group.ChatID, userID)
		}
		ev.Result = "allow"
		s.audit(ctx, ev)
		return
	}
	seconds := int64(s.cfg.Gatekeeper.FailureBanSeconds)
	ev.Result = "ban"
	ev.Extra["ban_seconds"] = itoa(seconds)
	if err := s.applyTempBan(ctx, group, userID, seconds, reason); err != nil {
		ev.Result = "ban_failed"
		ev.ErrorClass = classify(err)
	}
	s.audit(ctx, ev)
}

// nextChallenge 抽一道题并取好图片；题库还没加载、或出不了题时返回错误。
func (s *Service) nextChallenge(ctx context.Context, mode domain.SessionMode) (*generated, error) {
	gen := s.Generator()
	if gen == nil {
		return nil, errors.New("题库尚未加载")
	}
	return s.generate(ctx, gen, mode)
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

// adBanUntil 返回广告词封禁的到期时间（unix 秒）；days <= 0 表示永久封禁（返回 0）。
//
// 广告号被 Telegram 注销得很快，永久封禁只会让群的黑名单随时间无限膨胀，
// 因此默认改为临时封禁（ad_word_ban_days，默认 30 天）。
func adBanUntil(days int) int64 {
	if days <= 0 {
		return 0
	}
	return time.Now().Add(time.Duration(days) * 24 * time.Hour).Unix()
}

// rejectRequestByAdWord 处置"申请模式下命中广告词"。
//
// 默认只拒绝申请、不封禁：申请人还没进群，封一个不在群里的人只会往群黑名单里堆东西，
// 而广告号很快会被 Telegram 注销。要靠封禁扛住重复申请冲击的群，把
// ad_word_request_ban_days 配成 > 0（建议 3），语义与入群模式的 ad_word_ban_days 对齐。
func (s *Service) rejectRequestByAdWord(ctx context.Context, group domain.GroupConfig,
	userID int64, displayName, word, field string) error {
	days := s.cfg.Gatekeeper.AdWordRequestBanDays
	extra := map[string]string{"field": field, "request_ban_days": itoa(int64(days))}
	if word != "" {
		extra["word"] = word
	}
	ev := ports.AuditEvent{
		Event: "verify.ad_rejected", ChatID: group.ChatID, UserID: userID,
		DisplayName: displayName, Mode: string(domain.SessionRequest), Extra: extra,
	}
	if days > 0 {
		ev.Result = "ban"
		if err := s.applyTempBan(ctx, group, userID, int64(days)*86400, banReasonAdRequest); err != nil {
			ev.Result = "ban_failed"
			ev.ErrorClass = classify(err)
		}
	} else {
		ev.Result = "decline"
	}
	s.audit(ctx, ev)
	return s.declineRequest(ctx, group.ChatID, userID)
}

// englishNamePolicy 返回生效的 bot 农场策略（off/audit/kick），已归一化大小写。
func (s *Service) englishNamePolicy() string {
	switch p := strings.ToLower(strings.TrimSpace(s.cfg.Gatekeeper.EnglishNamePolicy)); p {
	case "audit", "kick":
		return p
	default:
		return "off"
	}
}

// kickBotProfile 静默踢出命中 bot 农场特征链的成员：不出题、不发任何提示。
//
// 用的是与"超时踢出"同一条路径——一次带 until 的定时封禁，到期由 Telegram 自动解除
// （kick_unban_after_seconds）。到期时间在执行这一刻才换算：提前算好会被重试拖成过去
// 时间，那样就成了永久封禁。
func (s *Service) kickBotProfile(ctx context.Context, group domain.GroupConfig, m Member) error {
	until := time.Now().Add(s.cfg.KickUnbanAfter()).Unix()
	return s.api.Ban(ctx, group.ChatID, m.UserID, s.cfg.Gatekeeper.RevokeMessagesOnBan, until)
}

// rejectJoinByAdWord 把"预检命中广告词"收进终态：EventAdBan → 按 ad_word_ban_days 封禁。
//
// 关键在于**副作用走 outbox**（effects 表，由 effectLoop 重试）：以前这里直接调一次
// api.Ban，429/网络抖动一失败就只剩一行日志，人还在群里。现在失败的封禁会被重试，
// 直到成功或明确判定为永久错误。
//
// 账本会话缺失或被替换（极小概率）时退回直接封禁——宁可重复封一次，也不能放过。
func (s *Service) rejectJoinByAdWord(ctx context.Context, group domain.GroupConfig, sess domain.Session,
	m Member, word, field string) error {
	days := s.cfg.Gatekeeper.AdWordBanDays
	extra := map[string]string{"field": field, "ban_days": itoa(int64(days))}
	if word != "" {
		extra["word"] = word
	}
	s.audit(ctx, ports.AuditEvent{
		Event: "verify.ad_rejected", ChatID: group.ChatID, UserID: m.UserID,
		Result: "ban", Mode: string(group.Mode), DisplayName: m.DisplayName, Extra: extra,
	})
	if sess.ID == "" {
		return s.api.Ban(ctx, group.ChatID, m.UserID, s.cfg.Gatekeeper.RevokeMessagesOnBan,
			adBanUntil(days))
	}
	res, err := s.registry.Claim(ctx, sess.Key, domain.Event{
		Kind: domain.EventAdBan, SessionID: sess.ID, At: time.Now(),
	}, s.effectPlan(group, ""))
	if err == nil && res.Won {
		s.flushEffects(ctx, res.Effects)
		return nil
	}
	// 账本已被替换/过期：直接封一次兜住，不让"状态不对"变成放行。
	return s.api.Ban(ctx, group.ChatID, m.UserID, s.cfg.Gatekeeper.RevokeMessagesOnBan,
		adBanUntil(days))
}

// kickJoinBot 静默踢出命中 bot 农场特征链的成员：EventKick → 临时踢（副作用走 outbox）。
func (s *Service) kickJoinBot(ctx context.Context, group domain.GroupConfig, sess domain.Session,
	m Member) error {
	if sess.ID == "" {
		return s.kickBotProfile(ctx, group, m)
	}
	res, err := s.registry.Claim(ctx, sess.Key, domain.Event{
		Kind: domain.EventKick, SessionID: sess.ID, At: time.Now(),
	}, s.effectPlan(group, ""))
	if err == nil && res.Won {
		s.flushEffects(ctx, res.Effects)
		return nil
	}
	return s.kickBotProfile(ctx, group, m)
}

// adRecheckOnPass 在"即将放行"这一刻复查简介，返回命中的词。
//
// 为什么这一刻要再查一次（入群时明明查过）：
//   - 入群时的 getChat 可能失败；bio_lookup_error_policy=audit_continue 会继续出题，
//     于是这个人的简介从来没被检查过就等在放行门口。
//   - 管理员点"✅ 放行"时看不到对方的简介，放行的可能是个写着"进群了解一天5万"的号。
//   - 答题窗口有几十秒，足够改一次简介（实测投放方是批量改的）。
//
// 读不到资料一律放行：与入群时的 audit_continue 保持同一立场——宁可漏，不可因机器人
// 自身故障把人挡在门外。
func (s *Service) adRecheckOnPass(ctx context.Context, group domain.GroupConfig, userID int64, displayName string) (string, bool) {
	if !s.cfg.AdFilter.Enabled || !s.cfg.AdFilter.RecheckOnPass {
		return "", false
	}
	prof, err := s.api.GetUserProfile(ctx, userID)
	if err != nil {
		s.audit(ctx, ports.AuditEvent{
			Event: "verify.bio_recheck_failed", ChatID: group.ChatID, UserID: userID,
			Result: "audit_continue", ErrorClass: classify(err), DisplayName: displayName,
		})
		return "", false
	}
	word, hit := s.adFilter.CheckBio(prof.Bio, group.AdWords)
	if !hit {
		return "", false
	}
	s.audit(ctx, ports.AuditEvent{
		Event: "verify.ad_rejected", ChatID: group.ChatID, UserID: userID,
		Result: "ban", Mode: string(group.Mode), DisplayName: displayName,
		Extra: map[string]string{"field": "bio_on_pass", "word": word},
	})
	return word, true
}

// rejectByFilter 处置"已进群/正在入群的人命中广告词"：封禁 ad_word_ban_days 天。
// 只走入群模式；申请模式在 OnJoinRequest 里单独处置（见 rejectRequestByAdWord），
// 以前这里留着的申请分支永远走不到，是死代码。
func (s *Service) rejectByFilter(ctx context.Context, group domain.GroupConfig, m Member, word, field string) error {
	extra := map[string]string{"field": field}
	if word != "" {
		extra["word"] = word
	}
	days := s.cfg.Gatekeeper.AdWordBanDays
	extra["ban_days"] = itoa(int64(days))
	s.audit(ctx, ports.AuditEvent{
		Event: "verify.ad_rejected", ChatID: group.ChatID, UserID: m.UserID,
		Result: "ban", Mode: string(group.Mode), DisplayName: m.DisplayName, Extra: extra,
	})
	return s.api.Ban(ctx, group.ChatID, m.UserID, s.cfg.Gatekeeper.RevokeMessagesOnBan, adBanUntil(days))
}

// awaitContact 处理"申请人还没和机器人交互过，私聊发不出去"：
// 在群里 @ 他，并给一个 https://t.me/<bot>?start=<会话ID> 的按钮。
// 会话保持"题目未发出"的状态，超时后按申请模式原有的策略拒绝申请。
func (s *Service) awaitContact(ctx context.Context, group domain.GroupConfig, session domain.Session,
	userID int64, displayName string) error {
	_, bot := s.api.Self()
	if bot == "" {
		return errors.New("未知机器人用户名，无法生成联系链接")
	}
	text := telegram.RenderCaption(s.cfg.Gatekeeper.CaptionContact, userID, displayName,
		s.cfg.Gatekeeper.TimeoutSeconds, "")
	buttons := ports.ButtonGrid{{
		{Text: s.cfg.Gatekeeper.ContactButton, URL: "https://t.me/" + bot + "?start=" + session.ID},
	}}
	notice, err := s.api.SendText(ctx, group.ChatID, text, true, buttons)
	if err != nil {
		return err
	}
	// 把提示挂成会话的"管理员消息"：验证结束（超时/放行/封禁/作废）时会被一起清掉。
	if upd, aerr := s.registry.AttachMessages(ctx, session.Key, session.ID, domain.MessageRef{},
		notice, time.Now().Add(s.cfg.ContactTimeout()), session.Version); aerr == nil {
		session = upd
	}
	s.audit(ctx, ports.AuditEvent{
		Event: "verify.awaiting_contact", ChatID: group.ChatID, UserID: userID,
		SessionID: session.ID, Mode: string(domain.SessionRequest), NewState: string(domain.StatePreparing),
		Result: "pending", DisplayName: displayName,
	})
	return nil
}

// OnBotContact 处理用户主动与机器人交互（私聊里的 /start <会话ID>）。
//
// 申请模式下机器人无法主动私聊没点过它的用户，因此群里会贴一条带
// ?start=<会话ID> 的提示。用户按下「开始」后走到这里：作废等待中的会话
// （顺带删掉群里的提示），然后重新出一道题私聊发给他。
func (s *Service) OnBotContact(ctx context.Context, userID int64, displayName, payload string) error {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		// 没有 payload：可能是用户自己搜到机器人按了开始，没有会话可关联。
		return nil
	}
	waiting, err := s.registry.GetByID(ctx, payload)
	if err != nil {
		s.audit(ctx, ports.AuditEvent{
			Event: "verify.contact_unknown", UserID: userID, Result: "ignored",
			ErrorClass: classify(err), DisplayName: displayName,
		})
		return nil
	}
	if waiting.Key.UserID != userID || waiting.Mode != domain.SessionRequest || waiting.State.Terminal() {
		// payload 被转发给别人、或会话已经处理过：忽略（防越权替他人验证）。
		s.audit(ctx, ports.AuditEvent{
			Event: "verify.contact_mismatch", ChatID: waiting.Key.ChatID, UserID: userID,
			SessionID: waiting.ID, Result: "ignored", NewState: string(waiting.State), DisplayName: displayName,
		})
		return nil
	}
	_, _ = s.registry.Claim(ctx, waiting.Key, domain.Event{
		Kind: domain.EventCancel, SessionID: waiting.ID, At: time.Now(),
	}, nil)
	if waiting.AdminMessage.Valid() {
		_ = s.api.DeleteMessage(ctx, waiting.AdminMessage)
	}
	group, err := s.Group(ctx, waiting.Key.ChatID)
	if err != nil {
		return err
	}
	sess, err := s.startVerification(ctx, group, userID, displayName, domain.SessionRequest,
		domain.MessageRef{}, 0, withUserChat(userID))
	result := "challenge_sent"
	if err != nil {
		result = "failed"
	}
	s.audit(ctx, ports.AuditEvent{
		Event: "verify.contact_received", ChatID: waiting.Key.ChatID, UserID: userID,
		SessionID: sess.ID, Mode: string(domain.SessionRequest), Result: result,
		ErrorClass: classify(err), DisplayName: displayName,
	})
	return err
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

// scrubToken 把文本里的 bot token 换成占位符。请求 Telegram 失败时，网络错误的原文里
// 带着含 token 的请求地址；错误文本进日志之前先过这里。
func (s *Service) scrubToken(text string) string {
	if token := s.cfg.Bot.Token; token != "" {
		text = strings.ReplaceAll(text, token, "[token]")
	}
	return text
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
