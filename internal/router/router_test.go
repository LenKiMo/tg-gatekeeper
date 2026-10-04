package router

import (
	"testing"
	"time"

	tgbotapi "github.com/ijnkawakaze/telegram-bot-api"
)

// TestMemberJoined 覆盖成员状态判断：只有"在群里"的状态才算入群。
func TestMemberJoined(t *testing.T) {
	cases := map[string]struct {
		cm   tgbotapi.ChatMember
		want bool
	}{
		"member":        {tgbotapi.ChatMember{Status: "member"}, true},
		"administrator": {tgbotapi.ChatMember{Status: "administrator"}, true},
		"creator":       {tgbotapi.ChatMember{Status: "creator"}, true},
		"left":          {tgbotapi.ChatMember{Status: "left"}, false},
		"kicked":        {tgbotapi.ChatMember{Status: "kicked"}, false},
		// restricted 需要看 IsMember：被限制但仍在群里 = 入群。
		"restricted+member": {tgbotapi.ChatMember{Status: "restricted", IsMember: true}, true},
		"restricted-left":   {tgbotapi.ChatMember{Status: "restricted", IsMember: false}, false},
	}
	for name, c := range cases {
		if got := memberJoined(c.cm); got != c.want {
			t.Errorf("%s: memberJoined = %v，期望 %v", name, got, c.want)
		}
	}
}

// TestStaleUploadSkipsBacklog 验证重启后不会把几小时前的积压更新当成新事件。
func TestStaleUploadSkipsBacklog(t *testing.T) {
	startup := time.Unix(1_700_000_000, 0)
	old := tgbotapi.Update{ChatMember: &tgbotapi.ChatMemberUpdated{
		Date: startup.Add(-2 * time.Hour).Unix()}}
	fresh := tgbotapi.Update{ChatMember: &tgbotapi.ChatMemberUpdated{
		Date: startup.Add(time.Minute).Unix()}}
	if !staleUpload(old, startup) {
		t.Error("两小时前的更新应判为过期")
	}
	if staleUpload(fresh, startup) {
		t.Error("启动后的更新不应判为过期")
	}
}

// TestMatchAcceptsJoinServiceMessageWithoutSender 是生产事故的回归测试。
//
// 用户自己经邀请链接加入群组时，Telegram 的入群服务消息**不带 from**；早期版本
// match() 要求 From != nil，于是这类更新在进入业务层之前就被静默丢弃——表现为
// "有人入群但机器人完全没反应"，且没有任何日志或审计记录。
func TestMatchAcceptsJoinServiceMessageWithoutSender(t *testing.T) {
	r := &Router{}
	cases := []struct {
		name string
		u    tgbotapi.Update
		want bool
	}{
		{
			name: "自加入：入群服务消息没有 from",
			u: tgbotapi.Update{Message: &tgbotapi.Message{
				Chat:           &tgbotapi.Chat{ID: -100},
				NewChatMembers: []tgbotapi.User{{ID: 42, FirstName: "新人"}},
			}},
			want: true,
		},
		{
			name: "被管理员拉进来：有 from",
			u: tgbotapi.Update{Message: &tgbotapi.Message{
				Chat:           &tgbotapi.Chat{ID: -100},
				From:           &tgbotapi.User{ID: 7},
				NewChatMembers: []tgbotapi.User{{ID: 42}},
			}},
			want: true,
		},
		{
			name: "退群服务消息没有 from",
			u: tgbotapi.Update{Message: &tgbotapi.Message{
				Chat:           &tgbotapi.Chat{ID: -100},
				LeftChatMember: &tgbotapi.User{ID: 42},
			}},
			want: true,
		},
		{
			name: "普通群消息有 from",
			u: tgbotapi.Update{Message: &tgbotapi.Message{
				Chat: &tgbotapi.Chat{ID: -100}, From: &tgbotapi.User{ID: 7}, Text: "你好",
			}},
			want: true,
		},
		{
			name: "普通消息没有 from（渠道/匿名）→ 不处理",
			u: tgbotapi.Update{Message: &tgbotapi.Message{
				Chat: &tgbotapi.Chat{ID: -100}, Text: "广告",
			}},
			want: false,
		},
		{
			name: "chat_member 状态跃迁（自加入的主信号）",
			u: tgbotapi.Update{ChatMember: &tgbotapi.ChatMemberUpdated{
				Chat:          tgbotapi.Chat{ID: -100},
				OldChatMember: tgbotapi.ChatMember{Status: "left"},
				NewChatMember: tgbotapi.ChatMember{Status: "member",
					User: &tgbotapi.User{ID: 42}},
			}},
			want: true,
		},
		{
			name: "callback 按钮",
			u:    tgbotapi.Update{CallbackQuery: &tgbotapi.CallbackQuery{ID: "x", Data: "g1.a.b"}},
			want: true,
		},
		{
			name: "入群申请（审批模式）",
			u:    tgbotapi.Update{ChatJoinRequest: &tgbotapi.ChatJoinRequest{}},
			want: true,
		},
		{
			name: "空更新",
			u:    tgbotapi.Update{},
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := r.match(c.u); got != c.want {
				t.Fatalf("match() = %v，期望 %v", got, c.want)
			}
		})
	}
}

// TestSplitCommandParsing 覆盖私聊 /start 的解析（payload 是会话 ID）。
func TestSplitCommandParsing(t *testing.T) {
	cases := []struct {
		in, cmd, payload string
	}{
		{"/start", "/start", ""},
		{"/start abc123", "/start", "abc123"},
		{"/start@GatekeeperBot abc123", "/start", "abc123"},
		{"  /start   abc123  ", "/start", "abc123"},
	}
	for _, c := range cases {
		cmd, payload := splitCommand(c.in)
		if cmd != c.cmd || payload != c.payload {
			t.Fatalf("splitCommand(%q) = (%q,%q)，期望 (%q,%q)", c.in, cmd, payload, c.cmd, c.payload)
		}
	}
}
