package store

import (
	"context"
	"fmt"
	"time"
)

// member_baseline 的读写。
//
// 这张表回答一个问题："这个 user_id 是我们知道的人吗？"——用于懒触发复查：
// 一个在群里发言、但既不在 baseline 也没有验证记录的 id，说明它的入群事件从未送达
// （停机丢事件、relay 丢更新等），需要补一次检查。没有这条基线，bot 上线前就在群的
// 老成员第一次发言时会被误判成"来路不明"。

// UpsertBaseline 批量写入基线（source 标明来路：import / observed / verified）。
func (s *SQL) UpsertBaseline(ctx context.Context, chatID int64, ids []int64, source string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, s.rebind(
		`INSERT INTO member_baseline (user_id, chat_id, source, created_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(user_id) DO UPDATE SET chat_id = excluded.chat_id,
		     source = excluded.source`))
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	n := 0
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, err := stmt.ExecContext(ctx, id, chatID, source, now); err != nil {
			return n, fmt.Errorf("写入基线失败 (user_id=%d): %w", id, err)
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// HasBaseline 判断某个 user_id 是否已在基线里。
func (s *SQL) HasBaseline(ctx context.Context, chatID, userID int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT 1 FROM member_baseline WHERE user_id = ? AND (chat_id = ? OR chat_id = 0)`),
		userID, chatID).Scan(&one)
	if err != nil {
		return false, nil // 查不到即"未知"，调用方按未知处理
	}
	return one == 1, nil
}

// ListBaseline 返回某个群的基线 user_id 列表（启动时预热内存缓存用）。
func (s *SQL) ListBaseline(ctx context.Context, chatID int64, limit int) ([]int64, error) {
	if limit <= 0 {
		limit = 10000
	}
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT user_id FROM member_baseline WHERE chat_id = ? OR chat_id = 0 LIMIT ?`),
		chatID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CountBaseline 返回基线规模（运维核对用）。
func (s *SQL) CountBaseline(ctx context.Context, chatID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT count(*) FROM member_baseline WHERE chat_id = ? OR chat_id = 0`), chatID).Scan(&n)
	return n, err
}

// HasPassedSession 判断某个用户是否有过"验证通过"的历史记录。
//
// 懒触发复查要先排除这类人：他们确实被 bot 验证过，只是那次会话已经终结、从活跃表
// 里消失了。没有这条判断，每个通过验证的成员下次发言都会被当成"来路不明"。
func (r *Registry) HasPassedSession(ctx context.Context, chatID, userID int64) (bool, error) {
	var one int
	err := r.h.db.QueryRowContext(ctx, r.h.rebind(
		`SELECT 1 FROM sessions WHERE chat_id = ? AND user_id = ? AND state = ? LIMIT 1`),
		chatID, userID, "passed").Scan(&one)
	if err != nil {
		return false, nil
	}
	return one == 1, nil
}
