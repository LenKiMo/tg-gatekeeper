package gatekeeper

// 禁言失败时的处置回归。
//
// 生产事故原型（2026-11-17 01:55，uid 9266676380 "Judy Bach"）：
//   getChat 失败 → verify.bio_lookup_failed(audit_continue) → 继续
//   RestrictNoMessages 失败 → 老代码"按跳过验证处理" → 不发题、不处置，人留在群里
// 该号当时的 bio 是 "https://t.me/+… 来单有码就能做随便一天挣九千"。
//
// 老代码的注释写的是"避免误踢管理员"，但它**没有真的去确认对方是不是管理员**——
// 任何失败都当管理员处理。这里把"确认身份"补上，并把非管理员按 failure_policy 处置。

import (
	"context"
	"errors"
	"testing"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
)

// TestRestrictFailedNonAdminDenyBans 禁言失败且不是管理员：按 failure_policy=deny 踢出。
func TestRestrictFailedNonAdminDenyBans(t *testing.T) {
	svc, api, _, _ := testService(t, func(cfg *config.Config) {
		cfg.Gatekeeper.FailurePolicy = "deny"
		cfg.Gatekeeper.FailureBanSeconds = 600
	})
	api.restrictErr = errors.New("Too Many Requests: retry after 30")

	// OnMembersJoined 对单个成员的失败只记日志、不向上返回（多成员事件里一个失败
	// 不该中断其余人），所以这里断言的是副作用而不是错误值。
	if err := svc.OnMembersJoined(context.Background(), -100, 1,
		[]Member{{UserID: 7, DisplayName: "Judy Bach"}}); err != nil {
		t.Fatalf("OnMembersJoined 不该把单个成员的错误冒泡上来: %v", err)
	}
	if len(api.challenges) != 0 {
		t.Fatalf("禁言失败时不该发题，实际 %d 道", len(api.challenges))
	}
	if len(api.banned) != 1 {
		t.Fatalf("非管理员 + deny 应当踢出，实际 banned=%d", len(api.banned))
	}
	until := api.banned[0].until
	if until == 0 {
		t.Fatal("应按 failure_ban_seconds 临时封禁，实际永久（until=0）")
	}
}

// TestRestrictFailedAdminIsSkipped 真是管理员：跳过验证，不踢。
func TestRestrictFailedAdminIsSkipped(t *testing.T) {
	svc, api, _, _ := testService(t, func(cfg *config.Config) {
		cfg.Gatekeeper.FailurePolicy = "deny"
	})
	api.restrictErr = errors.New("Bad Request: can't restrict chat creator")
	api.admins[7] = true

	if err := svc.OnMembersJoined(context.Background(), -100, 1,
		[]Member{{UserID: 7, DisplayName: "群主"}}); err != nil {
		t.Fatalf("管理员应被跳过而不是报错: %v", err)
	}
	if len(api.banned) != 0 {
		t.Fatalf("管理员不该被踢，实际 %+v", api.banned)
	}
	if len(api.challenges) != 0 {
		t.Fatalf("跳过验证时不该发题，实际 %d 道", len(api.challenges))
	}
}

// TestRestrictFailedNonAdminAllowPasses allow 策略下不踢（显式配置的取舍）。
func TestRestrictFailedNonAdminAllowPasses(t *testing.T) {
	svc, api, _, _ := testService(t, func(cfg *config.Config) {
		cfg.Gatekeeper.FailurePolicy = "allow"
	})
	api.restrictErr = errors.New("Too Many Requests: retry after 30")

	if err := svc.OnMembersJoined(context.Background(), -100, 1,
		[]Member{{UserID: 7, DisplayName: "Judy Bach"}}); err != nil {
		t.Fatalf("allow 策略下不该报错: %v", err)
	}
	if len(api.banned) != 0 {
		t.Fatalf("allow 策略下不该踢人，实际 %+v", api.banned)
	}
	// allow 下会话要被清理掉，不能留一个 Preparing 的僵尸会话。
	if s, err := svc.registry.GetByID(context.Background(), ""); err == nil && s.State == domain.StatePreparing {
		t.Fatal("allow 策略下不该留下未决会话")
	}
}
