package gatekeeper

import (
	"context"
	"fmt"
	"strings"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
)

// HasPendingSession 判断某用户是否正在验证中（用于"待验证发言"的快速判定）。
func (s *Service) HasPendingSession(ctx context.Context, chatID, userID int64) (bool, error) {
	_, err := s.registry.Get(ctx, domain.SessionKey{ChatID: chatID, UserID: userID})
	if err != nil {
		return false, nil
	}
	return true, nil
}

// OnMembersJoinedWithJoinMessage 与 OnMembersJoined 相同，但额外带上"XX 加入群组"服务消息，
// 便于验证结束后把它一并清理掉。
func (s *Service) OnMembersJoinedWithJoinMessage(ctx context.Context, chatID int64, updateID int,
	members []Member, joinMessage domain.MessageRef) error {
	group, err := s.Group(ctx, chatID)
	if err != nil {
		return err
	}
	for _, m := range members {
		if err := s.verifyNewMemberWithJoinMessage(ctx, group, m, updateID, joinMessage); err != nil {
			s.log.Warn("处理入群失败", "chat_id", chatID, "user_id", m.UserID, "err", err)
		}
	}
	return nil
}

func (s *Service) verifyNewMemberWithJoinMessage(ctx context.Context, group domain.GroupConfig, m Member,
	updateID int, joinMessage domain.MessageRef) error {
	if m.IsBot && s.cfg.Gatekeeper.BotMemberPolicy == "allow" {
		s.auditSkip(ctx, group, m, "bot")
		return nil
	}
	if !group.Enabled {
		s.auditSkip(ctx, group, m, "group_disabled")
		return nil
	}
	if group.Mode != domain.VerifyModeJoin {
		s.auditSkip(ctx, group, m, "mode_request")
		return nil
	}
	if s.cfg.Gatekeeper.AdminMemberPolicy == "allow" {
		if isAdmin, err := s.api.IsAdminWithPermissions(ctx, group.ChatID, m.UserID, 0); err == nil && isAdmin {
			s.auditSkip(ctx, group, m, "admin")
			return nil
		}
	}
	if m.InvitedBy != 0 && s.cfg.Gatekeeper.InvitedMemberPolicy == "allow" {
		s.auditSkip(ctx, group, m, "invited")
		return nil
	}
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
			return s.rejectByFilter(ctx, group, m, "", "bio_unavailable")
		}
	}
	sess, err := s.startVerification(ctx, group, m.UserID, m.DisplayName, domain.SessionJoin,
		joinMessage, updateID)
	if err != nil {
		return err
	}
	if sess.ID != "" {
		s.audit(ctx, ports.AuditEvent{
			Event: "verify.started", ChatID: group.ChatID, UserID: m.UserID, SessionID: sess.ID,
			Mode: string(domain.SessionJoin), NewState: string(domain.StatePending), Result: "pending",
			DatasetRevision: s.DatasetRevision(), DisplayName: m.DisplayName,
		})
	}
	return nil
}

// NotifyBusy 在命令队列已满时给一条"繁忙"提示（不占用队列）。
func (s *Service) NotifyBusy(ctx context.Context, chatID, userID int64, replyTo domain.MessageRef) {
	text := "当前请求过多，请稍后再试。"
	if ref, err := s.api.SendText(ctx, chatID, text, false, nil); err == nil {
		if s.cfg.Commands.ResponseDeleteAfterSeconds > 0 {
			_ = s.enqueueDeletion(ctx, ref, int64(s.cfg.Commands.ResponseDeleteAfterSeconds))
		}
	}
	s.audit(ctx, ports.AuditEvent{
		Event: "command.busy", ChatID: chatID, UserID: userID, Result: "rejected",
	})
}

