package gatekeeper

import (
	"context"
	"strings"
	"testing"

	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
)

// TestRegUsesRepliedMessage 验证 /reg（回复一条消息）记下的是被回复的那条消息。
//
// 以前路由层传进来的是 /reg 命令消息自己的引用，于是群规链接指向了那条 "/reg"。
func TestRegUsesRepliedMessage(t *testing.T) {
	svc, api, _, _ := testService(t, nil)
	ctx := context.Background()
	api.admins[8] = true

	replied := domain.MessageRef{ChatID: -100, MessageID: 321}
	if err := svc.HandleCommand(ctx, -100, 8, "管理员", "/reg", "/reg", replied); err != nil {
		t.Fatal(err)
	}
	if got := mustGroup(t, svc, -100).RulesMessageID; got != 321 {
		t.Fatalf("群规应当是被回复的消息 321，实际 %d", got)
	}

	// 既不是回复、也没给链接：只提示用法，不改已有设置。
	if err := svc.HandleCommand(ctx, -100, 8, "管理员", "/reg", "/reg", domain.MessageRef{}); err != nil {
		t.Fatal(err)
	}
	if got := mustGroup(t, svc, -100).RulesMessageID; got != 321 {
		t.Fatalf("不带回复的 /reg 不应改动群规，实际 %d", got)
	}
	if last := api.texts[len(api.texts)-1].text; !strings.Contains(last, "用法") {
		t.Fatalf("不带回复的 /reg 应当提示用法，实际 %q", last)
	}
}
