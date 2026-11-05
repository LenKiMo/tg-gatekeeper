package gatekeeper

import (
	"context"
	"strings"
	"testing"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/store"
	"github.com/LenKiMo/tg-gatekeeper/internal/telegram"
)

// joinAndPass 让 7 号用户在 -100 群入群并答对，然后把副作用跑完。
func joinAndPass(t *testing.T, svc *Service, registry *store.MemRegistry) {
	t.Helper()
	ctx := context.Background()
	if err := svc.OnMembersJoinedWithJoinMessage(ctx, -100, 1,
		[]Member{{UserID: 7, DisplayName: "新人"}}, domain.MessageRef{}); err != nil {
		t.Fatal(err)
	}
	sess, err := registry.Get(ctx, domain.SessionKey{ChatID: -100, UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.OnCallback(ctx, CallbackEvent{
		ID: "cb", ChatID: -100, UserID: 7, DisplayName: "新人",
		Data: telegram.EncodeAnswer(sess.ID, correctToken(t, sess)),
	}); err != nil {
		t.Fatal(err)
	}
	runBackground(t, svc)
}

// TestTagAppliedWithoutWelcome 验证只设了 /tag、没设欢迎语和群规的群也会打标签。
//
// 打标签以前写在 welcome() 里、排在"没有欢迎语就返回"之后，这种群永远打不上。
func TestTagAppliedWithoutWelcome(t *testing.T) {
	svc, api, registry, _ := testService(t, nil)
	group := mustGroup(t, svc, -100)
	group.Tag = "新人"
	if err := svc.SaveGroup(context.Background(), group); err != nil {
		t.Fatal(err)
	}
	joinAndPass(t, svc, registry)

	if len(api.tags) != 1 || api.tags[0].userID != 7 || api.tags[0].text != "新人" {
		t.Fatalf("通过验证后应当打标签，实际 %+v", api.tags)
	}
	if len(api.texts) != 0 {
		t.Fatalf("没有欢迎语也没有群规时不应发欢迎消息，实际 %+v", api.texts)
	}
}

// TestTagAppliedTogetherWithWelcome 验证有欢迎语时标签照打（抽出来之后行为不变）。
func TestTagAppliedTogetherWithWelcome(t *testing.T) {
	svc, api, registry, _ := testService(t, func(cfg *config.Config) {
		cfg.GroupDefaults.Welcome = "欢迎 {mention}"
	})
	group := mustGroup(t, svc, -100)
	group.Tag = "新人"
	if err := svc.SaveGroup(context.Background(), group); err != nil {
		t.Fatal(err)
	}
	joinAndPass(t, svc, registry)

	if len(api.tags) != 1 || api.tags[0].text != "新人" {
		t.Fatalf("应当打标签，实际 %+v", api.tags)
	}
	if got := welcomeOf(t, api); !strings.Contains(got, "新人") {
		t.Fatalf("应当发欢迎语，实际 %q", got)
	}
}

// TestWelcomeSentForRulesLinkOnly 验证只用 /reg <链接> 设了群规、没写欢迎语时也会发群规链接
// （与 /reg 回复消息设群规的行为一致）。
func TestWelcomeSentForRulesLinkOnly(t *testing.T) {
	svc, api, registry, _ := testService(t, nil)
	group := mustGroup(t, svc, -100)
	group.RulesLink = "https://t.me/somegroup/5"
	if err := svc.SaveGroup(context.Background(), group); err != nil {
		t.Fatal(err)
	}
	joinAndPass(t, svc, registry)

	if len(api.texts) != 1 || !strings.Contains(api.texts[0].text, "https://t.me/somegroup/5") {
		t.Fatalf("应当发出带群规链接的消息，实际 %+v", api.texts)
	}
}
