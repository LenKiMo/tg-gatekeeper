package gatekeeper

import (
	"context"
	"testing"

	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/telegram"
)

// TestRequestModeWrongAnswerOnlyDeclines 验证申请模式答错只拒绝申请：恰好三个副作用
// （拒绝申请、删私聊题目、删群里的管理员卡片），没有封禁。
//
// 以前答错走的是 kick()：封禁一个根本不在群里的人、稍后再解封，申请本身没人处理，
// 管理员卡片也留在群里。
func TestRequestModeWrongAnswerOnlyDeclines(t *testing.T) {
	svc, api, registry, _ := testService(t, nil)
	ctx := context.Background()
	group := mustGroup(t, svc, -300)
	group.Mode = domain.VerifyModeRequest
	if err := svc.SaveGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	if err := svc.OnJoinRequest(ctx, JoinRequest{ChatID: -300, UserID: 20, UserChatID: 20, DisplayName: "申请者", UpdateID: 5}); err != nil {
		t.Fatal(err)
	}
	sess, err := registry.Get(ctx, domain.SessionKey{ChatID: -300, UserID: 20})
	if err != nil {
		t.Fatal(err)
	}
	if !sess.ChallengeMessage.Valid() || !sess.AdminMessage.Valid() {
		t.Fatalf("申请模式应当有私聊题目和群内管理员卡片: %+v", sess)
	}
	if err := svc.OnCallback(ctx, CallbackEvent{
		ID: "cb", ChatID: 20, UserID: 20, DisplayName: "申请者",
		Data: telegram.EncodeAnswer(sess.ID, wrongToken(t, sess)),
	}); err != nil {
		t.Fatal(err)
	}

	// 先看计划出来的副作用，再让它们执行。
	kinds := pendingEffectKinds(t, registry)
	if len(kinds) != 3 || countKind(kinds, domain.EffectDecline) != 1 || countKind(kinds, domain.EffectDeleteMessage) != 2 {
		t.Fatalf("应当恰好是 拒绝申请 + 两条删除，实际 %v", kinds)
	}
	if countKind(kinds, domain.EffectBan) != 0 || countKind(kinds, domain.EffectUnban) != 0 {
		t.Fatalf("申请模式答错不应有封禁/解封，实际 %v", kinds)
	}

	runBackground(t, svc)
	if len(api.declined) != 1 || api.declined[0].userID != 20 || api.declined[0].chatID != -300 {
		t.Fatalf("答错应当拒绝申请，实际 %+v", api.declined)
	}
	if len(api.banned) != 0 || len(api.unbanned) != 0 {
		t.Fatalf("申请模式答错不应封禁/解封，实际 banned=%+v unbanned=%+v", api.banned, api.unbanned)
	}
	deleted := map[domain.MessageRef]bool{}
	for _, d := range api.deleted {
		deleted[domain.MessageRef{ChatID: d.chatID, MessageID: d.message}] = true
	}
	if !deleted[sess.ChallengeMessage] || !deleted[sess.AdminMessage] {
		t.Fatalf("应当删除私聊题目 %+v 和管理员卡片 %+v，实际删除 %+v", sess.ChallengeMessage, sess.AdminMessage, api.deleted)
	}
}

// TestJoinModeWrongAnswerStillKicks 验证入群模式答错的行为不变：踢出（封禁），不是拒绝申请。
func TestJoinModeWrongAnswerStillKicks(t *testing.T) {
	svc, api, registry, _ := testService(t, nil)
	ctx := context.Background()
	joinOne(t, svc)
	sess, err := registry.Get(ctx, domain.SessionKey{ChatID: -100, UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.OnCallback(ctx, CallbackEvent{
		ID: "cb", ChatID: -100, UserID: 7, DisplayName: "新人",
		Data: telegram.EncodeAnswer(sess.ID, wrongToken(t, sess)),
	}); err != nil {
		t.Fatal(err)
	}
	kinds := pendingEffectKinds(t, registry)
	if countKind(kinds, domain.EffectBan) != 1 || countKind(kinds, domain.EffectDecline) != 0 {
		t.Fatalf("入群模式答错应当踢出而不是拒绝申请，实际 %v", kinds)
	}
	runBackground(t, svc)
	if len(api.banned) != 1 || api.banned[0].userID != 7 || len(api.declined) != 0 {
		t.Fatalf("入群模式答错应当封禁，实际 banned=%+v declined=%+v", api.banned, api.declined)
	}
}
