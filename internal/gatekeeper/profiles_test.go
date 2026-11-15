package gatekeeper

// 入群资料链路的回归：bot 农场规则三态、简介专用词、脱敏快照。
//
// 断言口径刻意做成"成对"的：每条"该拦"必须配一条"不该拦"（有 bio / 有 username /
// 有头像 / 中文名），否则把整条规则改成恒真也能过测试。

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
)

// fakeProfileStore 记录写入的快照（只用于测试）。
type fakeProfileStore struct {
	saved  []ports.ProfileSnapshot
	purged int64
}

func (f *fakeProfileStore) SaveProfile(_ context.Context, p ports.ProfileSnapshot) error {
	f.saved = append(f.saved, p)
	return nil
}

func (f *fakeProfileStore) PurgeProfilesBefore(_ context.Context, _ int64) (int64, error) {
	return f.purged, nil
}

// testProfiles 非 nil 时由 testService 注入（见 service_test.go）。
var testProfiles *fakeProfileStore

func withProfileStore(t *testing.T) *fakeProfileStore {
	t.Helper()
	testProfiles = &fakeProfileStore{}
	t.Cleanup(func() { testProfiles = nil })
	return testProfiles
}

// TestEnglishNamePolicyOffKeepsChallenge policy=off 时一切照旧：出题，不封禁。
func TestEnglishNamePolicyOffKeepsChallenge(t *testing.T) {
	svc, api, _, _ := testService(t, nil)
	if err := svc.OnMembersJoined(context.Background(), -100, 1,
		[]Member{{UserID: 7, DisplayName: "Malia Schwartz"}}); err != nil {
		t.Fatal(err)
	}
	if len(api.challenges) != 1 || len(api.banned) != 0 {
		t.Fatalf("policy=off 应照常出题且不封禁，实际 challenges=%d banned=%d",
			len(api.challenges), len(api.banned))
	}
}

// TestEnglishNamePolicyKickOnlyWhenProfileEmpty 核心判据：三处资料全空才踢。
func TestEnglishNamePolicyKickOnlyWhenProfileEmpty(t *testing.T) {
	cases := []struct {
		name     string
		display  string
		profile  ports.UserProfile
		wantKick bool
	}{
		{"空资料英文名→踢", "Malia Schwartz", ports.UserProfile{}, true},
		{"有简介→不踢", "Jack Smith", ports.UserProfile{Bio: "热爱方舟"}, false},
		{"有 username→不踢", "Ella Rowe", ports.UserProfile{Username: "ella_r"}, false},
		{"有头像→不踢", "Hope Chalie", ports.UserProfile{HasPhoto: true}, false},
		{"中文名→不踢", "秦琴音", ports.UserProfile{}, false},
		{"单词英文名→不踢", "Malia", ports.UserProfile{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, api, _, _ := testService(t, func(cfg *config.Config) {
				cfg.Gatekeeper.EnglishNamePolicy = "kick"
				cfg.Gatekeeper.KickUnbanAfterSeconds = 60
			})
			api.profiles = map[int64]ports.UserProfile{7: c.profile}
			if err := svc.OnMembersJoined(context.Background(), -100, 1,
				[]Member{{UserID: 7, DisplayName: c.display}}); err != nil {
				t.Fatal(err)
			}
			kicked := len(api.banned) == 1
			if kicked != c.wantKick {
				t.Fatalf("%q：期望 kick=%v，实际 banned=%d", c.display, c.wantKick, len(api.banned))
			}
			if c.wantKick && len(api.challenges) != 0 {
				t.Fatalf("%q：命中后不该出题，实际 %d 道", c.display, len(api.challenges))
			}
			if !c.wantKick && len(api.challenges) != 1 {
				t.Fatalf("%q：应照常出题，实际 %d 道", c.display, len(api.challenges))
			}
		})
	}
}

// TestEnglishNamePolicyKickIsScheduledBan 命中时：不出题，封禁带解禁时间。
func TestEnglishNamePolicyKickIsScheduledBan(t *testing.T) {
	svc, api, _, _ := testService(t, func(cfg *config.Config) {
		cfg.Gatekeeper.EnglishNamePolicy = "kick"
		cfg.Gatekeeper.KickUnbanAfterSeconds = 60
	})
	api.profiles = map[int64]ports.UserProfile{7: {}}
	before := time.Now().Unix()
	if err := svc.OnMembersJoined(context.Background(), -100, 1,
		[]Member{{UserID: 7, DisplayName: "Malia Schwartz"}}); err != nil {
		t.Fatal(err)
	}
	if len(api.challenges) != 0 {
		t.Fatalf("命中 bot 农场特征链后不该出题，实际发出 %d 道", len(api.challenges))
	}
	if len(api.banned) != 1 {
		t.Fatalf("应当执行一次封禁，实际 %d", len(api.banned))
	}
	until := api.banned[0].until
	if until <= before {
		t.Fatalf("封禁必须带未来解禁时间（临时踢出），实际 until=%d now=%d", until, before)
	}
	if until > before+300 {
		t.Fatalf("解禁时间远超 kick_unban_after_seconds，实际 until=%d", until)
	}
}

