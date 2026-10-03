package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
)

// tempDir 返回一个测试用临时目录。
//
// 不用 t.TempDir()：在 Windows 上 SQLite 的 -wal/-shm 文件在 Close 之后可能仍被短暂
// 占用，导致 t.TempDir() 的 RemoveAll 清理失败并把测试判为失败。这里忽略清理错误。
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tg-gatekeeper-test-")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func testSQL(t *testing.T) *SQL {
	t.Helper()
	cfg := config.Default()
	cfg.Storage.SQLite.Path = filepath.Join(tempDir(t), "state.db")
	cfg.Storage.AutoMigrate = true
	h, err := Open(context.Background(), cfg, "sqlite")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func newSession(id string, chatID, userID int64, updateID int, mode domain.SessionMode, expires time.Time) domain.Session {
	token, _ := domain.NewOptionToken()
	return domain.Session{
		ID:               id,
		Key:              domain.SessionKey{ChatID: chatID, UserID: userID},
		UpdateID:         updateID,
		Mode:             mode,
		State:            domain.StatePreparing,
		Options:          []domain.Option{{Token: token, Label: "甲"}, {Token: "tok2", Label: "乙"}},
		CorrectTokenHash: domain.HashToken(token),
		DatasetRevision:  "rev1",
		StartedAt:        time.Now(),
		ExpiresAt:        expires,
		Version:          1,
	}
}

// TestOpenCreatesMissingParentDirectory 验证"全新克隆/自定义路径"也能启动：
// 父目录不存在时 SQLite 只会给一句 "unable to open database file"，曾把首次部署坑住。
func TestOpenCreatesMissingParentDirectory(t *testing.T) {
	ctx := context.Background()
	dir := tempDir(t)
	nested := filepath.Join(dir, "var", "state", "gatekeeper.db")
	cfg := config.Default()
	cfg.Storage.SQLite.Path = nested

	h, err := Open(ctx, cfg, "sqlite")
	if err != nil {
		t.Fatalf("父目录不存在时应自动创建: %v", err)
	}
	defer func() { _ = h.Close() }()
	if _, err := os.Stat(nested); err != nil {
		t.Fatalf("数据库文件未创建: %v", err)
	}
}

// TestGroupStoreRevisionCAS 验证群配置的乐观并发（读-改-写冲突必须被发现）。
func TestGroupStoreRevisionCAS(t *testing.T) {
	ctx := context.Background()
	g := NewGroupStore(testSQL(t))

	created, err := g.EnsureGroup(ctx, -100, domain.GroupConfig{Enabled: true, Mode: domain.VerifyModeJoin})
	if err != nil {
		t.Fatalf("EnsureGroup 失败: %v", err)
	}
	if created.Revision != 1 {
		t.Fatalf("新建 revision = %d，期望 1", created.Revision)
	}
	// 幂等：再次 Ensure 不会覆盖已有配置。
	again, err := g.EnsureGroup(ctx, -100, domain.GroupConfig{Enabled: false})
	if err != nil || !again.Enabled {
		t.Fatalf("EnsureGroup 应当返回已存在的配置: %+v err=%v", again, err)
	}

	updated, err := g.SaveGroup(ctx, domain.GroupConfig{ChatID: -100, Enabled: true, Mode: domain.VerifyModeRequest, Tag: "已验证"}, created.Revision)
	if err != nil {
		t.Fatalf("SaveGroup 失败: %v", err)
	}
	if updated.Revision != 2 || updated.Mode != domain.VerifyModeRequest || updated.Tag != "已验证" {
		t.Fatalf("更新结果异常: %+v", updated)
	}
	// 用过期 revision 写入必须冲突。
	if _, err := g.SaveGroup(ctx, domain.GroupConfig{ChatID: -100}, created.Revision); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("过期 revision 写入应当返回 ErrConflict，实际 %v", err)
	}
	if _, err := g.GetGroup(ctx, -999); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("不存在的群应当返回 ErrNotFound，实际 %v", err)
	}
}

// TestBeginIsIdempotentPerUpdate 验证同一条 update 被重复投递时不会重复出题。
func TestBeginIsIdempotentPerUpdate(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry(testSQL(t))

	expires := time.Now().Add(time.Minute)
	first, err := r.Begin(ctx, newSession("s1", -100, 7, 42, domain.SessionJoin, expires))
	if err != nil {
		t.Fatalf("Begin 失败: %v", err)
	}
	if first.Duplicate {
		t.Fatal("首次 Begin 不应是重复")
	}
	second, err := r.Begin(ctx, newSession("s2", -100, 7, 42, domain.SessionJoin, expires))
	if err != nil {
		t.Fatalf("重复 Begin 失败: %v", err)
	}
	if !second.Duplicate || second.Session.ID != "s1" {
		t.Fatalf("同 update 重复投递应当返回原会话，实际 %+v", second)
	}
}

