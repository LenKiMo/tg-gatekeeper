package gatekeeper

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/challenge"
	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
	"github.com/LenKiMo/tg-gatekeeper/internal/store"
)

// brokenImages 是永远取不到图的图片管线（模拟图床挂了 / 题库图片全坏）。
type brokenImages struct{ err error }

func (b brokenImages) Resolve(context.Context, string) (ports.ResolvedImage, error) {
	if b.err != nil {
		return ports.ResolvedImage{}, b.err
	}
	return ports.ResolvedImage{}, errors.New("图片服务不可用")
}
func (brokenImages) Close() error { return nil }

// emptyDataset 把题库换成空的（模拟数据源返回了空数据集）。
func emptyDataset(svc *Service) {
	svc.gen.Store(challenge.New(domain.Snapshot{SourceID: "empty", LoadedAt: time.Now()}))
}

// assertNoSession 断言没有为 7 号用户留下任何会话（活动的、终结的都没有）。
func assertNoSession(t *testing.T, registry *store.MemRegistry) {
	t.Helper()
	ctx := context.Background()
	if _, err := registry.Get(ctx, domain.SessionKey{ChatID: -100, UserID: 7}); err == nil {
		t.Fatal("出不了题不应留下活动会话")
	}
	active, err := registry.ListActive(ctx, time.Now(), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("会话表里不应有新记录，实际 %+v", active)
	}
}

// TestGenerateFailureBansTemporarily 验证出不了题时 failure_policy=deny 真的把人挡在门外。
//
// 这类失败发生在禁言和建会话之前：以前直接返回错误，新人就不经验证留在了群里。
func TestGenerateFailureBansTemporarily(t *testing.T) {
	cases := map[string]func(*Service){
		"题库为空":  emptyDataset,
		"图片取不到": func(svc *Service) { svc.images = brokenImages{} },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			svc, api, registry, _ := testService(t, nil)
			audit := withAudit(svc)
			breakIt(svc)
			joinOne(t, svc)
			runBackground(t, svc)

			if len(api.banned) != 1 || api.banned[0].userID != 7 {
				t.Fatalf("deny 应当封禁一次，实际 %+v", api.banned)
			}
			assertTempBan(t, api.banned[0], 600)
			if api.banned[0].revoke {
				t.Fatal("机器人自身故障导致的封禁不应撤回对方的历史消息")
			}
			if len(api.restricted) != 0 || len(api.challenges) != 0 {
				t.Fatalf("出不了题不应禁言/发题，实际 restricted=%+v challenges=%+v", api.restricted, api.challenges)
			}
			assertNoSession(t, registry)
			if kinds := pendingEffectKinds(t, registry); len(kinds) != 0 {
				t.Fatalf("不应留下任何待执行的副作用，实际 %v", kinds)
			}

			events := audit.named("verify.challenge_failed")
			if len(events) != 1 {
				t.Fatalf("应当有一条 verify.challenge_failed 审计，实际 %+v", events)
			}
			ev := events[0]
			if ev.Result != "ban" || ev.Extra["ban_seconds"] != "600" || ev.Extra["reason"] != "challenge_generate_failed" {
				t.Fatalf("审计内容不对: %+v", ev)
			}
		})
	}
}

// TestGenerateFailureAllowLetsIn 验证 failure_policy=allow 维持原样：放行、不封禁，
// 也不去"恢复"一个从没被禁言的人。
func TestGenerateFailureAllowLetsIn(t *testing.T) {
	svc, api, registry, _ := testService(t, func(cfg *config.Config) {
		cfg.Gatekeeper.FailurePolicy = "allow"
	})
	audit := withAudit(svc)
	emptyDataset(svc)
	joinOne(t, svc)
	runBackground(t, svc)

	if len(api.banned) != 0 || len(api.restricted) != 0 || len(api.restored) != 0 {
		t.Fatalf("allow 应当原样放行，实际 banned=%+v restricted=%+v restored=%+v", api.banned, api.restricted, api.restored)
	}
	assertNoSession(t, registry)
	if events := audit.named("verify.challenge_failed"); len(events) != 1 || events[0].Result != "allow" {
		t.Fatalf("应当有一条 result=allow 的审计，实际 %+v", events)
	}
}

// TestGenerateFailureRequestModeOnlyDeclines 验证申请模式不受影响：人还没进群，只拒绝申请。
func TestGenerateFailureRequestModeOnlyDeclines(t *testing.T) {
	svc, api, _, _ := testService(t, nil)
	emptyDataset(svc)
	ctx := context.Background()
	group := mustGroup(t, svc, -300)
	group.Mode = domain.VerifyModeRequest
	if err := svc.SaveGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	if err := svc.OnJoinRequest(ctx, JoinRequest{ChatID: -300, UserID: 20, UserChatID: 20, DisplayName: "申请者", UpdateID: 5}); err != nil {
		t.Fatal(err)
	}
	if len(api.declined) != 1 || len(api.banned) != 0 {
		t.Fatalf("申请模式应当只拒绝申请，实际 declined=%+v banned=%+v", api.declined, api.banned)
	}
}

// TestGenerateFailureWarnsWithoutLeakingToken 验证出不了题时有一条带 chat_id 的 Warn 日志
// （便于发现"基础设施故障引发批量封禁"），并且错误文本里的 bot token 被脱敏。
func TestGenerateFailureWarnsWithoutLeakingToken(t *testing.T) {
	svc, _, _, _ := testService(t, func(cfg *config.Config) {
		cfg.Bot.Token = "123456:SECRET-TOKEN"
	})
	var logs bytes.Buffer
	svc.log = slog.New(slog.NewTextHandler(&logs, nil))
	svc.images = brokenImages{err: errors.New(`Get "https://api.telegram.org/bot123456:SECRET-TOKEN/getFile": timeout`)}
	joinOne(t, svc)

	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "chat_id=-100") {
		t.Fatalf("应当有一条带 chat_id 的 Warn 日志，实际:\n%s", out)
	}
	if !strings.Contains(out, "出不了题") {
		t.Fatalf("Warn 日志应当说明是出不了题，实际:\n%s", out)
	}
	// 只看这条专门的告警：调用方那条通用的"处理入群失败"日志不在本次改动范围内。
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "出不了题") && strings.Contains(line, "SECRET-TOKEN") {
			t.Fatalf("告警日志里不应出现 bot token，实际: %s", line)
		}
	}
	if !strings.Contains(out, "[token]") {
		t.Fatalf("错误文本里的 token 应当被替换成占位符，实际:\n%s", out)
	}
}
