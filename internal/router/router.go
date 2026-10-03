package router

import (
	"context"
	"log/slog"
	"strings"
	"time"

	tgbotapi "github.com/ijnkawakaze/telegram-bot-api"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/gatekeeper"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
	"github.com/LenKiMo/tg-gatekeeper/internal/telegram"
)

// Router 把 Telegram 更新按 lane 分派到业务层。
//
// 客户端库的 Run() 是严格串行的 update 循环，因此这里只做"解包 + 提交队列"，任何
// 网络/数据库工作都在 worker 里做。全部更新走一个通用处理器（而不是依赖库的
// command/callback 前缀表），是为了让注册顺序与拒绝策略完全由我们掌控：
// 待验证发言拦截永远排在命令处理之前。
type Router struct {
	client   *telegram.Client
	svc      *gatekeeper.Service
	dispatch ports.Dispatcher
	groups   ports.Store
	cfg      *config.Config
	log      *slog.Logger
}

// NewRouter 构造路由。
func NewRouter(client *telegram.Client, svc *gatekeeper.Service, groups ports.Store, d ports.Dispatcher, cfg *config.Config, log *slog.Logger) *Router {
	if log == nil {
		log = slog.Default()
	}
	return &Router{client: client, svc: svc, dispatch: d, groups: groups, cfg: cfg, log: log}
}

// Register 注册唯一的通用处理器。
//
// 注意必须用 Client.Handle()：AddHandle() 每次调用都会新建一个空 Bot（处理器表是
// 每实例的），如果注册用一个实例、Run 用另一个，更新会被静默丢弃。
func (r *Router) Register() {
	r.client.Handle().NewProcessor(r.match, r.handle)
}

// Run 启动长轮询（阻塞）。
//
// 不使用客户端库自带的 Run()：它不设置 allowed_updates，Telegram 因此不会推送
// chat_member 更新，而"自己经邀请链接入群"只会以 chat_member 形式送达——漏订阅等于
// 完全不验证这类入群（生产群真实踩过：6 小时内 6 次入群全部静默漏掉）。
func (r *Router) Run() {
	startup := time.Now()
	for u := range r.client.UpdatesChan(r.cfg.Bot.PollingTimeoutSeconds) {
		if staleUpload(u, startup) {
			continue // 启动时会一次性取回积压更新，不能把几小时前的旧事件当新事件处理
		}
		if !r.match(u) {
			continue
		}
		if err := r.handle(u); err != nil {
			r.log.Warn("处理更新失败", "update_id", u.UpdateID, "err", err.Error())
		}
	}
}

// staleUpload 判断更新是否早于本次启动时刻。
func staleUpload(u tgbotapi.Update, startup time.Time) bool {
	var ts time.Time
	switch {
	case u.Message != nil:
		ts = u.Message.Time()
	case u.EditedMessage != nil:
		ts = u.EditedMessage.Time()
	case u.CallbackQuery != nil && u.CallbackQuery.Message != nil:
		ts = u.CallbackQuery.Message.Time()
	case u.ChatMember != nil:
		ts = time.Unix(u.ChatMember.Date, 0)
	case u.MyChatMember != nil:
		ts = time.Unix(u.MyChatMember.Date, 0)
	case u.ChatJoinRequest != nil:
		ts = time.Unix(u.ChatJoinRequest.Date, 0)
	default:
		return false
	}
	return ts.Before(startup)
}

// memberJoined 判断成员状态是否属于"在群里"。
func memberJoined(cm tgbotapi.ChatMember) bool {
	switch cm.Status {
	case "creator", "administrator", "member":
		return true
	case "restricted":
		return cm.IsMember
	}
	return false
}

// match 只接受我们真正处理的三类更新。
//
// 注意：入群/退群服务消息**可能没有发送者**——用户自己经邀请链接加入时 Telegram 不带
// from（from 只在"被别人拉进来"时才有）。这类更新恰恰是入群验证最需要的，不能因为
// From 为空就丢掉：曾经因此在生产群漏掉全部 6 次自加入（进程活着、日志安静、无审计）。
func (r *Router) match(u tgbotapi.Update) bool {
	if u.CallbackQuery != nil {
		return true
	}
	if u.ChatJoinRequest != nil {
		return true
	}
	// 成员状态变化是入群/退群的主信号源（自加入只会以它送达）。
	if u.ChatMember != nil || u.MyChatMember != nil {
		return true
	}
	if u.Message == nil || u.Message.Chat == nil {
		return false
	}
	if len(u.Message.NewChatMembers) > 0 || u.Message.LeftChatMember != nil {
		return true
	}
	return u.Message.From != nil
}

func (r *Router) handle(u tgbotapi.Update) error {
	switch {
	case u.CallbackQuery != nil:
		return r.handleCallback(u)
	case u.ChatJoinRequest != nil:
		return r.handleJoinRequest(u)
	case u.ChatMember != nil:
		return r.handleChatMember(u)
	case u.MyChatMember != nil:
		return r.handleMyChatMember(u)
	case u.Message != nil:
		return r.handleMessage(u)
	}
	return nil
}

