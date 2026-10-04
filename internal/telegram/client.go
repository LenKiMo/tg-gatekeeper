package telegram

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/ijnkawakaze/telegram-bot-api"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
)

// Client 是 ports.Telegram 的适配器，基于 github.com/ijnkawakaze/telegram-bot-api v1.1.1。
//
// 本适配器的两处兼容处理（替换客户端库时请复测）：
//   - v1.1.1 的 IsAdminWithPermissions 里 convertAdminRightToInt 恒返回 0，
//     传非零权限位时对"管理员"会误判为无权限。这里改用 IsAdmin（权限位为 0）。
//   - BanChatMember 硬编码 RevokeMessages=true，因此封禁走自定义 config，
//     以便遵守 revoke_messages_on_ban 配置。
//   - SendPhotoBytes 不支持 caption/按钮，出题走自定义 photo 配置。
type Client struct {
	api        *tgbotapi.BotAPI
	self       tgbotapi.User
	timeout    time.Duration
	tagMaxRune int

	// handler 是唯一的处理器容器：AddHandle() 每次都会 NewBot(api) 新建一个空 Bot，
	// 因此"注册处理器的那个 Bot"和"Run() 循环用的那个 Bot"必须是同一个实例，
	// 否则所有更新都会被静默忽略（处理器表在另一个对象里）。
	handlerOnce sync.Once
	handler     *tgbotapi.Bot
}

// Handle 返回唯一的处理器容器（懒创建，全局复用）。
func (c *Client) Handle() *tgbotapi.Bot {
	c.handlerOnce.Do(func() {
		c.handler = c.api.AddHandle()
	})
	return c.handler
}

// NewClient 构造适配器（会调用 getMe 校验 token）。
func NewClient(cfg *config.Config) (*Client, error) {
	client, err := buildHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	apiEndpoint := cfg.Bot.APIEndpoint
	if apiEndpoint == "" {
		apiEndpoint = tgbotapi.APIEndpoint
	}
	api, err := tgbotapi.NewBotAPIWithClient(cfg.Bot.Token, apiEndpoint, client)
	if err != nil {
		return nil, fmt.Errorf("初始化 Telegram 客户端失败（检查 token/网络/代理）: %w", err)
	}
	api.Debug = cfg.Bot.Debug
	if cfg.Bot.IgnoreChannelCommands {
		api.IgnoreChannelCMD = true
	}
	return &Client{
		api:        api,
		self:       api.Self,
		timeout:    time.Duration(cfg.Bot.RequestTimeoutSeconds) * time.Second,
		tagMaxRune: max(cfg.Commands.TagMaxRunes, 16),
	}, nil
}

func buildHTTPClient(cfg *config.Config) (*http.Client, error) {
	transport := &http.Transport{
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	if p := strings.TrimSpace(cfg.Bot.HTTPProxy); p != "" {
		pu, err := url.Parse(p)
		if err != nil {
			return nil, fmt.Errorf("bot.http_proxy %q 不是合法 URL: %w", p, err)
		}
		transport.Proxy = http.ProxyURL(pu)
	} else {
		transport.Proxy = http.ProxyFromEnvironment
	}

	// 客户端的 getUpdates 长轮询超时是库内固定的 60s（不读取配置），因此请求级超时
	// 必须按接口区分：getUpdates 要能等满整个长轮询，其余接口要快速失败。
	// 用 http.Client.Timeout 做不到（它对所有请求一律生效），所以在 RoundTripper
	// 上按路径下发放 deadline，并把 deadline 覆盖到 body 读取阶段。
	longPoll := time.Duration(cfg.Bot.PollingTimeoutSeconds) * time.Second
	if longPoll < 60*time.Second {
		longPoll = 60 * time.Second
	}
	longPoll += 20 * time.Second // 覆盖库内部的 60s 长轮询 + 余量
	normal := time.Duration(cfg.Bot.RequestTimeoutSeconds) * time.Second
	if normal <= 0 {
		normal = 20 * time.Second
	}
	return &http.Client{
		Transport: &pathTimeoutTransport{base: transport, longPoll: longPoll, normal: normal},
		// 不再设全局 Timeout：由 pathTimeoutTransport 逐请求控制。
	}, nil
}

// pathTimeoutTransport 按接口路径下发不同的请求超时。
//
// 背景：telegram-bot-api 的 getUpdates 是**长轮询**，服务端会在 timeout 秒内不发任何
// 响应头；如果客户端超时短于它，每次轮询都会以 "context deadline exceeded" 结束，
// 循环变成"每 40 秒断一次 + 重连"，日志噪音大、更新延迟不可控。
type pathTimeoutTransport struct {
	base     http.RoundTripper
	longPoll time.Duration
	normal   time.Duration
}

func (t *pathTimeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	timeout := t.normal
	if strings.Contains(req.URL.Path, "/getUpdates") {
		timeout = t.longPoll
	}
	ctx, cancel := context.WithTimeout(req.Context(), timeout)
	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &deadlineBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// deadlineBody 在读完/关闭响应体时释放 deadline（否则每次请求都会泄漏一个定时器）。
type deadlineBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.once.Do(b.cancel)
	}
	return n, err
}

