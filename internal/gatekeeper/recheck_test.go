package gatekeeper

// 放行前复查简介（ad_filter.recheck_on_pass）的回归。
//
// 覆盖两个入口：用户自己答对，以及管理员点"✅ 放行"。这两个入口都可能把一个 bio
// 写着广告的人放进群——前者是因为入群时 getChat 可能失败，后者是因为管理员看不到
// 简介。命中后必须**不放行**，并按广告号封（ad_word_ban_days），不是只封 7 天。

import (
	"context"
	"testing"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/store"
	"github.com/LenKiMo/tg-gatekeeper/internal/telegram"
)

// joinThenSetBio 先以"干净简介"入群拿到会话，再改 bio。
//
// 这个顺序是刻意的：入群那一刻 bio 是干净的（所以能出题），答题窗口内才被改成广告——
// 正是这个功能要拦的场景。若一上来就带 spam bio，入群时的检查会先拦下，测不到
// "放行前复查"。
func joinThenSetBio(t *testing.T, svc *Service, api *fakeAPI, reg *store.MemRegistry,
	userID int64, name, bio string) domain.Session {
	t.Helper()
	if err := svc.OnMembersJoined(context.Background(), -100, 1,
		[]Member{{UserID: userID, DisplayName: name}}); err != nil {
		t.Fatalf("入群失败: %v", err)
	}
	sess, err := reg.Get(context.Background(), domain.SessionKey{ChatID: -100, UserID: userID})
	if err != nil {
		t.Fatalf("取会话失败: %v（challenges=%d banned=%d）", err, len(api.challenges), len(api.banned))
	}
	api.bios = map[int64]string{userID: bio}
	return sess
}

func stateOf(t *testing.T, reg *store.MemRegistry, sessionID string) domain.SessionState {
	t.Helper()
	// 终态会话会被从活跃表里摘掉（Claim 里 delete(m.sessions, key)），只能用 GetByID
	// 从 last 表读——这就是为什么这里不按 (chat_id,user_id) 查。
	s, err := reg.GetByID(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("取会话失败: %v", err)
	}
	return s.State
}

// TestRecheckOnCorrectRejectsSpamBio 答对但简介命中广告词：不放行，按广告号封 30 天。
func TestRecheckOnCorrectRejectsSpamBio(t *testing.T) {
	svc, api, reg, _ := testService(t, func(cfg *config.Config) {
		cfg.AdFilter.Enabled = true
		cfg.AdFilter.RecheckOnPass = true
		cfg.Gatekeeper.AdWordBanDays = 30
	})
	sess := joinThenSetBio(t, svc, api, reg, 7, "Kumar Ray", "进群了解一小时8000 https://t.me/+abc")

	before := time.Now().Unix()
	if err := svc.OnCallback(context.Background(), CallbackEvent{
		ID: "cb", ChatID: -100, UserID: 7, DisplayName: "Kumar Ray",
		Data: telegram.EncodeAnswer(sess.ID, correctToken(t, sess)),
	}); err != nil {
		t.Fatal(err)
	}

	if got := stateOf(t, reg, sess.ID); got != domain.StateAdminBan {
		t.Fatalf("应进入广告封禁终态，实际 %s", got)
	}
	runBackground(t, svc) // 封禁是 outbox 副作用，要推进一轮才落到 api
	if len(api.banned) != 1 {
		t.Fatalf("应封禁一次，实际 %d", len(api.banned))
	}
	want := before + 30*86400
	if d := api.banned[0].until - want; d < -120 || d > 120 {
		t.Fatalf("封禁时长应取 ad_word_ban_days（30 天），实际 until=%d want≈%d",
			api.banned[0].until, want)
	}
	for _, r := range api.restored {
		t.Fatalf("命中后不应恢复权限，实际 %+v", r)
	}
}

// TestRecheckOnCorrectPassesCleanBio 简介干净时照旧放行。
func TestRecheckOnCorrectPassesCleanBio(t *testing.T) {
	svc, api, reg, _ := testService(t, func(cfg *config.Config) {
		cfg.AdFilter.Enabled = true
		cfg.AdFilter.RecheckOnPass = true
	})
	sess := joinThenSetBio(t, svc, api, reg, 7, "秦琴音", "热爱方舟，日常摸鱼")

	if err := svc.OnCallback(context.Background(), CallbackEvent{
		ID: "cb", ChatID: -100, UserID: 7, DisplayName: "秦琴音",
		Data: telegram.EncodeAnswer(sess.ID, correctToken(t, sess)),
	}); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, reg, sess.ID); got != domain.StatePassed {
		t.Fatalf("简介干净应放行，实际 %s", got)
	}
	if len(api.banned) != 0 {
		t.Fatalf("不该封禁，实际 %+v", api.banned)
	}
}

// TestRecheckOnAdminPassRejectsSpamBio 管理员点"放行"但简介命中：同样按广告号处置。
func TestRecheckOnAdminPassRejectsSpamBio(t *testing.T) {
	svc, api, reg, _ := testService(t, func(cfg *config.Config) {
		cfg.AdFilter.Enabled = true
		cfg.AdFilter.RecheckOnPass = true
		cfg.Gatekeeper.AdWordBanDays = 30
	})
	api.admins[99] = true
	sess := joinThenSetBio(t, svc, api, reg, 7, "老王", "手头紧，内部项目，一天5万随便拿")

	if err := svc.OnCallback(context.Background(), CallbackEvent{
		ID: "cb", ChatID: -100, UserID: 99, DisplayName: "admin",
		Data: telegram.EncodeAdmin(sess.ID, true),
	}); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, reg, sess.ID); got != domain.StateAdminBan {
		t.Fatalf("管理员放行时 bio 命中应改判广告封禁，实际 %s", got)
	}
	runBackground(t, svc)
	if len(api.banned) != 1 {
		t.Fatalf("应封禁一次，实际 %d", len(api.banned))
	}
}

// TestRecheckDisabledByConfig 关掉开关后不再复查（省一次 API 调用）。
func TestRecheckDisabledByConfig(t *testing.T) {
	svc, api, reg, _ := testService(t, func(cfg *config.Config) {
		cfg.AdFilter.Enabled = true
		cfg.AdFilter.RecheckOnPass = false
	})
	sess := joinThenSetBio(t, svc, api, reg, 7, "Kumar Ray", "进群了解一小时8000")

	if err := svc.OnCallback(context.Background(), CallbackEvent{
		ID: "cb", ChatID: -100, UserID: 7, DisplayName: "Kumar Ray",
		Data: telegram.EncodeAnswer(sess.ID, correctToken(t, sess)),
	}); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, reg, sess.ID); got != domain.StatePassed {
		t.Fatalf("关闭复查后应正常放行，实际 %s", got)
	}
}

// TestRecheckLookupErrorStillPasses 读不到资料时放行（与入群时的 audit_continue 同立场）。
func TestRecheckLookupErrorStillPasses(t *testing.T) {
	svc, api, reg, _ := testService(t, func(cfg *config.Config) {
		cfg.AdFilter.Enabled = true
		cfg.AdFilter.RecheckOnPass = true
	})
	sess := joinThenSetBio(t, svc, api, reg, 7, "Malia Warn", "")
	api.profileErr = context.DeadlineExceeded

	if err := svc.OnCallback(context.Background(), CallbackEvent{
		ID: "cb", ChatID: -100, UserID: 7, DisplayName: "Malia Warn",
		Data: telegram.EncodeAnswer(sess.ID, correctToken(t, sess)),
	}); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, reg, sess.ID); got != domain.StatePassed {
		t.Fatalf("资料读取失败时应放行，实际 %s", got)
	}
}
