package gatekeeper

import (
	"context"
	"testing"

	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
)

// runCommand 以 8 号管理员的身份在 -100 群里发一条命令。
func runCommand(t *testing.T, svc *Service, text string) {
	t.Helper()
	if err := svc.HandleCommand(context.Background(), -100, 8, "管理员", firstField(text), text, domain.MessageRef{}); err != nil {
		t.Fatalf("命令 %q 失败: %v", text, err)
	}
}

// TestCommandsAcceptOwnBotSuffix 验证带自己用户名后缀的命令（群里从命令菜单点出来的
// 就是这种）不会把 "@机器人" 当成参数。测试里机器人的用户名是 gatekeeper_bot。
func TestCommandsAcceptOwnBotSuffix(t *testing.T) {
	svc, api, _, _ := testService(t, nil)
	api.admins[8] = true

	runCommand(t, svc, "/welcome@gatekeeper_bot 欢迎 {mention}\n请先看群规")
	if got := mustGroup(t, svc, -100).Welcome; got != "欢迎 {mention}\n请先看群规" {
		t.Fatalf("欢迎语不应带上 @后缀，实际 %q", got)
	}
	runCommand(t, svc, "/welcome@gatekeeper_bot clear")
	if got := mustGroup(t, svc, -100).Welcome; got != "" {
		t.Fatalf("clear 应当清空欢迎语，实际 %q", got)
	}

	// 用户名大小写不敏感，命令本身也不敏感。
	runCommand(t, svc, "/Tag@Gatekeeper_Bot 新人")
	if got := mustGroup(t, svc, -100).Tag; got != "新人" {
		t.Fatalf("标签不应带上 @后缀，实际 %q", got)
	}
	runCommand(t, svc, "/tag@gatekeeper_bot clear")
	if got := mustGroup(t, svc, -100).Tag; got != "" {
		t.Fatalf("clear 应当清空标签，实际 %q", got)
	}

	runCommand(t, svc, "/reg@gatekeeper_bot https://t.me/somegroup/5")
	if got := mustGroup(t, svc, -100).RulesLink; got != "https://t.me/somegroup/5" {
		t.Fatalf("群规链接应当被识别，实际 %q", got)
	}

	runCommand(t, svc, "/adwords@gatekeeper_bot set 六合彩 澳门")
	if got := mustGroup(t, svc, -100).AdWords; len(got) != 2 || got[0] != "六合彩" {
		t.Fatalf("广告词应当为 2 个，实际 %v", got)
	}
}

// TestCommandsForOtherBotsAreIgnored 验证指名发给别的机器人的命令不会被我们执行，
// 也不回任何消息。
func TestCommandsForOtherBotsAreIgnored(t *testing.T) {
	svc, api, _, _ := testService(t, nil)
	api.admins[8] = true
	runCommand(t, svc, "/tag 老成员")
	api.texts = nil

	for _, text := range []string{
		"/tag@other_bot clear",
		"/welcome@other_bot 这是给别的机器人的欢迎语",
		"/reg@other_bot https://t.me/elsewhere/1",
		"/request_mode@other_bot request",
		"/ping@other_bot",
		"/help@other_bot",
	} {
		runCommand(t, svc, text)
	}

	group := mustGroup(t, svc, -100)
	if group.Tag != "老成员" || group.Welcome != "" || group.RulesLink != "" || group.Mode != domain.VerifyModeJoin {
		t.Fatalf("发给别的机器人的命令不应改动本群配置，实际 %+v", group)
	}
	if len(api.texts) != 0 {
		t.Fatalf("发给别的机器人的命令不应有任何回复，实际 %+v", api.texts)
	}
}

// TestPlainCommandsStillWork 验证不带后缀的命令行为不变。
func TestPlainCommandsStillWork(t *testing.T) {
	svc, api, _, _ := testService(t, nil)
	api.admins[8] = true

	runCommand(t, svc, "/welcome 欢迎 {mention}")
	runCommand(t, svc, "/tag 新人")
	group := mustGroup(t, svc, -100)
	if group.Welcome != "欢迎 {mention}" || group.Tag != "新人" {
		t.Fatalf("不带后缀的命令应当照常生效，实际 %+v", group)
	}
}
