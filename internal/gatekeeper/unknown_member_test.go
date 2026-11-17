package gatekeeper

// 懒触发复查（gatekeeper.unknown_member_policy）的回归。
//
// 场景来源：入群事件不可靠（getUpdates 缓冲溢出 / relay 丢更新 / 停机窗口），丢了就
// 绕过全部入群检查静默留在群里。补的办法是在它发言时认出来——但前提是"已知成员"
// 必须有办法被认作已知，否则 bot 上线前的老成员第一次发言就会被误判。

import (
	"context"
	"testing"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
)

// fakeBaseline 是内存版 BaselineStore。
type fakeBaseline struct {
	ids map[int64]string // user_id -> source
}

func newFakeBaseline() *fakeBaseline { return &fakeBaseline{ids: map[int64]string{}} }

func (f *fakeBaseline) UpsertBaseline(_ context.Context, _ int64, ids []int64, source string) (int, error) {
	for _, id := range ids {
		f.ids[id] = source
	}
	return len(ids), nil
}

func (f *fakeBaseline) HasBaseline(_ context.Context, _ int64, userID int64) (bool, error) {
	_, ok := f.ids[userID]
	return ok, nil
}

func (f *fakeBaseline) ListBaseline(_ context.Context, _ int64, _ int) ([]int64, error) {
	out := make([]int64, 0, len(f.ids))
	for id := range f.ids {
		out = append(out, id)
	}
	return out, nil
}

func (f *fakeBaseline) CountBaseline(_ context.Context, _ int64) (int, error) {
	return len(f.ids), nil
}

// testServiceWithBaseline 注入 fakeBaseline（testService 不传 Baseline）。
func testServiceWithBaseline(t *testing.T, mutate func(*config.Config)) (*Service, *fakeAPI, *fakeBaseline) {
	t.Helper()
	bl := newFakeBaseline()
	svc, api, _, _ := testService(t, mutate)
	svc.baseline = bl
	return svc, api, bl
}

// TestUnknownMemberPolicyOffDoesNothing 默认关：任何人不查、不记。
func TestUnknownMemberPolicyOffDoesNothing(t *testing.T) {
	svc, api, bl := testServiceWithBaseline(t, nil)
	api.bios = map[int64]string{7: "进群了解一小时8000"}

	svc.OnMemberSpoke(context.Background(), -100, 7, "陌生人")
	if len(bl.ids) != 0 {
		t.Fatalf("策略 off 时不该写基线，实际 %v", bl.ids)
	}
	if len(api.banned) != 0 {
		t.Fatalf("策略 off 时不该封人，实际 %+v", api.banned)
	}
}

// TestKnownMemberIsSkipped 已在基线里的人零成本跳过（也不该调 getChat）。
func TestKnownMemberIsSkipped(t *testing.T) {
	svc, api, bl := testServiceWithBaseline(t, func(cfg *config.Config) {
		cfg.Gatekeeper.UnknownMemberPolicy = "ban"
	})
	bl.ids[7] = "import"
	api.bios = map[int64]string{7: "进群了解一小时8000"}

	svc.OnMemberSpoke(context.Background(), -100, 7, "老成员")
	if len(api.banned) != 0 {
		t.Fatalf("基线成员不该被查/被封，实际 %+v", api.banned)
	}
}

// TestVerifiedMemberIsSkipped 有验证通过记录的人也算已知（不写基线也要跳过）。
func TestVerifiedMemberIsSkipped(t *testing.T) {
	svc, api, _, reg := testService(t, func(cfg *config.Config) {
		cfg.Gatekeeper.UnknownMemberPolicy = "ban"
		cfg.AdFilter.Enabled = true
	})
	_ = reg
	// 直接把 user 7 放进内存已知集合，模拟"启动预热已包含验证通过者"。
	svc.markKnownMember(context.Background(), -100, 7, "verified")
	api.bios = map[int64]string{7: "进群了解一小时8000"}

	svc.OnMemberSpoke(context.Background(), -100, 7, "已验证")
	if len(api.banned) != 0 {
		t.Fatalf("已验证成员不该被封，实际 %+v", api.banned)
	}
}

// TestUnknownSpamBan 未知成员 + bio 命中 → 按广告号封禁。
func TestUnknownSpamBan(t *testing.T) {
	svc, api, bl := testServiceWithBaseline(t, func(cfg *config.Config) {
		cfg.Gatekeeper.UnknownMemberPolicy = "ban"
		cfg.AdFilter.Enabled = true
		cfg.Gatekeeper.AdWordBanDays = 30
	})
	api.bios = map[int64]string{7: "https://t.me/+abc 进群了解一小时8000"}

	svc.OnMemberSpoke(context.Background(), -100, 7, "漏网的 Judy")
	if len(api.banned) != 1 {
		t.Fatalf("未知成员的 spam bio 应被封禁，实际 %+v", api.banned)
	}
	if _, ok := bl.ids[7]; ok {
		t.Fatal("处置成功后不该把它记成已知成员")
	}
	// 封禁时长按 ad_word_ban_days（30 天）。
	want := time.Now().Unix() + 30*86400
	if d := api.banned[0].until - want; d < -300 || d > 300 {
		t.Fatalf("封禁时长应取 ad_word_ban_days，实际 until=%d want≈%d", api.banned[0].until, want)
	}
}

// TestUnknownCleanBioIsRecorded 未知成员但 bio 干净 → 收进基线，下次不再查。
func TestUnknownCleanBioIsRecorded(t *testing.T) {
	svc, api, bl := testServiceWithBaseline(t, func(cfg *config.Config) {
		cfg.Gatekeeper.UnknownMemberPolicy = "ban"
		cfg.AdFilter.Enabled = true
	})
	api.bios = map[int64]string{7: "热爱方舟"}

	svc.OnMemberSpoke(context.Background(), -100, 7, "老玩家")
	if len(api.banned) != 0 {
		t.Fatalf("干净简介不该被封，实际 %+v", api.banned)
	}
	if src := bl.ids[7]; src != "observed" {
		t.Fatalf("应记为 observed，实际 %q", src)
	}
	// 第二次发言直接内存命中，不再查库/调 API。
	before := len(api.banned)
	svc.OnMemberSpoke(context.Background(), -100, 7, "老玩家")
	if len(api.banned) != before {
		t.Fatal("第二次发言不该再走一遍检查")
	}
}

var _ ports.BaselineStore = (*fakeBaseline)(nil)