func (b *deadlineBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.cancel)
	return err
}

// Bot 返回底层库句柄（router 注册处理器需要）。
func (c *Client) Bot() *tgbotapi.BotAPI { return c.api }

// UpdatesChan 返回订阅了必要更新类型的更新通道。
//
// 必须显式订阅 chat_member：Telegram 默认不推送这一类型，而"用户自己点邀请链接入群"
// 只会以 chat_member（成员状态 left→member）的形式送达，不会产生 new_chat_members
// 消息。漏订阅的表现就是"有人入群、机器人毫无反应"，且没有任何错误日志可查。
func (c *Client) UpdatesChan(timeoutSeconds int) tgbotapi.UpdatesChannel {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = timeoutSeconds
	u.AllowedUpdates = []string{
		"message",
		"edited_message",
		"callback_query",
		"chat_join_request",
		"chat_member",
		"my_chat_member",
	}
	return c.api.GetUpdatesChan(u)
}

// Self 实现 ports.Telegram。
func (c *Client) Self() (int64, string) { return c.self.ID, c.self.UserName }

// Close 实现 ports.Telegram。
func (c *Client) Close() error { return nil }

// RestrictNoMessages 禁言（只保留读取能力）。
func (c *Client) RestrictNoMessages(_ context.Context, chatID, userID int64) error {
	_, err := c.api.RestrictChatMember(chatID, userID, tgbotapi.NoMessagesPermission)
	return wrap("禁言", err)
}

// RestorePermissions 恢复默认发送权限。
func (c *Client) RestorePermissions(_ context.Context, chatID, userID int64) error {
	_, err := c.api.RestrictChatMember(chatID, userID, tgbotapi.AllPermissions)
	return wrap("恢复权限", err)
}

// Ban 封禁（可控制是否撤回历史消息）。
func (c *Client) Ban(_ context.Context, chatID, userID int64, revokeMessages bool, until int64) error {
	cfg := tgbotapi.BanChatMemberConfig{
		ChatMemberConfig: tgbotapi.ChatMemberConfig{ChatID: chatID, UserID: userID},
		RevokeMessages:   revokeMessages,
		UntilDate:        until, // 0 = 永久
	}
	_, err := c.api.Request(cfg)
	return wrap("封禁", err)
}

// Unban 解封（仅当确实被封禁时）。
func (c *Client) Unban(_ context.Context, chatID, userID int64, onlyIfBanned bool) error {
	cfg := tgbotapi.UnbanChatMemberConfig{
		ChatMemberConfig: tgbotapi.ChatMemberConfig{ChatID: chatID, UserID: userID},
		OnlyIfBanned:     onlyIfBanned,
	}
	_, err := c.api.Request(cfg)
	return wrap("解封", err)
}

// ApproveJoinRequest 通过入群申请。
func (c *Client) ApproveJoinRequest(_ context.Context, chatID, userID int64) error {
	_, err := c.api.ApproveChatJoinRequest(chatID, userID)
	return wrap("通过入群申请", err)
}

// DeclineJoinRequest 拒绝入群申请。
func (c *Client) DeclineJoinRequest(_ context.Context, chatID, userID int64) error {
	_, err := c.api.DeclineChatJoinRequest(chatID, userID)
	return wrap("拒绝入群申请", err)
}

// SendChallenge 发送题目图片 + 选项按钮。
func (c *Client) SendChallenge(_ context.Context, chatID int64, img ports.ResolvedImage,
	caption string, buttons ports.ButtonGrid) (domain.MessageRef, error) {

	photo := tgbotapi.NewPhoto(chatID, tgbotapi.FileBytes{Bytes: img.Bytes, Name: img.Filename})
	if caption != "" {
		photo.Caption = caption
		photo.ParseMode = tgbotapi.ModeMarkdownV2
	}
	if !buttons.Empty() {
		photo.ReplyMarkup = buildKeyboard(buttons)
	}
	msg, err := c.api.Send(photo)
	if err != nil {
		return domain.MessageRef{}, wrap("发送题目", err)
	}
	return refOf(msg), nil
}