// HandleCommand 处理群内命令。
func (s *Service) HandleCommand(ctx context.Context, chatID, userID int64, displayName, command, text string, msgRef domain.MessageRef) error {
	cmd := strings.ToLower(strings.TrimSpace(command))
	if idx := strings.IndexByte(cmd, '@'); idx > 0 {
		cmd = cmd[:idx]
	}
	// 私聊命令（申请者不会用到，这里只处理群内命令），非管理员命令直接忽略。
	switch cmd {
	case "/ping":
		return s.replyTemp(ctx, chatID, userID, fmt.Sprintf("pong（题库 %d 个标签，版本 %s）",
			func() int {
				if g := s.Generator(); g != nil {
					return g.Buckets()
				}
				return 0
			}(), s.DatasetRevision()))
	case "/help":
		return s.replyTemp(ctx, chatID, userID, helpText(s.cfg))
	case "/request_mode", "/mode":
		return s.cmdRequestMode(ctx, chatID, userID, text)
	case "/welcome":
		return s.cmdWelcome(ctx, chatID, userID, text)
	case "/reg":
		return s.cmdReg(ctx, chatID, userID, text, msgRef)
	case "/tag":
		return s.cmdTag(ctx, chatID, userID, text)
	case "/adwords", "/adword", "/banwords":
		return s.cmdAdWords(ctx, chatID, userID, text)
	case "/reload":
		if userID != s.ownerID() {
			return nil
		}
		if err := s.RefreshDataset(ctx); err != nil {
			return s.replyTemp(ctx, chatID, userID, "题库刷新失败："+err.Error())
		}
		return s.replyTemp(ctx, chatID, userID, "题库已刷新，版本 "+s.DatasetRevision())
	default:
		return nil
	}
}

func (s *Service) ownerID() int64 { return s.cfg.Bot.OwnerID }

// replyTemp 发送一条会在 response_delete_after_seconds 后被删除的临时提示。
func (s *Service) replyTemp(ctx context.Context, chatID, userID int64, text string) error {
	ref, err := s.api.SendText(ctx, chatID, text, false, nil)
	if err != nil {
		return err
	}
	if s.cfg.Commands.ResponseDeleteAfterSeconds > 0 {
		_ = s.enqueueDeletion(ctx, ref, int64(s.cfg.Commands.ResponseDeleteAfterSeconds))
	}
	s.audit(ctx, ports.AuditEvent{Event: "command.reply", ChatID: chatID, UserID: userID, Result: "ok"})
	return nil
}

// requireAdmin 校验操作者是管理员。
func (s *Service) requireAdmin(ctx context.Context, chatID, userID int64) (bool, error) {
	ok, err := s.api.IsAdminWithPermissions(ctx, chatID, userID, 0)
	if err != nil {
		return false, err
	}
	return ok, nil
}

func (s *Service) cmdRequestMode(ctx context.Context, chatID, userID int64, text string) error {
	ok, err := s.requireAdmin(ctx, chatID, userID)
	if err != nil || !ok {
		return s.replyTemp(ctx, chatID, userID, "只有管理员可以切换验证模式。")
	}
	fields := strings.Fields(text)
	group, err := s.Group(ctx, chatID)
	if err != nil {
		return err
	}
	if len(fields) < 2 {
		next := domain.VerifyModeJoin
		if group.Mode == domain.VerifyModeJoin {
			next = domain.VerifyModeRequest
		}
		group.Mode = next
	} else {
		switch strings.ToLower(fields[1]) {
		case "join", "on", "直接", "入群":
			group.Mode = domain.VerifyModeJoin
		case "request", "申请", "审批":
			group.Mode = domain.VerifyModeRequest
		default:
			return s.replyTemp(ctx, chatID, userID, "用法：/request_mode [join|request]")
		}
	}
	if err := s.SaveGroup(ctx, group); err != nil {
		return s.replyTemp(ctx, chatID, userID, "保存失败："+err.Error())
	}
	s.audit(ctx, ports.AuditEvent{
		Event: "command.request_mode", ChatID: chatID, UserID: userID, ActorID: userID,
		Result: string(group.Mode), Mode: string(group.Mode),
	})
	return s.replyTemp(ctx, chatID, userID, "验证模式已切换为："+string(group.Mode))
}

