package telegram

import (
	"sync"
	"testing"

	tgbotapi "github.com/ijnkawakaze/telegram-bot-api"
)

// TestHandleReturnsSameInstance 是上线后踩到的真实 bug 的回归测试：
//
// 该版本的 AddHandle() = NewBot(api)，每次调用都会新建一个空 Bot（处理器表挂在 Bot 上，
// InitMap 会重置）。如果注册处理器用一个实例、Run() 用另一个实例，更新会被静默丢弃：
// 没有任何报错、没有日志，机器人就像没收到任何消息。因此必须缓存同一个实例。
func TestHandleReturnsSameInstance(t *testing.T) {
	// 只构造壳子：Handle() 内部不发起网络请求。
	c := &Client{api: &tgbotapi.BotAPI{}}
	first := c.Handle()
	second := c.Handle()
	if first == nil || second == nil {
		t.Fatal("Handle() 不应返回 nil")
	}
	if first != second {
		t.Fatal("Handle() 必须返回同一个 *Bot，否则注册的处理器与 Run() 用的实例不一致")
	}
}

// TestHandleIsConcurrencySafe 验证并发调用也只创建一个实例（sync.Once）。
func TestHandleIsConcurrencySafe(t *testing.T) {
	c := &Client{api: &tgbotapi.BotAPI{}}
	var wg sync.WaitGroup
	got := make([]*tgbotapi.Bot, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			got[idx] = c.Handle()
		}(i)
	}
	wg.Wait()
	for i := 1; i < len(got); i++ {
		if got[i] != got[0] {
			t.Fatalf("第 %d 个 goroutine 拿到了不同的实例", i)
		}
	}
}

// TestAddHandleCreatesFreshBot 记录这个库行为本身（将来升级库时若语义变化，这里会红）。
func TestAddHandleCreatesFreshBot(t *testing.T) {
	api := &tgbotapi.BotAPI{}
	a := api.AddHandle()
	b := api.AddHandle()
	if a == b {
		t.Fatal("AddHandle() 的语义已变（现在返回同一实例）——可以简化路由层的缓存逻辑")
	}
}
