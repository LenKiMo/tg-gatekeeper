package gatekeeper

// "各层互相兜底"的核心回归：入群账本（ledger）在任何中途失败后都不能变成静默放行。
//
// 事故原型：预检阶段 getChat 失败 + 禁言失败 → 没有会话、没有副作用 → 人在群里待了
// 14 小时。修法是入群第一刻先落一条 Preparing 空壳会话，此后无论哪一步失败，它都会
// 在 expires_at 到期后被 scanExpired 当成超时处置（入群模式 = 踢出）。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
)

// TestAdBanSurvivesTransientAPIError 广告命中的封禁走 outbox：第一次 API 调用失败时，
// 不能"只留一行日志"，必须留在 effects 表里等重试。
//
// 这是 Judy Bach 事故的另一半：以前 rejectByFilter 直接调一次 api.Ban，429 之后就只剩
// 审计里一条失败，人在群里。现在封禁是 outbox 里的待办。
func TestAdBanSurvivesTransientAPIError(t *testing.T) {
	svc, api, reg, _ := testService(t, func(cfg *config.Config) {
		cfg.AdFilter.Enabled = true
		cfg.Gatekeeper.AdWordBanDays = 30
	})
	api.banErr = errFakeTransient // 第一次封禁失败（模拟 429/网络抖动）

	if err := svc.OnMembersJoined(context.Background(), -100, 1,
		[]Member{{UserID: 7, DisplayName: "催收直盘 长期招快递车队合作"}}); err != nil {
		t.Fatal(err)
	}

	// 会话必须已经在终态（不会被当成"还在等答题"），题目不发。
	if len(api.challenges) != 0 {
		t.Fatalf("预检命中不该出题，实际 %d 道", len(api.challenges))
	}
	if _, err := reg.Get(context.Background(), domain.SessionKey{ChatID: -100, UserID: 7}); err == nil {
		t.Fatal("会话应已进入终态，不再占着活跃会话槽")
	}

	// 封禁失败但没丢：effects 表里还有待重试的条目（把时间往后推一小时仍能取到，
	// 说明它只是被退避延后，而不是被当成已完成）。
	pending, err := reg.ListDueEffects(context.Background(), time.Now().Add(time.Hour), 20)
	if err != nil {
		t.Fatalf("读取待执行副作用失败: %v", err)
	}
	found := false
	for _, ef := range pending {
		if ef.Kind == domain.EffectBan {
			found = true
		}
	}
	if !found {
		t.Fatalf("封禁失败后应留在 outbox 等待重试，实际待执行 %+v", pending)
	}

	// 抖动过去之后，下一轮 effectLoop 应把它补上。
	api.banErr = nil
	runBackground(t, svc)
	ok := 0
	for _, b := range api.banned {
		if b.userID == 7 {
			ok++
		}
	}
	if ok < 1 {
		t.Fatalf("重试后应至少成功封禁一次，实际 %+v", api.banned)
	}
}

// TestStaleLedgerIsKickedByTimeoutScan 模拟"预检中途进程被杀"：重启后扫到的是一条
// 已过期的 Preparing 空壳，超时扫描必须把它踢掉。
//
// 这是整套兜底的最后一环，也是唯一不依赖"流程走完"的一环。
func TestStaleLedgerIsKickedByTimeoutScan(t *testing.T) {
	svc, api, reg, _ := testService(t, nil)

	sess := domain.Session{
		ID:          "ledger-stale",
		Key:         domain.SessionKey{ChatID: -100, UserID: 7},
		UpdateID:    0, // 账本空壳的标记
		Mode:        domain.SessionJoin,
		State:       domain.StatePreparing,
		DisplayName: "Judy Bach",
		StartedAt:   time.Now().Add(-10 * time.Minute),
		ExpiresAt:   time.Now().Add(-time.Minute), // 已过期
		Version:     1,
	}
	if _, err := reg.Begin(context.Background(), sess); err != nil {
		t.Fatalf("写入账本失败: %v", err)
	}

	runBackground(t, svc) // scanExpired + runDueEffects

	if len(api.banned) != 1 {
		t.Fatalf("过期账本应被超时扫描踢出，实际 banned=%d", len(api.banned))
	}
	if got := api.banned[0].userID; got != 7 {
		t.Fatalf("踢错人了，实际 user_id=%d", got)
	}
}

// TestLedgerExistsWhilePrecheckFails 预检阶段 getChat 失败时，账本必须已经在库里。
//
// 断言的是"有没有账"，不是"有没有处置"：处置由后续层（超时扫描）负责，但前提是
// 这条记录存在。以前失败路径上什么都不留，所以连兜底的机会都没有。
func TestLedgerExistsWhilePrecheckFails(t *testing.T) {
	svc, api, reg, _ := testService(t, func(cfg *config.Config) {
		cfg.AdFilter.Enabled = true
		// 保留默认的 audit_continue：getChat 失败不拦人，继续出题。
	})
	api.profileErr = errors.New("dial tcp: i/o timeout")

	if err := svc.OnMembersJoined(context.Background(), -100, 1,
		[]Member{{UserID: 7, DisplayName: "Judy Bach"}}); err != nil {
		t.Fatal(err)
	}

	// 正常路径：账本被正式会话原子替换，题目照发。
	if len(api.challenges) != 1 {
		t.Fatalf("getChat 失败但仍应出题（audit_continue），实际 %d 道", len(api.challenges))
	}
	cur, err := reg.Get(context.Background(), domain.SessionKey{ChatID: -100, UserID: 7})
	if err != nil {
		t.Fatalf("应当有一条进行中的会话（账本或正式会话）: %v", err)
	}
	if cur.State != domain.StatePending {
		t.Fatalf("正式会话应进入 pending，实际 %s", cur.State)
	}
	if cur.UpdateID == 0 {
		t.Fatal("正式会话不该保留账本的 UpdateID=0（那会被当成空壳再次被替换）")
	}
}

// TestLedgerDoesNotBlockSecondJoinSignal 账本空壳不能被误判成"重复入群信号"。
//
// 历史 bug 面：startVerification 里"同 key 已有非终态会话 → 当作重复信号直接返回"，
// 如果账本也算数，那么每个正常入群都会被判重复、一道题都不发。
func TestLedgerDoesNotBlockSecondJoinSignal(t *testing.T) {
	svc, api, _, _ := testService(t, nil)
	ctx := context.Background()

	if err := svc.OnMembersJoined(ctx, -100, 1,
		[]Member{{UserID: 7, DisplayName: "新人"}}); err != nil {
		t.Fatal(err)
	}
	if len(api.challenges) != 1 {
		t.Fatalf("首次入群应出 1 道题，实际 %d", len(api.challenges))
	}

	// 同一用户已经有进行中的正式会话：再来一次入群信号不应再出题。
	if err := svc.OnMembersJoined(ctx, -100, 2,
		[]Member{{UserID: 7, DisplayName: "新人"}}); err != nil {
		t.Fatal(err)
	}
	if len(api.challenges) != 1 {
		t.Fatalf("重复入群信号不该再出题，实际 %d 道", len(api.challenges))
	}
}