func (s *Service) cmdWelcome(ctx context.Context, chatID, userID int64, text string) error {
	ok, err := s.requireAdmin(ctx, chatID, userID)
	if err != nil || !ok {
		return s.replyTemp(ctx, chatID, userID, "只有管理员可以设置欢迎语。")
	}
	group, err := s.Group(ctx, chatID)
	if err != nil {
		return err
	}
	body := strings.TrimSpace(strings.TrimPrefix(text, "/welcome"))
	switch body {
	case "clear", "off", "清空":
		group.Welcome = ""
	default:
		if body == "" {
			return s.replyTemp(ctx, chatID, userID,
				"用法：/welcome 欢迎语（支持 {mention} 提及新成员、{rules} 群规链接；/welcome clear 清空）")
		}
		group.Welcome = body
	}
	if err := s.SaveGroup(ctx, group); err != nil {
		return s.replyTemp(ctx, chatID, userID, "保存失败："+err.Error())
	}
	s.audit(ctx, ports.AuditEvent{Event: "command.welcome", ChatID: chatID, UserID: userID, Result: "ok"})
	return s.replyTemp(ctx, chatID, userID, "欢迎语已更新（验证通过后发送）。")
}

// cmdAdWords 管理本群广告词（昵称/简介命中即封禁，见 gatekeeper.ad_word_ban_days）。
//
//	/adwords                 查看当前词表
//	/adwords add 六合彩 澳门   追加
//	/adwords del 澳门          删除
//	/adwords set 六合彩 澳门   整表替换
//	/adwords clear            清空（此后只受 ad_filter.global_words 约束）
//
// 之所以要有这个命令：群级设置在库里的群行上，用 SQL 手改列类型写错就会让
// 读群配置整体失败（表现为"所有人入群都不触发验证"）。走命令由程序自己写库。
func (s *Service) cmdAdWords(ctx context.Context, chatID, userID int64, text string) error {
	ok, err := s.requireAdmin(ctx, chatID, userID)
	if err != nil || !ok {
		return s.replyTemp(ctx, chatID, userID, "只有管理员可以修改广告词表。")
	}
	group, err := s.Group(ctx, chatID)
	if err != nil {
		return err
	}
	show := func() error {
		if len(group.AdWords) == 0 {
			return s.replyTemp(ctx, chatID, userID, "本群广告词表为空（仅受全局词表约束）。")
		}
		return s.replyTemp(ctx, chatID, userID, fmt.Sprintf("本群广告词 %d 个：\n%s\n（命中即封禁 %d 天，0 = 永久）",
			len(group.AdWords), strings.Join(group.AdWords, "、"), s.cfg.Gatekeeper.AdWordBanDays))
	}

	fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(text, "/"+strings.Fields(text)[0][1:])))
	if len(fields) == 0 {
		return show()
	}
	action, args := strings.ToLower(fields[0]), fields[1:]
	switch action {
	case "add", "追加":
		group.AdWords = append(group.AdWords, args...)
	case "del", "remove", "删除":
		drop := make(map[string]bool, len(args))
		for _, a := range args {
			drop[a] = true
		}
		kept := make([]string, 0, len(group.AdWords))
		for _, w := range group.AdWords {
			if !drop[w] {
				kept = append(kept, w)
			}
		}
		group.AdWords = kept
	case "set", "重置":
		group.AdWords = args
	case "clear", "off", "清空":
		group.AdWords = nil
	default:
		return s.replyTemp(ctx, chatID, userID,
			"用法：/adwords［查看］| /adwords add 词… | /adwords del 词… | /adwords set 词… | /adwords clear")
	}
	// 空白词会被过滤器丢弃，这里也顺手去掉，避免词表里出现看不见的项。
	cleaned := make([]string, 0, len(group.AdWords))
	seen := make(map[string]bool, len(group.AdWords))
	for _, w := range group.AdWords {
		w = strings.TrimSpace(w)
		if w == "" || seen[w] {
			continue
		}
		seen[w] = true
		cleaned = append(cleaned, w)
	}
	group.AdWords = cleaned
	if err := s.SaveGroup(ctx, group); err != nil {
		return s.replyTemp(ctx, chatID, userID, "保存失败："+err.Error())
	}
	s.audit(ctx, ports.AuditEvent{
		Event: "command.adwords", ChatID: chatID, UserID: userID, Result: "ok",
		Extra: map[string]string{"action": action, "count": itoa(int64(len(cleaned)))},
	})
	return show()
}

