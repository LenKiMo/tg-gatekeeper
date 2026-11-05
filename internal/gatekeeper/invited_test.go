package gatekeeper

import (
	"context"
	"testing"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
)

// TestInvitedMemberSkippedOnServiceMessagePath 验证入群服务消息这条通道带上邀请者之后，
// invited_member_policy=allow 真的会跳过验证；默认的 verify 则照常出题。
func TestInvitedMemberSkippedOnServiceMessagePath(t *testing.T) {
	invited := []Member{{UserID: 7, DisplayName: "新人", InvitedBy: 100}}
	joinRef := domain.MessageRef{ChatID: -100, MessageID: 900}

	t.Run("allow 跳过验证", func(t *testing.T) {
		svc, api, _, _ := testService(t, func(cfg *config.Config) {
			cfg.Gatekeeper.InvitedMemberPolicy = "allow"
		})
		if err := svc.OnMembersJoinedWithJoinMessage(context.Background(), -100, 1, invited, joinRef); err != nil {
			t.Fatal(err)
		}
		if len(api.restricted) != 0 || len(api.challenges) != 0 {
			t.Fatalf("被邀请的成员不应被禁言/出题，实际 restricted=%+v challenges=%+v", api.restricted, api.challenges)
		}
	})
	t.Run("verify 照常出题", func(t *testing.T) {
		svc, api, _, _ := testService(t, nil)
		if err := svc.OnMembersJoinedWithJoinMessage(context.Background(), -100, 1, invited, joinRef); err != nil {
			t.Fatal(err)
		}
		if len(api.restricted) != 1 || len(api.challenges) != 1 {
			t.Fatalf("默认策略下被邀请的成员照常验证，实际 restricted=%+v challenges=%+v", api.restricted, api.challenges)
		}
	})
}
