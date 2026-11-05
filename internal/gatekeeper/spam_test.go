package gatekeeper

import (
	"context"
	"testing"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
)

// TestSpamDeleteThenBanOnRepeat 覆盖默认策略：第一次只删消息（可能只是有人用 @gif），
// 同一用户在窗口内第二次命中才升级为封禁。
func TestSpamDeleteThenBanOnRepeat(t *testing.T) {
	svc, api, _, _ := testService(t, func(c *config.Config) {
		c.Moderation.BotMessageAction = "delete"
		c.Moderation.BanDays = 30
		c.Moderation.RepeatBanAfter = 2
		c.Moderation.RepeatWindowSec = 600
	})
	sig := SpamSignal{
		ChatID: -100, UserID: 777, DisplayName: "广告号", MessageID: 12,
		Reason: SpamReasonViaBot, ViaBotID: 555, ViaBotUsername: "dhm_a46ff8d7bot",
		ButtonURLs: []string{"https://spam.example/x"},
	}
	if err := svc.OnSpamSignal(context.Background(), sig); err != nil {
		t.Fatalf("第一次拦截失败: %v", err)
	}
	if len(api.deleted) != 1 || api.deleted[0].message != 12 {
		t.Fatalf("第一次应删除消息 12，实际 %+v", api.deleted)
	}
	if len(api.banned) != 0 {
		t.Fatalf("第一次不应封禁，实际 %+v", api.banned)
	}

	sig.MessageID = 13
	if err := svc.OnSpamSignal(context.Background(), sig); err != nil {
		t.Fatalf("第二次拦截失败: %v", err)
	}
	if len(api.deleted) != 2 {
		t.Fatalf("第二次也应删消息，实际 %+v", api.deleted)
	}
	if len(api.banned) != 1 || api.banned[0].userID != 777 {
		t.Fatalf("第二次应升级为封禁 777，实际 %+v", api.banned)
	}
	want := time.Now().Add(30 * 24 * time.Hour).Unix()
	if d := api.banned[0].until - want; d < -120 || d > 120 {
		t.Fatalf("封禁到期时间应约为 30 天后，实际 %d（期望 %d）", api.banned[0].until, want)
	}
}

// TestSpamBanActionPermanent 覆盖 bot_message_action=ban + ban_days=0（永久封禁）。
func TestSpamBanActionPermanent(t *testing.T) {
	svc, api, _, _ := testService(t, func(c *config.Config) {
		c.Moderation.BotMessageAction = "ban"
		c.Moderation.BanDays = 0
	})
	err := svc.OnSpamSignal(context.Background(), SpamSignal{
		ChatID: -100, UserID: 778, MessageID: 21, Reason: SpamReasonViaBot, ViaBotUsername: "adbot_x",
	})
	if err != nil {
		t.Fatalf("拦截失败: %v", err)
	}
	if len(api.deleted) != 1 {
		t.Fatalf("应删除消息，实际 %+v", api.deleted)
	}
	if len(api.banned) != 1 || api.banned[0].until != 0 {
		t.Fatalf("应永久封禁（until=0），实际 %+v", api.banned)
	}
}

// TestSpamGuestBotDeleteOnly 访客机器人不是群成员：只删消息、不去封一个不存在的成员。
func TestSpamGuestBotDeleteOnly(t *testing.T) {
	svc, api, _, _ := testService(t, func(c *config.Config) {
		c.Moderation.BotMessageAction = "ban"
	})
	api.statuses = map[int64]string{781: "left"}
	err := svc.OnSpamSignal(context.Background(), SpamSignal{
		ChatID: -100, UserID: 781, MessageID: 22, Reason: SpamReasonBotSender, SenderIsBot: true,
	})
	if err != nil {
		t.Fatalf("拦截失败: %v", err)
	}
	if len(api.deleted) != 1 {
		t.Fatalf("应删除消息，实际 %+v", api.deleted)
	}
	if len(api.banned) != 0 {
		t.Fatalf("非成员机器人不该尝试封禁，实际 %+v", api.banned)
	}
}

// TestSpamMemberBotExempt 自己拉进群的工具机器人（成员身份）不算代发广告。
func TestSpamMemberBotExempt(t *testing.T) {
	svc, api, _, _ := testService(t, func(c *config.Config) {
		c.Moderation.BotMessageAction = "ban"
	})
	api.statuses = map[int64]string{782: "member"}
	err := svc.OnSpamSignal(context.Background(), SpamSignal{
		ChatID: -100, UserID: 782, MessageID: 23, Reason: SpamReasonBotSender, SenderIsBot: true,
	})
	if err != nil {
		t.Fatalf("拦截失败: %v", err)
	}
	if len(api.deleted) != 0 || len(api.banned) != 0 {
		t.Fatalf("成员机器人应被放过，实际 deleted=%+v banned=%+v", api.deleted, api.banned)
	}
}

// TestSpamAdminExempt 管理员用 @gif 之类不该被拦。
func TestSpamAdminExempt(t *testing.T) {
	svc, api, _, _ := testService(t, func(c *config.Config) {
		c.Moderation.BotMessageAction = "ban"
		c.Moderation.AdminExempt = true
	})
	api.statuses = map[int64]string{779: "creator"}
	err := svc.OnSpamSignal(context.Background(), SpamSignal{
		ChatID: -100, UserID: 779, MessageID: 31, Reason: SpamReasonViaBot, ViaBotUsername: "gif",
	})
	if err != nil {
		t.Fatalf("拦截失败: %v", err)
	}
	if len(api.deleted) != 0 || len(api.banned) != 0 {
		t.Fatalf("管理员应被豁免，实际 deleted=%+v banned=%+v", api.deleted, api.banned)
	}
}

// TestSpamDisabled 全局关闭时什么都不做。
func TestSpamDisabled(t *testing.T) {
	svc, api, _, _ := testService(t, func(c *config.Config) {
		c.Moderation.Enabled = false
	})
	err := svc.OnSpamSignal(context.Background(), SpamSignal{
		ChatID: -100, UserID: 780, MessageID: 41, Reason: SpamReasonViaBot,
	})
	if err != nil {
		t.Fatalf("拦截失败: %v", err)
	}
	if len(api.deleted) != 0 || len(api.banned) != 0 {
		t.Fatalf("关闭时应无动作，实际 deleted=%+v banned=%+v", api.deleted, api.banned)
	}
}

// TestAdWordMatchOnlyWhenEnabled 广告词扫正文默认关闭；开启后用全局词表命中。
func TestAdWordMatchOnlyWhenEnabled(t *testing.T) {
	svc, _, _, _ := testService(t, func(c *config.Config) {
		c.AdFilter.GlobalWords = []string{"代肝"}
		c.Moderation.AdTextAction = "off"
	})
	if _, hit := svc.AdWordMatch("接单代肝，价格优惠"); hit {
		t.Fatal("ad_text_action=off 时不应命中")
	}

	svc2, _, _, _ := testService(t, func(c *config.Config) {
		c.AdFilter.GlobalWords = []string{"代肝"}
		c.Moderation.AdTextAction = "delete"
	})
	word, hit := svc2.AdWordMatch("接单代肝，价格优惠")
	if !hit || word != "代肝" {
		t.Fatalf("应命中代肝，实际 word=%q hit=%v", word, hit)
	}
}