// cmdReg 设置群规来源，三种用法：
//   - /reg <http(s) 链接>：显式链接（推荐，公群可以直接给 t.me/<群名>/<消息ID> 这种好看的链接）
//   - /reg（回复一条消息）：记录该消息 ID，欢迎语里的 {rules} 会推导成 t.me/c/<内部ID>/<消息ID>
//   - /reg clear：清空
func (s *Service) cmdReg(ctx context.Context, chatID, userID int64, text string, msgRef domain.MessageRef) error {
	ok, err := s.requireAdmin(ctx, chatID, userID)
	if err != nil || !ok {
		return s.replyTemp(ctx, chatID, userID, "只有管理员可以设置群规。")
	}
	group, err := s.Group(ctx, chatID)
	if err != nil {
		return err
	}
	arg := strings.TrimSpace(strings.TrimPrefix(text, "/reg"))
	switch {
	case arg == "clear":
		group.RulesLink = ""
		group.RulesMessageID = 0
	case strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://"):
		group.RulesLink = arg
	case arg == "" && msgRef.MessageID != 0:
		group.RulesMessageID = msgRef.MessageID
		group.RulesLink = ""
	default:
		return s.replyTemp(ctx, chatID, userID,
			"用法：/reg <群规链接>，或回复一条消息用 /reg 把它设为群规；/reg clear 清空。")
	}
	if err := s.SaveGroup(ctx, group); err != nil {
		return s.replyTemp(ctx, chatID, userID, "保存失败："+err.Error())
	}
	s.audit(ctx, ports.AuditEvent{Event: "command.reg", ChatID: chatID, UserID: userID, Result: "ok"})
	return s.replyTemp(ctx, chatID, userID, "群规已更新（欢迎语里的 {rules} 会替换成这条链接）。")
}

func (s *Service) cmdTag(ctx context.Context, chatID, userID int64, text string) error {
	ok, err := s.requireAdmin(ctx, chatID, userID)
	if err != nil || !ok {
		return s.replyTemp(ctx, chatID, userID, "只有管理员可以设置成员标签。")
	}
	group, err := s.Group(ctx, chatID)
	if err != nil {
		return err
	}
	body := strings.TrimSpace(strings.TrimPrefix(text, "/tag"))
	switch body {
	case "clear", "off", "清空":
		group.Tag = ""
	default:
		limit := s.cfg.Commands.TagMaxRunes
		if limit <= 0 || limit > 16 {
			limit = 16
		}
		runes := []rune(body)
		if len(runes) > limit {
			return s.replyTemp(ctx, chatID, userID, fmt.Sprintf("标签最多 %d 个字符。", limit))
		}
		group.Tag = body
	}
	if err := s.SaveGroup(ctx, group); err != nil {
		return s.replyTemp(ctx, chatID, userID, "保存失败："+err.Error())
	}
	s.audit(ctx, ports.AuditEvent{Event: "command.tag", ChatID: chatID, UserID: userID, Result: "ok"})
	return s.replyTemp(ctx, chatID, userID, "成员标签已更新（验证通过后自动设置）。")
}

func helpText(cfg *config.Config) string {
	noun := "选项"
	if cfg.Gatekeeper.OptionCount > 0 {
		noun = fmt.Sprintf("%d 个选项", cfg.Gatekeeper.OptionCount)
	}
	return strings.Join([]string{
		"入群验证机器人",
		fmt.Sprintf("· 新成员入群后需在 %d 秒内从 %s 中选出与图片对应的名称，选错或超时会被移出群组。",
			cfg.Gatekeeper.TimeoutSeconds, noun),
		"· /ping 存活检查",
		"· /help 查看帮助",
		"管理员：",
		"· /request_mode [join|request] 切换验证模式",
		"· /welcome 欢迎语（支持 {mention} 提及、{rules} 群规链接；clear 清空）",
		"· /reg <链接> 设置群规链接（或回复一条消息用 /reg 把它设为群规）",
		"· /tag 验证通过后自动设置的成员标签（clear 清空）",
		"· /adwords [add|del|set|clear] 本群广告词表（昵称/简介命中即封禁）",
	}, "\n")
}
