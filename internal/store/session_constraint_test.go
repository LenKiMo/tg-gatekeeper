package store

// 会话写入的约束回归。
//
// 生产事故（2026-11-17 17:48）：账本会话还没抽题，CorrectTokenHash 是 nil，而
// sessions.correct_hash 列是 NOT NULL → INSERT 报 `constraint failed: NOT NULL
// constraint failed: sessions.correct_hash`，整条入群流程中断：不出题、不处置，
// 人静默留在群里。
//
// 测试套件里一直用内存注册表（MemRegistry），它没有列约束，所以这个 bug 只在
// SQLite 上才暴露——这就是为什么这里必须用真实 SQLite 覆盖一次。

import (
	"context"
	"testing"

	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
)

// TestBeginAcceptsEmptyCorrectHash 账本形态（没有答案摘要）必须能写进 SQLite。
func TestBeginAcceptsEmptyCorrectHash(t *testing.T) {
	h := testSQL(t)
	reg := NewRegistry(h)
	ctx := context.Background()

	sess := domain.Session{
		ID:               "ledger-1",
		Key:              domain.SessionKey{ChatID: -100, UserID: 7},
		UpdateID:         0,
		Mode:             domain.SessionJoin,
		State:            domain.StatePreparing,
		DisplayName:      "Judy Bach",
		CorrectTokenHash: nil, // 账本会话就是这样：还没抽题
		Version:          1,
	}
	if _, err := reg.Begin(ctx, sess); err != nil {
		t.Fatalf("账本会话写入失败（correct_hash NOT NULL）: %v", err)
	}
	got, err := reg.Get(ctx, sess.Key)
	if err != nil {
		t.Fatalf("读回会话失败: %v", err)
	}
	if got.ID != sess.ID {
		t.Fatalf("读回的不是刚写的会话: %+v", got)
	}
}

// TestBeginAcceptsEmptyCorrectHashSlice 显式空切片（调用方自己归一）同样可用。
func TestBeginAcceptsEmptyCorrectHashSlice(t *testing.T) {
	h := testSQL(t)
	reg := NewRegistry(h)
	ctx := context.Background()

	sess := domain.Session{
		ID:               "ledger-2",
		Key:              domain.SessionKey{ChatID: -100, UserID: 8},
		Mode:             domain.SessionJoin,
		State:            domain.StatePreparing,
		CorrectTokenHash: []byte{},
		Version:          1,
	}
	if _, err := reg.Begin(ctx, sess); err != nil {
		t.Fatalf("空切片写入失败: %v", err)
	}
}