// TestBeginReplacesOldGeneration 验证重新入群会替换旧世代并作废旧副作用。
func TestBeginReplacesOldGeneration(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry(testSQL(t))

	if _, err := r.Begin(ctx, newSession("old", -100, 7, 1, domain.SessionJoin, time.Now().Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	// 给旧会话写入一条待执行副作用。
	if _, err := r.Claim(ctx, domain.SessionKey{ChatID: -100, UserID: 7}, domain.Event{
		Kind: domain.EventAnswer, SessionID: "old", OptionToken: "nope", At: time.Now(),
	}, func(before, after domain.Session, ev domain.Event) []domain.Effect {
		return []domain.Effect{{Kind: domain.EffectBan, Payload: domain.EffectPayload{"chat_id": "-100"}, DueAt: time.Now().Add(time.Hour)}}
	}); err != nil {
		t.Fatal(err)
	}

	res, err := r.Begin(ctx, newSession("new", -100, 7, 2, domain.SessionJoin, time.Now().Add(time.Minute)))
	if err != nil {
		t.Fatalf("替换 Begin 失败: %v", err)
	}
	if res.Replaced == nil || res.Replaced.ID != "old" {
		t.Fatalf("应当报告被替换的旧会话，实际 %+v", res.Replaced)
	}
	old, err := r.GetByID(ctx, "old")
	if err != nil {
		t.Fatalf("GetByID 失败: %v", err)
	}
	if old.State != domain.StateCancelled {
		t.Fatalf("旧会话状态 = %s，期望 cancelled", old.State)
	}
	effects, err := r.ListDueEffects(ctx, time.Now().Add(2*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(effects) != 0 {
		t.Fatalf("旧世代的未执行副作用应当被作废，仍有 %d 条", len(effects))
	}
}

// TestClaimOnlyOneWinnerConcurrent 是并发正确性的核心测试：
// 64 个 goroutine 同时用正确答案点击同一个会话，只能有一个赢。
func TestClaimOnlyOneWinnerConcurrent(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry(testSQL(t))

	expires := time.Now().Add(time.Minute)
	sess := newSession("race", -100, 7, 1, domain.SessionJoin, expires)
	token, _ := domain.NewOptionToken()
	sess.Options = []domain.Option{{Token: token, Label: "甲"}, {Token: "other", Label: "乙"}}
	sess.CorrectTokenHash = domain.HashToken(token)
	if _, err := r.Begin(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AttachMessages(ctx, sess.Key, sess.ID, domain.MessageRef{ChatID: -100, MessageID: 555},
		domain.MessageRef{}, expires, 1); err != nil {
		t.Fatalf("AttachMessages 失败: %v", err)
	}

	var wins atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := r.Claim(ctx, sess.Key, domain.Event{
				Kind: domain.EventAnswer, SessionID: sess.ID, ActorID: 7, OptionToken: token, At: time.Now(),
			}, func(before, after domain.Session, ev domain.Event) []domain.Effect {
				return []domain.Effect{{Kind: domain.EffectRestorePermissions, Payload: domain.EffectPayload{"chat_id": "-100", "user_id": "7"}, DueAt: time.Now()}}
			})
			if err != nil {
				t.Errorf("Claim 报错: %v", err)
				return
			}
			if res.Won {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := wins.Load(); got != 1 {
		t.Fatalf("赢得终态的次数 = %d，期望恰好 1（并发点击必须只有一个生效）", got)
	}
	after, err := r.GetByID(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != domain.StatePassed {
		t.Fatalf("终态 = %s，期望 passed", after.State)
	}
	effects, err := r.ListDueEffects(ctx, time.Now().Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(effects) != 1 {
		t.Fatalf("副作用条数 = %d，期望 1（不能重复放行）", len(effects))
	}
}

// TestClaimExpiredOnlyTimeoutWins 验证"到点之后答对也不放行"（避免超时与答对同时生效）。
func TestClaimExpiredOnlyTimeoutWins(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry(testSQL(t))

	expires := time.Now().Add(-time.Second) // 已过期
	sess := newSession("exp", -100, 7, 1, domain.SessionJoin, expires)
	if _, err := r.Begin(ctx, sess); err != nil {
		t.Fatal(err)
	}
	// 先进入 pending（题目已发出），再模拟"用户迟到答对"。
	if _, err := r.AttachMessages(ctx, sess.Key, sess.ID, domain.MessageRef{ChatID: -100, MessageID: 9},
		domain.MessageRef{}, expires, 1); err != nil {
		t.Fatal(err)
	}
	res, err := r.Claim(ctx, sess.Key, domain.Event{
		Kind: domain.EventAnswer, SessionID: "exp", ActorID: 7,
		OptionToken: sess.Options[0].Token, At: time.Now(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Won || res.Reason != ports.ClaimExpired {
		t.Fatalf("过期后答题应当被拒绝（reason=expired），实际 won=%v reason=%s", res.Won, res.Reason)
	}
	timeout, err := r.Claim(ctx, sess.Key, domain.Event{Kind: domain.EventTimeout, SessionID: "exp", At: time.Now()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !timeout.Won || timeout.After.State != domain.StateTimedOut {
		t.Fatalf("到点后的 timeout 应当赢得 timed_out，实际 won=%v state=%s", timeout.Won, timeout.After.State)
	}
	// 未到点的 timeout 不能赢。
	sess2 := newSession("fresh", -100, 8, 2, domain.SessionJoin, time.Now().Add(time.Minute))
	if _, err := r.Begin(ctx, sess2); err != nil {
		t.Fatal(err)
	}
	tooEarly, err := r.Claim(ctx, sess2.Key, domain.Event{Kind: domain.EventTimeout, SessionID: "fresh", At: time.Now()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tooEarly.Won || tooEarly.Reason != ports.ClaimNotDue {
		t.Fatalf("未到点的 timeout 不应生效，实际 won=%v reason=%s", tooEarly.Won, tooEarly.Reason)
	}
}

// TestClaimStaleSessionIDIsReplaced 验证旧题按钮不能作用到新会话。
func TestClaimStaleSessionIDIsReplaced(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry(testSQL(t))

	if _, err := r.Begin(ctx, newSession("gen1", -100, 7, 1, domain.SessionJoin, time.Now().Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	newSess := newSession("gen2", -100, 7, 2, domain.SessionJoin, time.Now().Add(time.Minute))
	if _, err := r.Begin(ctx, newSess); err != nil {
		t.Fatal(err)
	}
	res, err := r.Claim(ctx, newSess.Key, domain.Event{
		Kind: domain.EventAdminPass, SessionID: "gen1", ActorID: 999, At: time.Now(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Won || res.Reason != ports.ClaimReplaced {
		t.Fatalf("旧 session id 的操作应被拒绝，实际 won=%v reason=%s", res.Won, res.Reason)
	}
}

// TestClaimBadTokenAndWrongAnswer 验证非法 token 与答错的区别。
func TestClaimBadTokenAndWrongAnswer(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry(testSQL(t))
	sess := newSession("t1", -100, 7, 1, domain.SessionJoin, time.Now().Add(time.Minute))
	if _, err := r.Begin(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AttachMessages(ctx, sess.Key, sess.ID, domain.MessageRef{ChatID: -100, MessageID: 1}, domain.MessageRef{}, sess.ExpiresAt, 1); err != nil {
		t.Fatal(err)
	}
	bad, err := r.Claim(ctx, sess.Key, domain.Event{Kind: domain.EventAnswer, SessionID: "t1", ActorID: 7, OptionToken: "not-a-token", At: time.Now()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bad.Won || bad.Reason != ports.ClaimBadToken {
		t.Fatalf("非法 token 应返回 bad_token，实际 %v %s", bad.Won, bad.Reason)
	}
	wrong, err := r.Claim(ctx, sess.Key, domain.Event{Kind: domain.EventAnswer, SessionID: "t1", ActorID: 7, OptionToken: sess.Options[1].Token, At: time.Now()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !wrong.Won || wrong.After.State != domain.StateWrong {
		t.Fatalf("答错应当赢得 wrong 终态，实际 won=%v state=%s", wrong.Won, wrong.After.State)
	}
	// 已经终结后再次点击：应当返回 already_done。
	dup, err := r.Claim(ctx, sess.Key, domain.Event{Kind: domain.EventAnswer, SessionID: "t1", ActorID: 7, OptionToken: sess.Options[1].Token, At: time.Now()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dup.Won || dup.Reason != ports.ClaimDone {
		t.Fatalf("重复点击应返回 already_done，实际 %v %s", dup.Won, dup.Reason)
	}
}

// TestCancelEffectsOf 验证永久封禁后撤销临时解封。
func TestCancelEffectsOf(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry(testSQL(t))
	sess := newSession("c1", -100, 7, 1, domain.SessionJoin, time.Now().Add(time.Minute))
	if _, err := r.Begin(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AttachMessages(ctx, sess.Key, sess.ID, domain.MessageRef{ChatID: -100, MessageID: 1}, domain.MessageRef{}, sess.ExpiresAt, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Claim(ctx, sess.Key, domain.Event{Kind: domain.EventAnswer, SessionID: "c1", ActorID: 7, OptionToken: sess.Options[1].Token, At: time.Now()},
		func(before, after domain.Session, ev domain.Event) []domain.Effect {
			return []domain.Effect{
				{Kind: domain.EffectBan, DueAt: time.Now()},
				{Kind: domain.EffectUnban, DueAt: time.Now().Add(time.Hour)},
			}
		}); err != nil {
		t.Fatal(err)
	}
	n, err := r.CancelEffectsOf(ctx, "c1", domain.EffectUnban)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("撤销条数 = %d，期望 1", n)
	}
	due, err := r.ListDueEffects(ctx, time.Now().Add(2*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Kind != domain.EffectBan {
		t.Fatalf("剩余副作用应当只有封禁，实际 %+v", due)
	}
}

// TestRegistrySurvivesRestart 验证进程重启后待验证会话仍可恢复（防止"重启即放行"）。
func TestRegistrySurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := tempDir(t)
	cfg := config.Default()
	cfg.Storage.SQLite.Path = filepath.Join(dir, "state.db")

	h1, err := Open(ctx, cfg, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	r1 := NewRegistry(h1)
	sess := newSession("restart", -100, 7, 1, domain.SessionJoin, time.Now().Add(time.Minute))
	if _, err := r1.Begin(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := h1.Close(); err != nil {
		t.Fatal(err)
	}

	h2, err := Open(ctx, cfg, "sqlite")
	if err != nil {
		t.Fatalf("重开后打开失败: %v", err)
	}
	defer func() { _ = h2.Close() }()
	r2 := NewRegistry(h2)
	got, err := r2.Get(ctx, sess.Key)
	if err != nil {
		t.Fatalf("重启后应当仍能读到会话: %v", err)
	}
	if got.ID != "restart" {
		t.Fatalf("会话 id = %s", got.ID)
	}
	active, err := r2.ListActive(ctx, time.Now().Add(2*time.Minute), true, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 {
		t.Fatalf("过期扫描应当发现 1 个会话，实际 %d", len(active))
	}
}

// TestDeletionQueueLeaseAckNack 验证删除队列的租约语义。
func TestDeletionQueueLeaseAckNack(t *testing.T) {
	ctx := context.Background()
	q := NewDeletionQueue(testSQL(t))

	ref := domain.MessageRef{ChatID: -100, MessageID: 42}
	if err := q.Enqueue(ctx, ref, time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	// 重复入队幂等。
	if err := q.Enqueue(ctx, ref, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("重复入队应当幂等: %v", err)
	}
	jobs, err := q.LeaseDue(ctx, time.Now(), 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("到期任务数 = %d，期望 1（重复入队不应产生两条）", len(jobs))
	}
	// 租约内不会被再次领取。
	again, err := q.LeaseDue(ctx, time.Now(), 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("租约内不应被重复领取，实际 %d", len(again))
	}
	// Nack 之后可以再次领取。
	if err := q.Nack(ctx, jobs[0].ID, time.Now().Add(-time.Second), "telegram 500"); err != nil {
		t.Fatal(err)
	}
	retried, err := q.LeaseDue(ctx, time.Now(), 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(retried) != 1 || retried[0].Attempts != 1 {
		t.Fatalf("重试领取异常: %+v", retried)
	}
	if err := q.Ack(ctx, jobs[0].ID); err != nil {
		t.Fatal(err)
	}
	done, err := q.LeaseDue(ctx, time.Now(), 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 0 {
		t.Fatalf("Ack 后不应再领取，实际 %d", len(done))
	}
}

// TestYamlStoreRoundTrip 验证 YAML 模式可读写且 CAS 生效。
func TestYamlStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Storage.YAML.Path = filepath.Join(tempDir(t), "groups.yaml")
	cfg.Storage.YAML.RuntimeWrites = true

	s, err := NewYAMLStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.EnsureGroup(ctx, -100, domain.GroupConfig{Enabled: true, Mode: domain.VerifyModeJoin})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveGroup(ctx, domain.GroupConfig{ChatID: -100, Enabled: true, Mode: domain.VerifyModeRequest}, created.Revision); err != nil {
		t.Fatalf("SaveGroup 失败: %v", err)
	}
	if _, err := s.SaveGroup(ctx, domain.GroupConfig{ChatID: -100}, created.Revision); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("过期 revision 应当冲突，实际 %v", err)
	}
	// 重新加载：确认写盘并可读回。
	s2, err := NewYAMLStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.GetGroup(ctx, -100)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != domain.VerifyModeRequest || got.Revision != 2 {
		t.Fatalf("重载结果异常: %+v", got)
	}
}