// handleChatMember 处理成员状态跃迁。
//
// 这是入群的主信号源，覆盖全部入群方式：被管理员拉进来、自己点邀请链接、申请通过。
// 相比之下 new_chat_members 服务消息只在"被别人拉进来"时才出现。
func (r *Router) handleChatMember(u tgbotapi.Update) error {
	cm := u.ChatMember
	// 注意：库里的 ChatMemberUpdated.Chat 是值类型，不能用 nil 判断。
	if cm == nil || cm.Chat.ID == 0 || cm.NewChatMember.User == nil {
		return nil
	}
	chatID := cm.Chat.ID
	user := cm.NewChatMember.User
	wasIn := memberJoined(cm.OldChatMember)
	nowIn := memberJoined(cm.NewChatMember)

	switch {
	case !wasIn && nowIn:
		member := gatekeeper.Member{
			UserID:      user.ID,
			DisplayName: user.FullName(),
			IsBot:       user.IsBot,
			IsPremium:   user.IsPremium,
		}
		// 被别人拉进来时记录邀请者；自己点链接进来时 From 就是本人。
		if cm.From.ID != 0 && cm.From.ID != user.ID {
			member.InvitedBy = cm.From.ID
		}
		r.log.Info("成员入群（状态跃迁）",
			"chat_id", chatID, "user_id", user.ID,
			"old", cm.OldChatMember.Status, "new", cm.NewChatMember.Status,
			"via_join_request", cm.ViaJoinRequest, "via_invite_link", cm.InviteLink != nil)
		_, cancel := r.criticalCtx()
		key := ports.DispatchKey{ChatID: chatID}
		updateID := int(u.UpdateID)
		if err := r.dispatch.Submit(context.Background(), ports.LaneCritical, key, func(ctx context.Context) error {
			defer cancel()
			return r.svc.OnMembersJoined(ctx, chatID, updateID, []gatekeeper.Member{member})
		}); err != nil {
			cancel()
			return err
		}
	case wasIn && !nowIn:
		// 退群/被踢：同样以状态跃迁为准，清掉待验证会话。
		r.log.Info("成员离开（状态跃迁）",
			"chat_id", chatID, "user_id", user.ID, "new", cm.NewChatMember.Status)
		_, cancel := r.criticalCtx()
		key := ports.DispatchKey{ChatID: chatID, UserID: user.ID}
		if err := r.dispatch.Submit(context.Background(), ports.LaneCritical, key, func(ctx context.Context) error {
			defer cancel()
			return r.svc.OnMemberLeft(ctx, chatID, user.ID)
		}); err != nil {
			cancel()
			return err
		}
	}
	return nil
}

// handleMyChatMember 记录机器人自身在群内的状态/权限变化（被提权、被降权、被移出等）。
func (r *Router) handleMyChatMember(u tgbotapi.Update) error {
	cm := u.MyChatMember
	if cm == nil {
		return nil
	}
	r.log.Info("机器人在群内状态变化",
		"chat_id", cm.Chat.ID,
		"old", cm.OldChatMember.Status, "new", cm.NewChatMember.Status)
	return nil
}

func (r *Router) handleCallback(u tgbotapi.Update) error {
	cq := u.CallbackQuery
	if cq.From == nil {
		return nil
	}
	chatID := cq.From.ID
	if cq.Message != nil && cq.Message.Chat != nil {
		chatID = cq.Message.Chat.ID
	}
	ev := gatekeeper.CallbackEvent{
		ID:          cq.ID,
		ChatID:      chatID,
		UserID:      cq.From.ID,
		DisplayName: cq.From.FullName(),
		Data:        cq.Data,
	}
	_, cancel := r.criticalCtx()
	key := ports.DispatchKey{ChatID: chatID, UserID: cq.From.ID}
	if err := r.dispatch.Submit(context.Background(), ports.LaneCritical, key, func(ctx context.Context) error {
		defer cancel()
		return r.svc.OnCallback(ctx, ev)
	}); err != nil {
		cancel()
		return err
	}
	return nil
}

func (r *Router) handleJoinRequest(u tgbotapi.Update) error {
	jr := u.ChatJoinRequest
	if jr.From.ID == 0 || jr.Chat.ID == 0 {
		return nil
	}
	chatID := jr.Chat.ID
	userID := jr.From.ID
	userChatID := jr.UserChatID
	if userChatID == 0 {
		userChatID = userID
	}
	req := gatekeeper.JoinRequest{
		ChatID:      chatID,
		UserID:      userID,
		UserChatID:  userChatID,
		DisplayName: jr.From.FullName(),
		Bio:         jr.Bio,
		UpdateID:    int(u.UpdateID),
	}
	_, cancel := r.criticalCtx()
	key := ports.DispatchKey{ChatID: chatID, UserID: userID}
	if err := r.dispatch.Submit(context.Background(), ports.LaneCritical, key, func(ctx context.Context) error {
		defer cancel()
		return r.svc.OnJoinRequest(ctx, req)
	}); err != nil {
		cancel()
		return err
	}
	return nil
}

