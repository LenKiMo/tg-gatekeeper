package gatekeeper

import (
	"context"
	"testing"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/store"
	"github.com/LenKiMo/tg-gatekeeper/internal/telegram"
)

// answerWrong 让 7 号用户入群并答错。
func answerWrong(t *testing.T, svc *Service, registry *store.MemRegistry) {
	t.Helper()
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
}

// assertSingleBanNoUnban 断言 outbox 里恰好一条封禁、没有解封。
func assertSingleBanNoUnban(t *testing.T, registry *store.MemRegistry) {
	t.Helper()
	kinds := pendingEffectKinds(t, registry)
	if countKind(kinds, domain.EffectBan) != 1 || countKind(kinds, domain.EffectUnban) != 0 {
		t.Fatalf("应当恰好一条封禁、没有解封任务，实际 %v", kinds)
	}
}

// TestKickIsSingleTimedBan 验证自动踢出是一条定时封禁，不再配一条独立的解封任务。
//
// 以前踢出 = 永久封禁 + kick_unban_after_seconds 后的解封，两条副作用各自重试：
// 封禁被重试拖过解封的到点时间时，解封先空跑，之后封禁成功就变成了永久封禁。
func TestKickIsSingleTimedBan(t *testing.T) {
	cases := map[string]struct {
		unbanAfter int
		wantAfter  int64 // 0 = 永久
	}{
		"默认 60 秒":         {unbanAfter: 60, wantAfter: 60},
		"按配置时长":           {unbanAfter: 300, wantAfter: 300},
		"不到下限的按 60 秒算":    {unbanAfter: 30, wantAfter: 60},
		"显式配 0 才是永久（不解封）": {unbanAfter: 0, wantAfter: 0},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			svc, api, registry, _ := testService(t, func(cfg *config.Config) {
				cfg.Gatekeeper.KickUnbanAfterSeconds = c.unbanAfter
			})
			answerWrong(t, svc, registry)
			assertSingleBanNoUnban(t, registry)
			runBackground(t, svc)

			if len(api.banned) != 1 || api.banned[0].userID != 7 {
				t.Fatalf("答错应当封禁一次，实际 %+v", api.banned)
			}
			if c.wantAfter == 0 {
				if api.banned[0].until != 0 {
					t.Fatalf("配成 0 应为永久封禁，实际 until=%d", api.banned[0].until)
				}
			} else {
				assertTempBan(t, api.banned[0], c.wantAfter)
			}
			if !api.banned[0].revoke {
				t.Fatal("踢出不是机器人自身故障，应当按 revoke_messages_on_ban 撤回消息")
			}
			if len(api.unbanned) != 0 {
				t.Fatalf("不应调用解封，实际 %+v", api.unbanned)
			}
		})
	}
}

// TestKickOnTimeoutAndSpeakingIsAlsoSingleBan 验证超时、没验证就发言这两种踢出同样只有一条定时封禁。
func TestKickOnTimeoutAndSpeakingIsAlsoSingleBan(t *testing.T) {
	t.Run("超时", func(t *testing.T) {
		svc, api, registry, _ := testService(t, func(cfg *config.Config) {
			cfg.Gatekeeper.KickUnbanAfterSeconds = 60
		})
		ctx := context.Background()
		joinOne(t, svc)
		sess, err := registry.Get(ctx, domain.SessionKey{ChatID: -100, UserID: 7})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := registry.AttachMessages(ctx, sess.Key, sess.ID, sess.ChallengeMessage, domain.MessageRef{},
			time.Now().Add(-time.Second), sess.Version); err != nil {
			t.Fatal(err)
		}
		svc.scanExpired(ctx)
		assertSingleBanNoUnban(t, registry)
		runBackground(t, svc)
		if len(api.banned) != 1 {
			t.Fatalf("超时应当封禁一次，实际 %+v", api.banned)
		}
		assertTempBan(t, api.banned[0], 60)
	})
	t.Run("没验证就发言", func(t *testing.T) {
		svc, api, registry, _ := testService(t, func(cfg *config.Config) {
			cfg.Gatekeeper.KickUnbanAfterSeconds = 60
		})
		joinOne(t, svc)
		if _, err := svc.OnGroupMessage(context.Background(), -100, 7, 555, "新人"); err != nil {
			t.Fatal(err)
		}
		assertSingleBanNoUnban(t, registry)
		runBackground(t, svc)
		if len(api.banned) != 1 {
			t.Fatalf("抢话应当封禁一次，实际 %+v", api.banned)
		}
		assertTempBan(t, api.banned[0], 60)
	})
}