// TestEnglishNamePolicyAuditDoesNotKick audit 模式下只记录，照常出题。
func TestEnglishNamePolicyAuditDoesNotKick(t *testing.T) {
	svc, api, _, _ := testService(t, func(cfg *config.Config) {
		cfg.Gatekeeper.EnglishNamePolicy = "audit"
	})
	api.profiles = map[int64]ports.UserProfile{7: {}}
	if err := svc.OnMembersJoined(context.Background(), -100, 1,
		[]Member{{UserID: 7, DisplayName: "Jsheg Syej"}}); err != nil {
		t.Fatal(err)
	}
	if len(api.challenges) != 1 || len(api.banned) != 0 {
		t.Fatalf("audit 模式应照常出题且不封禁，实际 challenges=%d banned=%d",
			len(api.challenges), len(api.banned))
	}
}

// TestProfileLookupErrorDoesNotKick 读不到资料时不得触发规则（信息不足就当正常）。
func TestProfileLookupErrorDoesNotKick(t *testing.T) {
	svc, api, _, _ := testService(t, func(cfg *config.Config) {
		cfg.Gatekeeper.EnglishNamePolicy = "kick"
	})
	api.profileErr = context.DeadlineExceeded
	if err := svc.OnMembersJoined(context.Background(), -100, 1,
		[]Member{{UserID: 7, DisplayName: "Malia Schwartz"}}); err != nil {
		t.Fatal(err)
	}
	if len(api.challenges) != 1 {
		t.Fatalf("资料读取失败时应放行去答题，实际 challenges=%d", len(api.challenges))
	}
}

// TestBioWordsRejectBeforeChallenge 简介命中 bio_words：不出题、直接封禁。
func TestBioWordsRejectBeforeChallenge(t *testing.T) {
	svc, api, _, _ := testService(t, func(cfg *config.Config) {
		cfg.AdFilter.Enabled = true
		cfg.Gatekeeper.AdWordBanDays = 30
	})
	api.bios = map[int64]string{7: "虚拟货币搬砖日挣1千U 招团队合作"}
	if err := svc.OnMembersJoined(context.Background(), -100, 1,
		[]Member{{UserID: 7, DisplayName: "老王"}}); err != nil {
		t.Fatal(err)
	}
	if len(api.challenges) != 0 {
		t.Fatalf("简介命中广告词后不该出题，实际 %d", len(api.challenges))
	}
	if len(api.banned) != 1 {
		t.Fatalf("简介命中应封禁，实际 %d", len(api.banned))
	}
}

// TestHarmonicNicknameRejected 谐音昵称走真实服务层：确认真会封人（不是只命中 filter）。
func TestHarmonicNicknameRejected(t *testing.T) {
	svc, api, _, _ := testService(t, func(cfg *config.Config) {
		cfg.AdFilter.Enabled = true
		cfg.AdFilter.GlobalWords = []string{"看我主页"}
		cfg.Gatekeeper.AdWordBanDays = 30
	})
	if err := svc.OnMembersJoined(context.Background(), -100, 1,
		[]Member{{UserID: 7, DisplayName: "看我主椰"}}); err != nil {
		t.Fatal(err)
	}
	if len(api.banned) != 1 || len(api.challenges) != 0 {
		t.Fatalf("谐音昵称应被封禁且不出题，实际 banned=%d challenges=%d",
			len(api.banned), len(api.challenges))
	}

	// 对照组：同一套配置下，正常昵称必须照常出题。
	api2svc, api2, _, _ := testService(t, func(cfg *config.Config) {
		cfg.AdFilter.Enabled = true
		cfg.AdFilter.GlobalWords = []string{"看我主页"}
	})
	if err := api2svc.OnMembersJoined(context.Background(), -100, 1,
		[]Member{{UserID: 8, DisplayName: "秦琴音"}}); err != nil {
		t.Fatal(err)
	}
	if len(api2.banned) != 0 || len(api2.challenges) != 1 {
		t.Fatalf("正常昵称不该被封，实际 banned=%d challenges=%d",
			len(api2.banned), len(api2.challenges))
	}
}

