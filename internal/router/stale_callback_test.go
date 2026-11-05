package router

import (
	"testing"
	"time"

	tgbotapi "github.com/ijnkawakaze/telegram-bot-api"
)

// TestStaleUploadIgnoresCallbacks 回调不能按"题目消息的发送时间"判积压：
// 那样重启前发出的卡片会整体失效，管理员卡片上的放行/封禁也一样点不动。
// 点击的有效性由会话状态机（state + expires_at）保证。
func TestStaleUploadIgnoresCallbacks(t *testing.T) {
	startup := time.Now()
	old := tgbotapi.Message{MessageID: 9, Date: startup.Add(-2 * time.Hour).Unix()}

	cb := tgbotapi.Update{CallbackQuery: &tgbotapi.CallbackQuery{ID: "1", Data: "a", Message: &old}}
	if staleUpload(cb, startup) {
		t.Fatal("重启前的卡片上的按钮点击不该被当成积压丢弃")
	}

	// 普通消息仍然参与积压过滤（避免重启后把几小时前的入群/命令重放一遍）。
	msg := tgbotapi.Update{Message: &old}
	if !staleUpload(msg, startup) {
		t.Fatal("早于启动时间的普通消息应当仍被过滤")
	}
}
