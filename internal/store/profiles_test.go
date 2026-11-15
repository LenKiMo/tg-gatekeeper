package store

import (
	"context"
	"testing"

	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
)

// TestSaveProfileRoundTrip 验证脱敏快照能写能读，且表结构里真的没有 user_id 列。
//
// 后半条是关键：脱敏是"表里没有这一列"，不是"写了但没人看"。用 PRAGMA 断言，
// 防止将来有人图省事加一列原始 user_id。
func TestSaveProfileRoundTrip(t *testing.T) {
	h := testSQL(t)
	ctx := context.Background()

	cols := map[string]bool{}
	rows, err := h.DB().QueryContext(ctx, `PRAGMA table_info(profile_snapshots)`)
	if err != nil {
		t.Fatalf("读表结构失败: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    any
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("扫描表结构失败: %v", err)
		}
		cols[name] = true
	}
	for _, banned := range []string{"user_id", "username"} {
		if cols[banned] {
			t.Fatalf("脱敏表里不该出现 %q 列（会直接暴露身份）", banned)
		}
	}
	for _, want := range []string{"ref", "display_name", "bio", "decision"} {
		if !cols[want] {
			t.Fatalf("脱敏表缺少必备列 %q", want)
		}
	}

	rec := ports.ProfileSnapshot{
		Ref: "0123456789abcdef0123456789abcdef", ChatID: -100,
		DisplayName: "Ella Rowe", Bio: "热爱方舟",
		HasUsername: true, HasPhoto: false,
		NameShape: "ascii_name", Decision: "verify", Rule: "", Word: "",
		CreatedAt: 1000, ExpiresAt: 2000,
	}
	if err := h.SaveProfile(ctx, rec); err != nil {
		t.Fatalf("写入快照失败: %v", err)
	}

	var (
		ref, name, bio, shape, decision string
		hasUser, hasPhoto               int
		created, expires                int64
	)
	err = h.DB().QueryRowContext(ctx,
		`SELECT ref, display_name, bio, name_shape, decision, has_username, has_photo,
		        created_at, expires_at FROM profile_snapshots LIMIT 1`).
		Scan(&ref, &name, &bio, &shape, &decision, &hasUser, &hasPhoto, &created, &expires)
	if err != nil {
		t.Fatalf("读回快照失败: %v", err)
	}
	if ref != rec.Ref || name != rec.DisplayName || bio != rec.Bio || shape != rec.NameShape ||
		decision != rec.Decision || hasUser != 1 || hasPhoto != 0 ||
		created != rec.CreatedAt || expires != rec.ExpiresAt {
		t.Fatalf("读回内容与写入不一致: got ref=%q name=%q bio=%q shape=%q decision=%q "+
			"user=%d photo=%d created=%d expires=%d",
			ref, name, bio, shape, decision, hasUser, hasPhoto, created, expires)
	}
}

// TestPurgeProfilesBefore 只删早于 cutoff 的记录，且返回准确条数。
func TestPurgeProfilesBefore(t *testing.T) {
	h := testSQL(t)
	ctx := context.Background()
	for i, ts := range []int64{100, 200, 300} {
		if err := h.SaveProfile(ctx, ports.ProfileSnapshot{
			Ref:       "ref" + string(rune('a'+i)),
			CreatedAt: ts,
		}); err != nil {
			t.Fatalf("写入快照失败: %v", err)
		}
	}
	n, err := h.PurgeProfilesBefore(ctx, 250)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("应删除 2 条（100/200），实际 %d", n)
	}
	var left int
	if err := h.DB().QueryRowContext(ctx, `SELECT count(*) FROM profile_snapshots`).Scan(&left); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if left != 1 {
		t.Fatalf("应剩 1 条，实际 %d", left)
	}
}
