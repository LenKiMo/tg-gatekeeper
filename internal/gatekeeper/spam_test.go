package gatekeeper

import (
	"context"
	"testing"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
)

// TestGuestBotDelete 访客机器人不是群成员：只删消息，不去封一个不存在的成员。
func TestGuestBotDelete(t *testing.T) {
	svc, api, _, _ := testService(t, func(c *config.Config) {
		c.Moderation.BotMessageAction = "delete"
	})
	api.statuses = map[int64]string{781: "left"}
	err := svc.OnSpamSignal(context.Background(), SpamSignal{
		ChatID: -100, UserID: 781, MessageID: 22, Reason: SpamReasonBotSender,
		SenderIsBot: true, ViaBotUsername: "dhm_a46ff8d7bot", CallerID: 900,
	})
	if err != nil {
		t.Fatalf("拦截失败: %v", err)
	}
	if len(api.deleted) != 1 || api.deleted[0].message != 22 {
		t.Fatalf("应删除消息 22，实际 %+v", api.deleted)
	}
	if len(api.banned) != 0 {
		t.Fatalf("delete 动作不该封人，实际 %+v", api.banned)
	}
}

// TestGuestBotBanCaller 封的是"召唤机器人的人"，不是机器人本身。
func TestGuestBotBanCaller(t *testing.T) {
	svc, api, _, _ := testService(t, func(c *config.Config) {
		c.Moderation.BotMessageAction = "ban"
		c.Moderation.BanDays = 30
	})
	api.statuses = map[int64]string{781: "left", 900: "member"}
	err := svc.OnSpamSignal(context.Background(), SpamSignal{
		ChatID: -100, UserID: 781, MessageID: 22, Reason: SpamReasonBotSender,
		SenderIsBot: true, ViaBotUsername: "dhm_a46ef8d7bot", CallerID: 900,
	})
	if err != nil {
		t.Fatalf("拦截失败: %v", err)
	}
	if len(api.banned) != 1 || api.banned[0].userID != 900 {
		t.Fatalf("应封召唤者 900，实际 %+v", api.banned)
	}
	want := time.Now().Add(30 * 24 * time.Hour).Unix()
	if d := api.banned[0].until - want; d < -120 || d > 120 {
		t.Fatalf("封禁到期时间应约为 30 天后，实际 %d", api.banned[0].until)
	}
}

// TestGuestBotWithoutCallerDeleteOnly 拿不到召唤者（老客户端/字段缺失）时只删消息。
func TestGuestBotWithoutCallerDeleteOnly(t *testing.T) {
	svc, api, _, _ := testService(t, func(c *config.Config) {
		c.Moderation.BotMessageAction = "ban"
	})
	api.statuses = map[int64]string{783: "left"}
	err := svc.OnSpamSignal(context.Background(), SpamSignal{
		ChatID: -100, UserID: 783, MessageID: 24, Reason: SpamReasonBotSender, SenderIsBot: true,
	})
	if err != nil {
		t.Fatalf("拦截失败: %v", err)
	}
	if len(api.deleted) != 1 || len(api.banned) != 0 {
		t.Fatalf("无召唤者时应只删消息，实际 deleted=%+v banned=%+v", api.deleted, api.banned)
	}
}

// TestMemberBotExempt 自己拉进群的工具机器人（成员身份）不算代发广告。
func TestMemberBotExempt(t *testing.T) {
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

// TestInlineBotOffByDefault 内联机器人（成员自己 @ 一个 bot 发消息）默认不动：
// 线上就是这条规则把两个正常用 @cvEvthBot 的成员删消息又封了 30 天。
func TestInlineBotOffByDefault(t *testing.T) {
	svc, api, _, _ := testService(t, nil) // 默认 inline_bot_action=off
	err := svc.OnSpamSignal(context.Background(), SpamSignal{
		ChatID: -100, UserID: 791, MessageID: 31, Reason: SpamReasonViaBot,
		ViaBotID: 555, ViaBotUsername: "cvevthbot",
		ButtonURLs: []string{"https://example/cv"},
	})
	if err != nil {
		t.Fatalf("拦截失败: %v", err)
	}
	if len(api.deleted) != 0 || len(api.banned) != 0 {
		t.Fatalf("默认应完全不拦内联代发，实际 deleted=%+v banned=%+v", api.deleted, api.banned)
	}
}

// TestInlineBotBanWhenConfigured 显式开启后才处置，且封的是发消息的真人。
func TestInlineBotBanWhenConfigured(t *testing.T) {
	svc, api, _, _ := testService(t, func(c *config.Config) {
		c.Moderation.InlineBotAction = "ban"
		c.Moderation.BanDays = 30
	})
	err := svc.OnSpamSignal(context.Background(), SpamSignal{
		ChatID: -100, UserID: 792, MessageID: 33, Reason: SpamReasonViaBot,
		ViaBotID: 555, ViaBotUsername: "adbot_x",
	})
	if err != nil {
		t.Fatalf("拦截失败: %v", err)
	}
	if len(api.deleted) != 1 || len(api.banned) != 1 || api.banned[0].userID != 792 {
		t.Fatalf("应删消息并封 792，实际 deleted=%+v banned=%+v", api.deleted, api.banned)
	}
}

// TestGuestBotAdminExemptByAdminFlag 管理员豁免（admin_exempt）对内联代发同样生效。
func TestGuestBotAdminExemptByAdminFlag(t *testing.T) {
	svc, api, _, _ := testService(t, func(c *config.Config) {
		c.Moderation.InlineBotAction = "ban"
		c.Moderation.AdminExempt = true
	})
	api.statuses = map[int64]string{779: "creator"}
	err := svc.OnSpamSignal(context.Background(), SpamSignal{
		ChatID: -100, UserID: 779, MessageID: 35, Reason: SpamReasonViaBot, ViaBotUsername: "gif",
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
		c.Moderation.BotMessageAction = "ban"
	})
	err := svc.OnSpamSignal(context.Background(), SpamSignal{
		ChatID: -100, UserID: 780, MessageID: 41, Reason: SpamReasonBotSender, SenderIsBot: true,
	})
	if err != nil {
		t.Fatalf("拦截失败: %v", err)
	}
	if len(api.deleted) != 0 || len(api.banned) != 0 {
		t.Fatalf("关闭时应无动作，实际 deleted=%+v banned=%+v", api.deleted, api.banned)
	}
}

// TestAdTextDeleteNeverBans 正文命中只删消息：词表是粗筛，重复命中也不封人。
func TestAdTextDeleteNeverBans(t *testing.T) {
	svc, api, _, _ := testService(t, func(c *config.Config) {
		c.AdFilter.GlobalWords = []string{"代肝"}
		c.Moderation.AdTextAction = "delete"
	})
	sig := SpamSignal{
		ChatID: -100, UserID: 790, DisplayName: "路人", MessageID: 51,
		Reason: SpamReasonAdText, MatchedWord: "代肝",
	}
	for i := 0; i < 3; i++ {
		sig.MessageID++
		if err := svc.OnSpamSignal(context.Background(), sig); err != nil {
			t.Fatalf("第 %d 次拦截失败: %v", i+1, err)
		}
	}
	if len(api.deleted) != 3 {
		t.Fatalf("三次都应删消息，实际 %+v", api.deleted)
	}
	if len(api.banned) != 0 {
		t.Fatalf("正文命中不应封人，实际 %+v", api.banned)
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