// TestKickBanExpiryIsComputedAtExecution 验证到期时间是执行封禁的那一刻才算的：
// 封禁任务被重试拖了很久之后再执行，仍然是"从现在起 N 秒"，不会因为时间点已经过去而变成永久。
func TestKickBanExpiryIsComputedAtExecution(t *testing.T) {
	svc, api, _, _ := testService(t, nil)
	stale := domain.Effect{
		Kind:  domain.EffectBan,
		DueAt: time.Now().Add(-2 * time.Hour), // 两小时前就该执行了
		Payload: domain.EffectPayload{
			"chat_id": "-100", "user_id": "7", "seconds": "60", "reason": string(banReasonKick),
		},
	}
	if err := svc.doEffect(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	if len(api.banned) != 1 {
		t.Fatalf("应当执行一次封禁，实际 %+v", api.banned)
	}
	assertTempBan(t, api.banned[0], 60)
}

// TestAdminBanIsSevenDaysByDefault 验证管理员手动封禁默认 7 天，没有解封任务；
// 申请模式下是"拒绝申请 + 7 天封禁"。
func TestAdminBanIsSevenDaysByDefault(t *testing.T) {
	const week = 7 * 24 * 3600

	t.Run("入群模式", func(t *testing.T) {
		svc, api, registry, _ := testService(t, nil)
		ctx := context.Background()
		joinOne(t, svc)
		sess, err := registry.Get(ctx, domain.SessionKey{ChatID: -100, UserID: 7})
		if err != nil {
			t.Fatal(err)
		}
		api.admins[999] = true
		if err := svc.OnCallback(ctx, CallbackEvent{
			ID: "cb", ChatID: -100, UserID: 999, DisplayName: "管理员", Data: telegram.EncodeAdmin(sess.ID, true),
		}); err != nil {
			t.Fatal(err)
		}
		assertSingleBanNoUnban(t, registry)
		runBackground(t, svc)
		if len(api.banned) != 1 || api.banned[0].userID != 7 {
			t.Fatalf("管理员封禁应当生效，实际 %+v", api.banned)
		}
		assertTempBan(t, api.banned[0], week)
		if len(api.unbanned) != 0 {
			t.Fatalf("不应调用解封，实际 %+v", api.unbanned)
		}
	})

	t.Run("申请模式：拒绝申请 + 封禁", func(t *testing.T) {
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
		api.admins[999] = true
		if err := svc.OnCallback(ctx, CallbackEvent{
			ID: "cb", ChatID: -300, UserID: 999, DisplayName: "管理员", Data: telegram.EncodeAdmin(sess.ID, true),
		}); err != nil {
			t.Fatal(err)
		}
		assertSingleBanNoUnban(t, registry)
		runBackground(t, svc)
		if len(api.declined) != 1 || len(api.banned) != 1 || api.banned[0].userID != 20 {
			t.Fatalf("应当拒绝申请并封禁，实际 declined=%+v banned=%+v", api.declined, api.banned)
		}
		assertTempBan(t, api.banned[0], week)
	})

	t.Run("显式配 0 才是永久", func(t *testing.T) {
		svc, api, registry, _ := testService(t, func(cfg *config.Config) {
			cfg.Gatekeeper.AdminBanSeconds = 0
		})
		ctx := context.Background()
		joinOne(t, svc)
		sess, err := registry.Get(ctx, domain.SessionKey{ChatID: -100, UserID: 7})
		if err != nil {
			t.Fatal(err)
		}
		api.admins[999] = true
		if err := svc.OnCallback(ctx, CallbackEvent{
			ID: "cb", ChatID: -100, UserID: 999, DisplayName: "管理员", Data: telegram.EncodeAdmin(sess.ID, true),
		}); err != nil {
			t.Fatal(err)
		}
		runBackground(t, svc)
		if len(api.banned) != 1 || api.banned[0].until != 0 {
			t.Fatalf("admin_ban_seconds=0 应为永久封禁，实际 %+v", api.banned)
		}
	})
}

// TestBanEffectAuditCarriesSecondsAndReason 验证封禁类副作用的审计带 ban_seconds 与 reason。
func TestBanEffectAuditCarriesSecondsAndReason(t *testing.T) {
	svc, _, registry, _ := testService(t, func(cfg *config.Config) {
		cfg.Gatekeeper.KickUnbanAfterSeconds = 60
	})
	audit := withAudit(svc)
	answerWrong(t, svc, registry)
	runBackground(t, svc)

	var found bool
	for _, ev := range audit.named("effect.done") {
		if ev.Effect == string(domain.EffectBan) {
			found = true
			if ev.Extra["ban_seconds"] != "60" || ev.Extra["reason"] != "kick" {
				t.Fatalf("封禁审计应带 ban_seconds=60、reason=kick，实际 %+v", ev.Extra)
			}
		}
	}
	if !found {
		t.Fatal("没有找到封禁副作用的审计")
	}
}

// TestLegacyBanAndUnbanEffectsStillRun 验证升级前已经落库的"永久封禁 + 独立解封"任务照旧能执行完，
// 不会因为格式变了而卡在 outbox 里。
func TestLegacyBanAndUnbanEffectsStillRun(t *testing.T) {
	svc, api, _, _ := testService(t, nil)
	ctx := context.Background()
	legacyBan := domain.Effect{Kind: domain.EffectBan, Payload: domain.EffectPayload{
		"chat_id": "-100", "user_id": "7", "revoke": "1", "until": "0",
	}}
	legacyUnban := domain.Effect{Kind: domain.EffectUnban, Payload: domain.EffectPayload{
		"chat_id": "-100", "user_id": "7", "delay": "60",
	}}
	if err := svc.doEffect(ctx, legacyBan); err != nil {
		t.Fatal(err)
	}
	if err := svc.doEffect(ctx, legacyUnban); err != nil {
		t.Fatal(err)
	}
	if len(api.banned) != 1 || api.banned[0].until != 0 || !api.banned[0].revoke {
		t.Fatalf("旧格式的封禁应按原样执行，实际 %+v", api.banned)
	}
	if len(api.unbanned) != 1 || api.unbanned[0].userID != 7 {
		t.Fatalf("旧格式的解封应照常执行，实际 %+v", api.unbanned)
	}
}