// TestProfileSnapshotIsDeidentified 采集到的快照必须脱敏：没有 user_id 可读形态。
func TestProfileSnapshotIsDeidentified(t *testing.T) {
	ps := withProfileStore(t)
	svc, api, _, _ := testService(t, func(cfg *config.Config) {
		cfg.Privacy.CollectProfiles = true
		cfg.Privacy.RetentionDays = 90
		cfg.Privacy.HMACKey = "test-key"
	})
	api.profiles = map[int64]ports.UserProfile{
		7: {Bio: "热爱方舟", Username: "ella_r", HasPhoto: true},
	}
	now := time.Now().Unix()
	if err := svc.OnMembersJoined(context.Background(), -100, 1,
		[]Member{{UserID: 7, DisplayName: "Ella Rowe"}}); err != nil {
		t.Fatal(err)
	}
	if len(ps.saved) != 1 {
		t.Fatalf("应写入 1 条快照，实际 %d", len(ps.saved))
	}
	s := ps.saved[0]
	if len(s.Ref) != 32 {
		t.Fatalf("ref 应是 32 位十六进制摘要，实际 %q", s.Ref)
	}
	if _, err := hex.DecodeString(s.Ref); err != nil {
		t.Fatalf("ref 不是合法十六进制：%q", s.Ref)
	}
	if s.NameShape != "ascii_name" {
		t.Errorf("name_shape 应为 ascii_name，实际 %q", s.NameShape)
	}
	if s.Decision != profileDecisionVerify {
		t.Errorf("decision 应为 %s，实际 %q", profileDecisionVerify, s.Decision)
	}
	if !s.HasUsername || !s.HasPhoto {
		t.Errorf("应记录 username/头像的布尔位，实际 %+v", s)
	}
	if s.ExpiresAt <= now {
		t.Errorf("保留期应给出未来的 expires_at，实际 %d", s.ExpiresAt)
	}
	// 同一用户稳定、不同用户不同。
	if profileRef([]byte("test-key"), 7) != s.Ref {
		t.Error("同一 user_id 的 ref 必须稳定（否则无法做重复出现统计）")
	}
	if profileRef([]byte("test-key"), 8) == s.Ref {
		t.Error("不同 user_id 的 ref 不应相同")
	}
	if profileRef([]byte("other-key"), 7) == s.Ref {
		t.Error("换密钥后 ref 应变化（证明它真的是 HMAC 而不是裸哈希）")
	}
}

// TestProfileNotCollectedWhenDisabled 默认不采集：不写库、也不因为缺 profile 而报错。
func TestProfileNotCollectedWhenDisabled(t *testing.T) {
	ps := withProfileStore(t)
	svc, _, _, _ := testService(t, func(cfg *config.Config) {
		cfg.Privacy.CollectProfiles = false
	})
	if err := svc.OnMembersJoined(context.Background(), -100, 1,
		[]Member{{UserID: 7, DisplayName: "新人"}}); err != nil {
		t.Fatal(err)
	}
	if len(ps.saved) != 0 {
		t.Fatalf("未开启采集时不该写快照，实际 %d 条", len(ps.saved))
	}
}

// TestAuditBotProfileWritesExactlyOneSnapshot audit 模式下命中 bot 农场特征链后仍然出题，
// 但同一次入群只能写一条快照——否则 "命中后被出题" 的号会同时有 english_name_bot 与
// verify 两条记录，事后统计"命中者里有多少最终答题通过"就被污染了。
func TestAuditBotProfileWritesExactlyOneSnapshot(t *testing.T) {
	ps := withProfileStore(t)
	svc, api, _, _ := testService(t, func(cfg *config.Config) {
		cfg.Gatekeeper.EnglishNamePolicy = "audit"
		cfg.Privacy.CollectProfiles = true
		cfg.Privacy.HMACKey = "test-key"
	})
	api.profiles = map[int64]ports.UserProfile{7: {}}
	if err := svc.OnMembersJoined(context.Background(), -100, 1,
		[]Member{{UserID: 7, DisplayName: "Eileen Carn"}}); err != nil {
		t.Fatal(err)
	}
	if len(api.challenges) != 1 {
		t.Fatalf("audit 模式应照常出题，实际 %d 道", len(api.challenges))
	}
	if len(ps.saved) != 1 {
		t.Fatalf("同一次入群应只写 1 条快照，实际 %d 条: %+v", len(ps.saved), ps.saved)
	}
	if d := ps.saved[0].Decision; d != profileDecisionEnglishName {
		t.Fatalf("decision 应为 %s，实际 %s", profileDecisionEnglishName, d)
	}
}

// TestNameShape 形态分类。
func TestNameShape(t *testing.T) {
	cases := map[string]string{
		"":               "empty",
		"Malia Schwartz": "ascii_name",
		"秦琴音":            "cjk",
		"快😘乐营":           "cjk_emoji",
		"🐂":              "emoji_only",
		"Ryltqngpuu 云":   "cjk",
	}
	for in, want := range cases {
		if got := nameShape(in); got != want {
			t.Errorf("nameShape(%q) = %q，期望 %q", in, got, want)
		}
	}
}
