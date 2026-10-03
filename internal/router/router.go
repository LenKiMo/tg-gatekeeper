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
func (r *Router) Run() { r.client.Handle().Run() }

// match 只接受我们真正处理的三类更新。
func (r *Router) match(u tgbotapi.Update) bool {
	if u.CallbackQuery != nil {
		return true
	}
	if u.ChatJoinRequest != nil {
		return true
	}
	if u.Message != nil && u.Message.Chat != nil && u.Message.From != nil {
		return true
	}
	return false
}

func (r *Router) handle(u tgbotapi.Update) error {
	switch {
	case u.CallbackQuery != nil:
		return r.handleCallback(u)
	case u.ChatJoinRequest != nil:
		return r.handleJoinRequest(u)
	case u.Message != nil:
		return r.handleMessage(u)
	}
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
	if m.Chat == nil || m.From == nil {
		return nil
	}
	chatID := m.Chat.ID
	userID := m.From.ID
	displayName := m.From.FullName()

	// 入群 / 退群服务消息。
	if len(m.NewChatMembers) > 0 {
		members := make([]gatekeeper.Member, 0, len(m.NewChatMembers))
		for _, member := range m.NewChatMembers {
			members = append(members, gatekeeper.Member{
				UserID:      member.ID,
				DisplayName: member.FullName(),
				IsBot:       member.IsBot,
				IsPremium:   member.IsPremium,
			})
		}
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

	// 私聊：本 bot 只在私聊里走"申请验证出题"，其余私聊消息一律忽略。
	if m.Chat.IsPrivate() {
		return nil
	}
	if !m.Chat.IsGroup() && !m.Chat.IsSuperGroup() {
		return nil
	}
	if m.From.ID == r.selfID() {
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
