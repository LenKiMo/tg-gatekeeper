package gatekeeper

import (
	"context"
	"errors"
	"testing"
	"time"

	tgbotapi "github.com/ijnkawakaze/telegram-bot-api"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
)

// TestRequestModeAdWordDeclinesByDefault 申请模式命中广告词默认只拒绝申请、不封禁：
// 申请人还没进群，封一个不在群里的人只会往黑名单里堆东西。
func TestRequestModeAdWordDeclinesByDefault(t *testing.T) {
	svc, api, _, _ := testService(t, func(cfg *config.Config) {
		cfg.GroupDefaults.Mode = string(domain.VerifyModeRequest)
		cfg.GroupDefaults.AdWords = []string{"广告"}
		cfg.Gatekeeper.AdWordRequestBanDays = 0
	})
	ctx := context.Background()

	if err := svc.OnJoinRequest(ctx, JoinRequest{ChatID: -100, UserID: 5, DisplayName: "卖广告的"}); err != nil {
		t.Fatalf("处理入群申请失败: %v", err)
	}
	if len(api.declined) != 1 {
		t.Fatalf("应当拒绝申请 1 次，实际 %d", len(api.declined))
	}
	if len(api.banned) != 0 {
		t.Fatalf("默认不该封禁，实际 %+v", api.banned)
	}
}

// TestRequestModeAdWordBansWhenConfigured 配了 ad_word_request_ban_days 才封禁，且是定时封禁。
func TestRequestModeAdWordBansWhenConfigured(t *testing.T) {
	svc, api, _, _ := testService(t, func(cfg *config.Config) {
		cfg.GroupDefaults.Mode = string(domain.VerifyModeRequest)
		cfg.GroupDefaults.AdWords = []string{"广告"}
		cfg.Gatekeeper.AdWordRequestBanDays = 3
	})
	ctx := context.Background()

	before := time.Now().Unix()
	if err := svc.OnJoinRequest(ctx, JoinRequest{ChatID: -100, UserID: 5, DisplayName: "卖广告的"}); err != nil {
		t.Fatalf("处理入群申请失败: %v", err)
	}
	if len(api.banned) != 1 {
		t.Fatalf("应当封禁 1 次，实际 %d", len(api.banned))
	}
	got := api.banned[0].until
	want := before + 3*86400
	if got < want-5 || got > want+60 {
		t.Fatalf("封禁到期时间应按 3 天计算，实际 until=%d（期望≈%d）", got, want)
	}
	if len(api.declined) != 1 {
		t.Fatalf("封禁之后仍要拒绝申请，实际 decline=%d", len(api.declined))
	}
}

// TestDuplicateJoinSignalDeletesJoinMessage chat_member 先到时，随后到达的入群服务消息
// 会被当成重复信号丢弃——但那句"X 加入了群组"仍然要删掉，否则会永远留在群里。
func TestDuplicateJoinSignalDeletesJoinMessage(t *testing.T) {
	svc, api, _, _ := testService(t, func(cfg *config.Config) {
		cfg.Gatekeeper.DeleteJoinMessage = true
	})
	ctx := context.Background()
	members := []Member{{UserID: 7, DisplayName: "新人"}}

	// 第一条通道：chat_member 状态跃迁（没有入群消息可挂）。
	if err := svc.OnMembersJoined(ctx, -100, 1, members); err != nil {
		t.Fatalf("入群失败: %v", err)
	}
	// 第二条通道：入群服务消息，带 joinRef。
	joinRef := domain.MessageRef{ChatID: -100, MessageID: 4242}
	if err := svc.OnMembersJoinedWithJoinMessage(ctx, -100, 2, members, joinRef); err != nil {
		t.Fatalf("重复入群信号处理失败: %v", err)
	}

	runBackground(t, svc)

	found := false
	for _, d := range api.deleted {
		if d.message == 4242 {
			found = true
		}
	}
	if !found {
		t.Fatalf("重复入群信号带来的服务消息应当被删除，实际 deleted=%+v", api.deleted)
	}
	if len(api.challenges) != 1 {
		t.Fatalf("只应出一道题，实际 %d", len(api.challenges))
	}
}

// TestSaveGroupConflictsOnStaleRevision 两个管理员基于同一份配置各改一个字段时，
// 后写的那个必须拿到冲突错误，而不是把前一个人的改动覆盖掉。
func TestSaveGroupConflictsOnStaleRevision(t *testing.T) {
	svc, _, _, _ := testService(t, nil)
	ctx := context.Background()

	first, err := svc.Group(ctx, -100)
	if err != nil {
		t.Fatalf("读取群配置失败: %v", err)
	}
	second := first // 另一个人读到的还是同一版本

	first.Welcome = "A 改的"
	if err := svc.SaveGroup(ctx, first); err != nil {
		t.Fatalf("第一次保存应当成功: %v", err)
	}

	second.Tag = "B 改的"
	err = svc.SaveGroup(ctx, second)
	if !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("基于旧 revision 的保存应当返回冲突，实际 %v", err)
	}

	cur, err := svc.Group(ctx, -100)
	if err != nil {
		t.Fatalf("读取群配置失败: %v", err)
	}
	if cur.Welcome != "A 改的" {
		t.Fatalf("第一次的改动被覆盖了：%q", cur.Welcome)
	}
}

// TestPermanentEffectErrorUsesTypedCode 永久错误按结构化错误码判断，不做文本子串匹配。
func TestPermanentEffectErrorUsesTypedCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"chat not found", &tgbotapi.Error{Code: 400, Message: "Bad Request: chat not found"}, true},
		{"not enough rights", &tgbotapi.Error{Code: 400, Message: "Bad Request: not enough rights to send text messages"}, true},
		{"bot blocked", &tgbotapi.Error{Code: 403, Message: "Forbidden: bot was blocked by the user"}, true},
		// 真正的 400 但不属于永久错误：应当重试，而不是直接判死。
		{"empty text", &tgbotapi.Error{Code: 400, Message: "Bad Request: message text is empty"}, false},
		// 网络错误里恰好带 400：以前会被当成永久错误。
		{"network with 400 in text", errors.New("Post https://api.telegram.org/bot400:xx/sendMessage: dial tcp: i/o timeout"), false},
		{"plain network", errors.New("dial tcp 1.2.3.4:443: connect: connection refused"), false},
	}
	for _, c := range cases {
		if got := isPermanentEffectError(c.err); got != c.want {
			t.Errorf("%s: 期望 %v，实际 %v", c.name, c.want, got)
		}
	}
}

var _ = time.Now