// SendText 发送文本；markdownV2 为 true 时按 MarkdownV2 解析。
func (c *Client) SendText(_ context.Context, chatID int64, text string, markdownV2 bool, buttons ports.ButtonGrid) (domain.MessageRef, error) {
	msg := tgbotapi.NewMessage(chatID, text)
	if markdownV2 {
		msg.ParseMode = tgbotapi.ModeMarkdownV2
	}
	if !buttons.Empty() {
		msg.ReplyMarkup = buildKeyboard(buttons)
	}
	sent, err := c.api.Send(msg)
	if err != nil {
		if markdownV2 {
			// MarkdownV2 解析失败几乎总是转义漏了；退化为纯文本再试一次，
			// 避免因为一个昵称里的圆点让整条提示发不出去。
			plain := tgbotapi.NewMessage(chatID, stripMarkdown(text))
			if !buttons.Empty() {
				plain.ReplyMarkup = buildKeyboard(buttons)
			}
			if fallback, ferr := c.api.Send(plain); ferr == nil {
				return refOf(fallback), nil
			}
		}
		return domain.MessageRef{}, wrap("发送消息", err)
	}
	return refOf(sent), nil
}

// DeleteMessage 删除消息。
func (c *Client) DeleteMessage(_ context.Context, ref domain.MessageRef) error {
	if !ref.Valid() {
		return nil
	}
	_, err := c.api.Request(tgbotapi.NewDeleteMessage(ref.ChatID, int64(ref.MessageID)))
	return wrap("删除消息", err)
}

// AnswerCallback 回应按钮点击。
func (c *Client) AnswerCallback(_ context.Context, callbackID, text string, alert bool) error {
	if callbackID == "" {
		return nil
	}
	var cfg tgbotapi.CallbackConfig
	if alert {
		cfg = tgbotapi.NewCallbackWithAlert(callbackID, text)
	} else {
		cfg = tgbotapi.NewCallback(callbackID, text)
	}
	_, err := c.api.Request(cfg)
	return err
}

// GetBio 取用户简介（普通入群模式的广告词检查用）。
func (c *Client) GetBio(_ context.Context, userID int64) (string, error) {
	info, err := c.api.GetChatInfo(userID)
	if err != nil {
		return "", wrap("读取用户资料", err)
	}
	return strings.TrimSpace(info.Bio), nil
}

// IsAdminWithPermissions 判断是否管理员/创建者。
//
// 注意：这里刻意使用 IsAdmin（requiredPermissions=0），原因见类型注释。
func (c *Client) IsAdminWithPermissions(_ context.Context, chatID, userID int64, _ uint16) (bool, error) {
	return c.api.IsAdmin(chatID, userID), nil
}

// GetMemberStatus 返回成员状态字符串。
func (c *Client) GetMemberStatus(_ context.Context, chatID, userID int64) (string, error) {
	member, err := c.api.GetChatMember(tgbotapi.GetChatMemberConfig{
		ChatConfigWithUser: tgbotapi.ChatConfigWithUser{ChatID: chatID, UserID: userID},
	})
	if err != nil {
		return "unknown", wrap("读取成员状态", err)
	}
	return member.Status, nil
}

// SetMemberTag 设置成员标签（最多 16 个字符，Telegram 硬限制）。
func (c *Client) SetMemberTag(_ context.Context, chatID, userID int64, tag string) error {
	runes := []rune(strings.TrimSpace(tag))
	if len(runes) == 0 {
		return nil
	}
	limit := c.tagMaxRune
	if limit <= 0 || limit > 16 {
		limit = 16
	}
	if len(runes) > limit {
		runes = runes[:limit]
	}
	_, err := c.api.SetMemberTag(chatID, userID, string(runes))
	return wrap("设置成员标签", err)
}

func refOf(msg tgbotapi.Message) domain.MessageRef {
	chatID := int64(0)
	if msg.Chat != nil {
		chatID = msg.Chat.ID
	}
	return domain.MessageRef{ChatID: chatID, MessageID: msg.MessageID}
}

func buildKeyboard(grid ports.ButtonGrid) tgbotapi.InlineKeyboardMarkup {
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(grid))
	for _, row := range grid {
		buttons := make([]tgbotapi.InlineKeyboardButton, 0, len(row))
		for _, b := range row {
			if b.URL != "" {
				buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonURL(b.Text, b.URL))
				continue
			}
			buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData(b.Text, b.Data))
		}
		rows = append(rows, buttons)
	}
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func wrap(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s失败: %w", action, err)
}

// stripMarkdown 去掉 MarkdownV2 转义反斜杠，用于降级为纯文本发送。
func stripMarkdown(s string) string {
	return strings.NewReplacer(`\`, "").Replace(s)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