func (r *Router) handleMessage(u tgbotapi.Update) error {
	m := u.Message
	if m.Chat == nil {
		return nil
	}
	isService := len(m.NewChatMembers) > 0 || m.LeftChatMember != nil
	// 普通消息必须有发送者；入群/退群服务消息允许没有 from（自加入时不带发送者）。
	if m.From == nil && !isService {
		return nil
	}
	chatID := m.Chat.ID
	var userID int64
	var displayName string
	if m.From != nil {
		userID = m.From.ID
		displayName = m.From.FullName()
	}

	// 入群 / 退群服务消息。
	if len(m.NewChatMembers) > 0 {
		members := make([]gatekeeper.Member, 0, len(m.NewChatMembers))
		ids := make([]int64, 0, len(m.NewChatMembers))
		for _, member := range m.NewChatMembers {
			members = append(members, gatekeeper.Member{
				UserID:      member.ID,
				DisplayName: member.FullName(),
				IsBot:       member.IsBot,
				IsPremium:   member.IsPremium,
			})
			ids = append(ids, member.ID)
		}
		// 入群是本 bot 最关键的输入，用 Info 打点：以后"没触发验证"可以直接看这句日志。
		r.log.Info("收到入群服务消息",
			"chat_id", chatID, "members", ids, "sender_nil", m.From == nil)
		joinRef := toRef(m)
		_, cancel := r.criticalCtx()
		key := ports.DispatchKey{ChatID: chatID}
		updateID := int(u.UpdateID)
		if err := r.dispatch.Submit(context.Background(), ports.LaneCritical, key, func(ctx context.Context) error {
			defer cancel()
			return r.svc.OnMembersJoinedWithJoinMessage(ctx, chatID, updateID, members, joinRef)
		}); err != nil {
			cancel()
			return err
		}
		return nil
	}
	if m.LeftChatMember != nil {
		left := m.LeftChatMember.ID
		_, cancel := r.criticalCtx()
		key := ports.DispatchKey{ChatID: chatID, UserID: left}
		if err := r.dispatch.Submit(context.Background(), ports.LaneCritical, key, func(ctx context.Context) error {
			defer cancel()
			return r.svc.OnMemberLeft(ctx, chatID, left)
		}); err != nil {
			cancel()
			return err
		}
		return nil
	}

	// 以下都是普通消息/命令路径：必须有发送者。
	if m.From == nil {
		return nil
	}

	// 私聊：本 bot 只在私聊里走"申请验证出题"，其余私聊消息一律忽略。
	if m.Chat.IsPrivate() {
		return nil
	}
	if !m.Chat.IsGroup() && !m.Chat.IsSuperGroup() {
		return nil
	}
	if userID == r.selfID() {
		return nil
	}

	// 待验证发言拦截：同步查一次会话（本地索引查询），保证它排在命令处理之前。
	text := strings.TrimSpace(m.Text)
	isCommand := m.IsCommand()
	if !isCommand {
		if pending, err := r.svc.HasPendingSession(context.Background(), chatID, userID); err == nil && pending {
			msgRef := toRef(m)
			_, cancel := r.criticalCtx()
			key := ports.DispatchKey{ChatID: chatID, UserID: userID}
			if err := r.dispatch.Submit(context.Background(), ports.LaneCritical, key, func(ctx context.Context) error {
				defer cancel()
				_, err := r.svc.OnGroupMessage(ctx, chatID, userID, msgRef.MessageID, displayName)
				return err
			}); err != nil {
				cancel()
				return err
			}
			return nil
		}
		return nil
	}

	// 命令处理（命令 lane 满载时回"繁忙"，不阻塞 update 循环）。
	cmd := strings.Fields(text)[0]
	key := ports.DispatchKey{ChatID: chatID, UserID: userID}
	msgRef := toRef(m)
	if err := r.dispatch.Submit(context.Background(), ports.LaneCommand, key, func(ctx context.Context) error {
		return r.svc.HandleCommand(ctx, chatID, userID, displayName, cmd, text, msgRef)
	}); err != nil {
		ctx, cancel := r.criticalCtx()
		defer cancel()
		go r.svc.NotifyBusy(ctx, chatID, userID, msgRef)
		return nil
	}
	return nil
}

// criticalCtx 给关键任务一个比"进程退出"更细的取消边界：
// 退群/停服时不会把半截操作留在库里（Claim 是事务，超时扫描会补）。
func (r *Router) criticalCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Minute)
}

func (r *Router) selfID() int64 {
	id, _ := r.client.Self()
	return id
}

func toRef(m *tgbotapi.Message) domain.MessageRef {
	ref := domain.MessageRef{MessageID: m.MessageID}
	if m.Chat != nil {
		ref.ChatID = m.Chat.ID
	}
	return ref
}
