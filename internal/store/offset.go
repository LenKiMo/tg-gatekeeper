package store

// update offset 的持久化。
//
// 为什么需要它：Telegram 的 getUpdates 是"确认制"——下次请求带 offset=N+1 就表示
// N 及之前的更新已经消费。offset 只存在进程内存里时，进程重启会从"当前最新"重新开始，
// 停机窗口内的入群事件（left→member 的 chat_member）就永远收不到了，表现为"有人进群、
// bot 毫无反应"，而且日志与审计里一条记录都没有。
//
// 这里把 offset 落进 meta 表，重启后从上次消费的位置继续拉。单次写入很便宜（一行
// UPSERT），但群消息量大时没必要每条都写，所以调用方按批节流（见 Router.Run）。

import (
	"context"
	"strconv"
)

const updateOffsetKey = "update_offset"

// LoadUpdateOffset 返回上次持久化的 offset（0 表示从头开始或非 SQLite 存储）。
func (s *Set) LoadUpdateOffset(ctx context.Context) int64 {
	if s.SQL == nil {
		return 0
	}
	var v string
	err := s.SQL.DB().QueryRowContext(ctx,
		`SELECT v FROM meta WHERE k = ?`, updateOffsetKey).Scan(&v)
	if err != nil {
		return 0
	}
	n, perr := strconv.ParseInt(v, 10, 64)
	if perr != nil || n < 0 {
		return 0
	}
	return n
}

// SaveUpdateOffset 写入 offset（幂等 UPSERT）。
func (s *Set) SaveUpdateOffset(ctx context.Context, offset int64) error {
	if s.SQL == nil || offset <= 0 {
		return nil
	}
	_, err := s.SQL.DB().ExecContext(ctx,
		`INSERT INTO meta (k, v) VALUES (?, ?)
		 ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
		updateOffsetKey, strconv.FormatInt(offset, 10))
	return err
}
