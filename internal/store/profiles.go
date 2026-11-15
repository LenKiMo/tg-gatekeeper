package store

import (
	"context"
	"fmt"

	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
)

// SaveProfile 写入一条脱敏的入群资料快照。
//
// 注意：这张表里没有 user_id 列，只有调用方算好的 ref。加列之前先问一句"这个字段
// 能不能反查到人"——能反查就不要加，改用 audit.jsonl（那条线本来就是处置记录）。
func (s *SQL) SaveProfile(ctx context.Context, p ports.ProfileSnapshot) error {
	_, err := s.db.ExecContext(ctx, s.rebind(`INSERT INTO profile_snapshots
			(ref, chat_id, display_name, bio, has_username, has_photo,
			 name_shape, decision, rule, word, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		p.Ref, p.ChatID, p.DisplayName, p.Bio, boolToInt(p.HasUsername), boolToInt(p.HasPhoto),
		p.NameShape, p.Decision, p.Rule, p.Word, p.CreatedAt, p.ExpiresAt)
	if err != nil {
		return fmt.Errorf("写入资料快照失败: %w", err)
	}
	return nil
}

// PurgeProfilesBefore 删除 CreatedAt < cutoff 的快照，返回删除条数。
//
// 保留期到了就删：这份数据只有"近期语料"的用途，留着只会变成负资产。
func (s *SQL) PurgeProfilesBefore(ctx context.Context, cutoff int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		s.rebind(`DELETE FROM profile_snapshots WHERE created_at < ?`), cutoff)
	if err != nil {
		return 0, fmt.Errorf("清理资料快照失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
