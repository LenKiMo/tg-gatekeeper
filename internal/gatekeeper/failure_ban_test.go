package gatekeeper

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
	"github.com/LenKiMo/tg-gatekeeper/internal/store"
)

// ---------------------------------------------------------------- 封禁类测试的公共脚手架

// recordingAudit 把审计事件记在内存里，供测试断言。
type recordingAudit struct {
	mu     sync.Mutex
	events []ports.AuditEvent
}

func (r *recordingAudit) Log(_ context.Context, ev ports.AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recordingAudit) Close() error { return nil }

// named 返回指定名字的审计事件。
func (r *recordingAudit) named(event string) []ports.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ports.AuditEvent
	for _, ev := range r.events {
		if ev.Event == event {
			out = append(out, ev)
		}
	}
	return out
}

// withAudit 给测试用的 Service 接上内存审计。
func withAudit(svc *Service) *recordingAudit {
	rec := &recordingAudit{}
	svc.auditLog = rec
	return rec
}

// pendingEffectKinds 列出 outbox 里（含尚未到点的）副作用类型。
func pendingEffectKinds(t *testing.T, registry *store.MemRegistry) []domain.EffectKind {
	t.Helper()
	effects, err := registry.ListDueEffects(context.Background(), time.Now().Add(365*24*time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]domain.EffectKind, 0, len(effects))
	for _, e := range effects {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

func countKind(kinds []domain.EffectKind, want domain.EffectKind) int {
	n := 0
	for _, k := range kinds {
		if k == want {
			n++
		}
	}
	return n
}

// assertTempBan 断言这次封禁带了大约 seconds 秒后的到期时间（不是永久封禁）。
func assertTempBan(t *testing.T, call apiCall, seconds int64) {
	t.Helper()
	now := time.Now().Unix()
	if call.until < now+seconds-5 || call.until > now+seconds+5 {
		t.Fatalf("封禁应在约 %d 秒后到期（约 %d），实际 until=%d", seconds, now+seconds, call.until)
	}
}

// joinOne 让 7 号用户以入群模式进 -100 群。
func joinOne(t *testing.T, svc *Service) {
	t.Helper()
	if err := svc.OnMembersJoinedWithJoinMessage(context.Background(), -100, 1,
		[]Member{{UserID: 7, DisplayName: "新人"}}, domain.MessageRef{}); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------- 题目发不出去

// errRateLimited 模拟集中入群时 Telegram 的单群限流。
var errRateLimited = errors.New("Too Many Requests: retry after 30")

// TestChallengeSendFailureBansTemporarily 验证题目发不出去时，deny 是定时封禁而不是永久封禁。
//
// 以前这里是 Ban(until=0) 且没有任何解封：机器人自己被限流、网络抖动、题目文案
// 转义写错，都会把刚进群的人永久拉黑。
func TestChallengeSendFailureBansTemporarily(t *testing.T) {
	svc, api, registry, _ := testService(t, nil)
	audit := withAudit(svc)
	api.challengeErr = errRateLimited
	joinOne(t, svc)
	runBackground(t, svc)

	if len(api.banned) != 1 || api.banned[0].userID != 7 {
		t.Fatalf("deny 应当封禁一次，实际 %+v", api.banned)
	}
	assertTempBan(t, api.banned[0], 600)
	if api.banned[0].revoke {
		t.Fatal("机器人自身故障导致的封禁不应撤回对方的历史消息")
	}
	kinds := pendingEffectKinds(t, registry)
	if countKind(kinds, domain.EffectUnban) != 0 || countKind(kinds, domain.EffectBan) != 0 {
		t.Fatalf("定时封禁由 Telegram 到期解除，不应再排封禁/解封任务，实际 %v", kinds)
	}
	if len(api.unbanned) != 0 {
		t.Fatalf("不应调用解封，实际 %+v", api.unbanned)
	}
	if _, err := registry.Get(context.Background(), domain.SessionKey{ChatID: -100, UserID: 7}); err == nil {
		t.Fatal("题目没发出去，不应留下活动会话")
	}

	events := audit.named("verify.challenge_failed")
	if len(events) != 1 {
		t.Fatalf("应当有一条 verify.challenge_failed 审计，实际 %+v", events)
	}
	ev := events[0]
	if ev.Result != "ban" || ev.ErrorClass != "rate_limited" || ev.UserID != 7 || ev.ChatID != -100 {
		t.Fatalf("审计内容不对: %+v", ev)
	}
	if ev.Extra["ban_seconds"] != "600" || ev.Extra["reason"] != "challenge_send_failed" || len(ev.Extra) != 2 {
		t.Fatalf("审计应当只带 ban_seconds 与 reason（不带错误原文），实际 %+v", ev.Extra)
	}
}

// TestChallengeSendFailureHonoursConfiguredSeconds 验证封禁时长取自 failure_ban_seconds。
func TestChallengeSendFailureHonoursConfiguredSeconds(t *testing.T) {
	svc, api, _, _ := testService(t, func(cfg *config.Config) {
		cfg.Gatekeeper.FailureBanSeconds = 120
	})
	api.challengeErr = errRateLimited
	joinOne(t, svc)

	if len(api.banned) != 1 {
		t.Fatalf("应当封禁一次，实际 %+v", api.banned)
	}
	assertTempBan(t, api.banned[0], 120)
}

// TestChallengeSendFailureAllowRestores 验证 failure_policy=allow 仍是恢复权限、不封禁。
func TestChallengeSendFailureAllowRestores(t *testing.T) {
	svc, api, _, _ := testService(t, func(cfg *config.Config) {
		cfg.Gatekeeper.FailurePolicy = "allow"
	})
	audit := withAudit(svc)
	api.challengeErr = errRateLimited
	joinOne(t, svc)
	runBackground(t, svc)

	if len(api.banned) != 0 {
		t.Fatalf("allow 不应封禁，实际 %+v", api.banned)
	}
	if len(api.restored) != 1 || api.restored[0].userID != 7 {
		t.Fatalf("allow 应当恢复权限，实际 %+v", api.restored)
	}
	if events := audit.named("verify.challenge_failed"); len(events) != 1 || events[0].Result != "allow" {
		t.Fatalf("应当有一条 result=allow 的审计，实际 %+v", events)
	}
}

// TestChallengeSendFailureBanErrorIsSwallowed 验证封不了（目标是管理员等，Telegram 回 403）
// 时错误被吞掉、只记审计，并且不会重试成请求风暴。
func TestChallengeSendFailureBanErrorIsSwallowed(t *testing.T) {
	svc, api, registry, _ := testService(t, nil)
	audit := withAudit(svc)
	api.challengeErr = errRateLimited
	api.banErr = errors.New("Forbidden: not enough rights to restrict/unrestrict chat member")
	joinOne(t, svc)
	// 多跑几轮后台循环：封禁失败不应被重新排队。
	for i := 0; i < 3; i++ {
		runBackground(t, svc)
	}

	if len(api.banned) != 1 {
		t.Fatalf("封禁只应尝试一次，实际 %d 次: %+v", len(api.banned), api.banned)
	}
	if kinds := pendingEffectKinds(t, registry); len(kinds) != 0 {
		t.Fatalf("封禁失败不应留下待重试的任务，实际 %v", kinds)
	}
	events := audit.named("verify.challenge_failed")
	if len(events) != 1 || events[0].Result != "ban_failed" || events[0].ErrorClass != "forbidden" {
		t.Fatalf("应当记一条 result=ban_failed、error_class=forbidden 的审计，实际 %+v", events)
	}
}
