// Package store 提供群配置、会话注册表与延时删除队列的持久化实现。
//
// 默认 SQLite（纯 Go 驱动，CGO_ENABLED=0 即可出单二进制），可选 MySQL；两者共用同一
// 份 SQL 与同一份契约测试。memory 实现只用于测试：进程重启会丢失待验证会话。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"

	// SQLite 使用纯 Go 驱动；MySQL 用官方驱动。
	_ "github.com/go-sql-driver/mysql"
	_ "modernc.org/sqlite"
)

// schema 是建表语句。两种方言各一份，字段语义完全一致。
var schemas = map[string][]string{
	"sqlite": {
		`CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS groups (
			chat_id INTEGER PRIMARY KEY,
			enabled INTEGER NOT NULL DEFAULT 1,
			mode TEXT NOT NULL DEFAULT 'join',
			rules_message_id INTEGER NOT NULL DEFAULT 0,
			welcome TEXT NOT NULL DEFAULT '',
			tag TEXT NOT NULL DEFAULT '',
			ad_words TEXT NOT NULL DEFAULT '',
			revision INTEGER NOT NULL DEFAULT 1,
			updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			chat_id INTEGER NOT NULL,
			user_id INTEGER NOT NULL,
			update_id INTEGER NOT NULL DEFAULT 0,
			mode TEXT NOT NULL,
			state TEXT NOT NULL,
			options_json TEXT NOT NULL DEFAULT '',
			correct_hash BLOB NOT NULL,
			dataset_revision TEXT NOT NULL DEFAULT '',
			challenge_chat_id INTEGER NOT NULL DEFAULT 0,
			challenge_message_id INTEGER NOT NULL DEFAULT 0,
			admin_chat_id INTEGER NOT NULL DEFAULT 0,
			admin_message_id INTEGER NOT NULL DEFAULT 0,
			join_chat_id INTEGER NOT NULL DEFAULT 0,
			join_message_id INTEGER NOT NULL DEFAULT 0,
			request_user_chat_id INTEGER NOT NULL DEFAULT 0,
			started_at INTEGER NOT NULL DEFAULT 0,
			expires_at INTEGER NOT NULL DEFAULT 0,
			version INTEGER NOT NULL DEFAULT 1,
			updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_key ON sessions(chat_id, user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_state ON sessions(state, expires_at)`,
		`CREATE TABLE IF NOT EXISTS effects (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			payload_json TEXT NOT NULL DEFAULT '{}',
			due_at INTEGER NOT NULL DEFAULT 0,
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_effects_due ON effects(due_at)`,
		`CREATE INDEX IF NOT EXISTS idx_effects_session ON effects(session_id)`,
		`CREATE TABLE IF NOT EXISTS deletions (
			id TEXT PRIMARY KEY,
			chat_id INTEGER NOT NULL,
			message_id INTEGER NOT NULL,
			due_at INTEGER NOT NULL DEFAULT 0,
			lease_until INTEGER NOT NULL DEFAULT 0,
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_deletions_due ON deletions(due_at)`,
	},
	"mysql": {
		"CREATE TABLE IF NOT EXISTS meta (k VARCHAR(64) PRIMARY KEY, v VARCHAR(255) NOT NULL) ENGINE=InnoDB",
		`CREATE TABLE IF NOT EXISTS ` + "`groups`" + ` (
			chat_id BIGINT PRIMARY KEY,
			enabled TINYINT NOT NULL DEFAULT 1,
			mode VARCHAR(16) NOT NULL DEFAULT 'join',
			rules_message_id BIGINT NOT NULL DEFAULT 0,
			welcome TEXT NOT NULL,
			tag VARCHAR(255) NOT NULL DEFAULT '',
			ad_words TEXT NOT NULL,
			revision BIGINT UNSIGNED NOT NULL DEFAULT 1,
			updated_at BIGINT NOT NULL DEFAULT 0
		) ENGINE=InnoDB`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id VARCHAR(32) PRIMARY KEY,
			chat_id BIGINT NOT NULL,
			user_id BIGINT NOT NULL,
			update_id BIGINT NOT NULL DEFAULT 0,
			mode VARCHAR(16) NOT NULL,
			state VARCHAR(16) NOT NULL,
			options_json MEDIUMTEXT NOT NULL,
			correct_hash VARBINARY(32) NOT NULL,
			dataset_revision VARCHAR(64) NOT NULL DEFAULT '',
			challenge_chat_id BIGINT NOT NULL DEFAULT 0,
			challenge_message_id BIGINT NOT NULL DEFAULT 0,
			admin_chat_id BIGINT NOT NULL DEFAULT 0,
			admin_message_id BIGINT NOT NULL DEFAULT 0,
			join_chat_id BIGINT NOT NULL DEFAULT 0,
			join_message_id BIGINT NOT NULL DEFAULT 0,
			request_user_chat_id BIGINT NOT NULL DEFAULT 0,
			started_at BIGINT NOT NULL DEFAULT 0,
			expires_at BIGINT NOT NULL DEFAULT 0,
			version BIGINT UNSIGNED NOT NULL DEFAULT 1,
			updated_at BIGINT NOT NULL DEFAULT 0,
			INDEX idx_sessions_key (chat_id, user_id),
			INDEX idx_sessions_state (state, expires_at)
		) ENGINE=InnoDB`,
		`CREATE TABLE IF NOT EXISTS effects (
			id VARCHAR(32) PRIMARY KEY,
			session_id VARCHAR(32) NOT NULL,
			kind VARCHAR(32) NOT NULL,
			payload_json TEXT NOT NULL,
			due_at BIGINT NOT NULL DEFAULT 0,
			attempts INT NOT NULL DEFAULT 0,
			last_error VARCHAR(512) NOT NULL DEFAULT '',
			created_at BIGINT NOT NULL DEFAULT 0,
			INDEX idx_effects_due (due_at),
			INDEX idx_effects_session (session_id)
		) ENGINE=InnoDB`,
		`CREATE TABLE IF NOT EXISTS deletions (
			id VARCHAR(48) PRIMARY KEY,
			chat_id BIGINT NOT NULL,
			message_id BIGINT NOT NULL,
			due_at BIGINT NOT NULL DEFAULT 0,
			lease_until BIGINT NOT NULL DEFAULT 0,
			attempts INT NOT NULL DEFAULT 0,
			last_error VARCHAR(512) NOT NULL DEFAULT '',
			created_at BIGINT NOT NULL DEFAULT 0,
			INDEX idx_deletions_due (due_at)
		) ENGINE=InnoDB`,
	},
}

// SQL 是共享的数据库句柄。
type SQL struct {
	db      *sql.DB
	dialect string
}

// Open 按配置打开数据库并（可选）自动建表。
func Open(ctx context.Context, cfg *config.Config, backend string) (*SQL, error) {
	switch strings.ToLower(backend) {
	case "sqlite":
		return openSQLite(ctx, cfg)
	case "mysql":
		return openMySQL(ctx, cfg)
	default:
		return nil, fmt.Errorf("不支持的 SQL 后端 %q", backend)
	}
}

func openSQLite(ctx context.Context, cfg *config.Config) (*SQL, error) {
	s := cfg.Storage.SQLite
	// 目录不存在时 SQLite 只会报 "unable to open database file"，对首次部署很不友好：
	// 这里主动创建父目录（例如全新克隆后 ./data 还不存在、或运营改成了 ./var/state.db）。
	if dir := filepath.Dir(strings.TrimPrefix(s.Path, "file:")); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("创建 SQLite 目录 %s 失败: %w", dir, err)
		}
	}
	dsn := "file:" + strings.TrimPrefix(s.Path, "file:")
	pragmas := []string{"_pragma=busy_timeout(5000)"}
	if s.BusyTimeoutMS > 0 {
		pragmas = []string{fmt.Sprintf("_pragma=busy_timeout(%d)", s.BusyTimeoutMS)}
	}
	if s.WAL {
		pragmas = append(pragmas, "_pragma=journal_mode(WAL)", "_pragma=synchronous(NORMAL)")
	}
	pragmas = append(pragmas, "_pragma=foreign_keys(1)")
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	dsn = dsn + sep + strings.Join(pragmas, "&")

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 失败: %w", err)
	}
	maxOpen := s.MaxOpenConnections
	if maxOpen <= 0 {
		// SQLite 写事务靠单连接串行化，避免多写并发导致的 BUSY 升级死锁。
		maxOpen = 1
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxOpen)
	db.SetConnMaxLifetime(0)
	h := &SQL{db: db, dialect: "sqlite"}
	if err := h.ping(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if cfg.Storage.AutoMigrate {
		if err := h.Migrate(ctx); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return h, nil
}

func openMySQL(ctx context.Context, cfg *config.Config) (*SQL, error) {
	m := cfg.Storage.MySQL
	if strings.TrimSpace(m.DSN) == "" {
		return nil, errors.New("storage.mysql.dsn 为空")
	}
	// 统一 UTC：deadline 在库里是 Unix 秒，Go 层只做展示时转换。
	dsn := m.DSN
	if !strings.Contains(dsn, "parseTime=") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dsn += sep + "parseTime=true&loc=UTC"
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 MySQL 失败: %w", err)
	}
	maxOpen := m.MaxOpenConnections
	if maxOpen <= 0 {
		maxOpen = 10
	}
	maxIdle := m.MaxIdleConnections
	if maxIdle <= 0 {
		maxIdle = 5
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	if m.ConnectionMaxLifetimeSeconds > 0 {
		db.SetConnMaxLifetime(time.Duration(m.ConnectionMaxLifetimeSeconds) * time.Second)
	}
	h := &SQL{db: db, dialect: "mysql"}
	if err := h.ping(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if cfg.Storage.AutoMigrate {
		if err := h.Migrate(ctx); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return h, nil
}

func (s *SQL) ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("连接数据库失败: %w", err)
	}
	return nil
}

// Migrate 建表（幂等）。
func (s *SQL) Migrate(ctx context.Context) error {
	stmts, ok := schemas[s.dialect]
	if !ok {
		return fmt.Errorf("没有 %s 的建表脚本", s.dialect)
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("建表失败 (%s): %w", firstLine(stmt), err)
		}
	}
	_, err := s.db.ExecContext(ctx,
		s.rebind(`INSERT INTO meta (k, v) VALUES (?, ?)`), "schema_version", "1")
	if err != nil && !isDuplicateErr(err) {
		return fmt.Errorf("写入 schema_version 失败: %w", err)
	}
	return nil
}

// Close 关闭连接。
func (s *SQL) Close() error { return s.db.Close() }

// DB 暴露底层连接（供需要自己拼事务的调用方使用）。
func (s *SQL) DB() *sql.DB { return s.db }

func (s *SQL) rebind(q string) string { return q }

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func isDuplicateErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate") || strings.Contains(msg, "unique constraint")
}

// ---------------------------------------------------------------- 群配置

// GroupStore 是 SQL 版群配置存储，用 revision 做乐观并发。
type GroupStore struct{ h *SQL }

// NewGroupStore 构造群配置存储。
func NewGroupStore(h *SQL) *GroupStore { return &GroupStore{h: h} }

// EnsureGroup 在群首次出现时写入默认配置；已存在则原样返回。
func (g *GroupStore) EnsureGroup(ctx context.Context, chatID int64, defaults domain.GroupConfig) (domain.GroupConfig, error) {
	cfg, err := g.GetGroup(ctx, chatID)
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, ports.ErrNotFound) {
		return domain.GroupConfig{}, err
	}
	now := time.Now().Unix()
	_, err = g.h.db.ExecContext(ctx, g.h.rebind(`
		INSERT INTO groups (chat_id, enabled, mode, rules_message_id, welcome, tag, ad_words, revision, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?)`),
		chatID, boolToInt(defaults.Enabled), string(defaults.Mode), defaults.RulesMessageID,
		defaults.Welcome, defaults.Tag, strings.Join(defaults.AdWords, "\n"), now)
	if err != nil {
		if isDuplicateErr(err) {
			return g.GetGroup(ctx, chatID)
		}
		return domain.GroupConfig{}, fmt.Errorf("创建群配置失败: %w", err)
	}
	return g.GetGroup(ctx, chatID)
}

// GetGroup 读取群配置。
func (g *GroupStore) GetGroup(ctx context.Context, chatID int64) (domain.GroupConfig, error) {
	row := g.h.db.QueryRowContext(ctx, g.h.rebind(`
		SELECT chat_id, enabled, mode, rules_message_id, welcome, tag, ad_words, revision, updated_at
		FROM groups WHERE chat_id = ?`), chatID)
	return scanGroup(row.Scan)
}

// SaveGroup 以 CAS 方式保存群配置：expectedRevision 必须与库中一致。
func (g *GroupStore) SaveGroup(ctx context.Context, cfg domain.GroupConfig, expectedRevision uint64) (domain.GroupConfig, error) {
	now := time.Now().Unix()
	if expectedRevision == 0 {
		_, err := g.h.db.ExecContext(ctx, g.h.rebind(`
			INSERT INTO groups (chat_id, enabled, mode, rules_message_id, welcome, tag, ad_words, revision, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?)`),
			cfg.ChatID, boolToInt(cfg.Enabled), string(cfg.Mode), cfg.RulesMessageID,
			cfg.Welcome, cfg.Tag, strings.Join(cfg.AdWords, "\n"), now)
		if err != nil {
			if isDuplicateErr(err) {
				return domain.GroupConfig{}, ports.ErrConflict
			}
			return domain.GroupConfig{}, fmt.Errorf("写入群配置失败: %w", err)
		}
		return g.GetGroup(ctx, cfg.ChatID)
	}
	res, err := g.h.db.ExecContext(ctx, g.h.rebind(`
		UPDATE groups SET enabled = ?, mode = ?, rules_message_id = ?, welcome = ?, tag = ?, ad_words = ?,
			revision = revision + 1, updated_at = ?
		WHERE chat_id = ? AND revision = ?`),
		boolToInt(cfg.Enabled), string(cfg.Mode), cfg.RulesMessageID, cfg.Welcome, cfg.Tag,
		strings.Join(cfg.AdWords, "\n"), now, cfg.ChatID, expectedRevision)
	if err != nil {
		return domain.GroupConfig{}, fmt.Errorf("更新群配置失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return domain.GroupConfig{}, fmt.Errorf("更新群配置失败: %w", err)
	}
	if n == 0 {
		return domain.GroupConfig{}, ports.ErrConflict
	}
	return g.GetGroup(ctx, cfg.ChatID)
}

// Close 由共享句柄负责关闭。
func (g *GroupStore) Close() error { return nil }

func scanGroup(scan func(...any) error) (domain.GroupConfig, error) {
	var (
		cfg     domain.GroupConfig
		enabled int
		mode    string
		adWords string
		updated int64
	)
	if err := scan(&cfg.ChatID, &enabled, &mode, &cfg.RulesMessageID, &cfg.Welcome, &cfg.Tag, &adWords, &cfg.Revision, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.GroupConfig{}, ports.ErrNotFound
		}
		return domain.GroupConfig{}, err
	}
	cfg.Enabled = enabled != 0
	cfg.Mode = domain.VerifyMode(mode)
	if !cfg.Mode.Valid() {
		cfg.Mode = domain.VerifyModeJoin
	}
	for _, w := range strings.Split(adWords, "\n") {
		if w = strings.TrimSpace(w); w != "" {
			cfg.AdWords = append(cfg.AdWords, w)
		}
	}
	cfg.UpdatedAt = time.Unix(updated, 0)
	return cfg, nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------- 会话注册表

// Registry 是 SQL 版会话注册表。
//
// Claim 在一个事务里完成"核对 + 判定 + 落终态 + 写副作用 outbox"，
// 因此并发点击、超时与管理员操作只有一个能赢。
type Registry struct{ h *SQL }

// NewRegistry 构造会话注册表。
func NewRegistry(h *SQL) *Registry { return &Registry{h: h} }

const sessionCols = `id, chat_id, user_id, update_id, mode, state, options_json, correct_hash,
	dataset_revision, challenge_chat_id, challenge_message_id, admin_chat_id, admin_message_id,
	join_chat_id, join_message_id, request_user_chat_id, started_at, expires_at, version`

// Begin 开始一次会话：同 update 幂等；同 key 的旧世代会被替换并作废其未执行副作用。
func (r *Registry) Begin(ctx context.Context, s domain.Session) (ports.BeginResult, error) {
	tx, err := r.h.db.BeginTx(ctx, nil)
	if err != nil {
		return ports.BeginResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := queryActiveTx(ctx, r.h, tx, s.Key)
	switch {
	case err == nil:
		if existing.UpdateID == s.UpdateID && existing.UpdateID != 0 {
			return ports.BeginResult{Session: existing, Duplicate: true}, tx.Commit()
		}
		if _, err := tx.ExecContext(ctx, r.h.rebind(
			`UPDATE sessions SET state = ?, version = version + 1, updated_at = ? WHERE id = ?`),
			string(domain.StateCancelled), time.Now().Unix(), existing.ID); err != nil {
			return ports.BeginResult{}, fmt.Errorf("替换旧会话失败: %w", err)
		}
		if _, err := tx.ExecContext(ctx, r.h.rebind(`DELETE FROM effects WHERE session_id = ?`), existing.ID); err != nil {
			return ports.BeginResult{}, fmt.Errorf("清理旧会话副作用失败: %w", err)
		}
	case errors.Is(err, ports.ErrNotFound):
		// 正常路径：没有活动会话。
	default:
		return ports.BeginResult{}, err
	}

	options, err := json.Marshal(s.Options)
	if err != nil {
		return ports.BeginResult{}, fmt.Errorf("序列化选项失败: %w", err)
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, r.h.rebind(`
		INSERT INTO sessions (`+sessionCols+`, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`),
		s.ID, s.Key.ChatID, s.Key.UserID, s.UpdateID, string(s.Mode), string(s.State),
		string(options), s.CorrectTokenHash, s.DatasetRevision,
		s.ChallengeMessage.ChatID, s.ChallengeMessage.MessageID,
		s.AdminMessage.ChatID, s.AdminMessage.MessageID,
		s.JoinMessage.ChatID, s.JoinMessage.MessageID,
		s.RequestUserChatID, s.StartedAt.Unix(), s.ExpiresAt.Unix(), now); err != nil {
		if isDuplicateErr(err) {
			return ports.BeginResult{}, ports.ErrConflict
		}
		return ports.BeginResult{}, fmt.Errorf("创建会话失败: %w", err)
	}
	res := ports.BeginResult{Session: s}
	if err == nil && existing.ID != "" {
		old := existing
		res.Replaced = &old
	}
	if err := tx.Commit(); err != nil {
		return ports.BeginResult{}, err
	}
	return res, nil
}

// AttachMessages 在题目发出后把消息引用与 deadline 写入会话（CAS）。
func (r *Registry) AttachMessages(ctx context.Context, key domain.SessionKey, sessionID string,
	challenge, admin domain.MessageRef, expiresAt time.Time, expectedVersion uint64) (domain.Session, error) {
	res, err := r.h.db.ExecContext(ctx, r.h.rebind(`
		UPDATE sessions SET challenge_chat_id = ?, challenge_message_id = ?,
			admin_chat_id = ?, admin_message_id = ?, expires_at = ?, state = ?,
			version = version + 1, updated_at = ?
		WHERE id = ? AND chat_id = ? AND user_id = ? AND version = ?`),
		challenge.ChatID, challenge.MessageID, admin.ChatID, admin.MessageID,
		expiresAt.Unix(), string(domain.StatePending), time.Now().Unix(),
		sessionID, key.ChatID, key.UserID, expectedVersion)
	if err != nil {
		return domain.Session{}, fmt.Errorf("更新会话消息失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return domain.Session{}, err
	}
	if n == 0 {
		return domain.Session{}, ports.ErrConflict
	}
	return r.Get(ctx, key)
}

// Get 读取当前活动会话；没有活动会话时返回 ErrNotFound。
func (r *Registry) Get(ctx context.Context, key domain.SessionKey) (domain.Session, error) {
	row := r.h.db.QueryRowContext(ctx, r.h.rebind(`
		SELECT `+sessionCols+` FROM sessions
		WHERE chat_id = ? AND user_id = ? AND state IN (?, ?)
		ORDER BY started_at DESC LIMIT 1`),
		key.ChatID, key.UserID, string(domain.StatePreparing), string(domain.StatePending))
	return scanSession(row.Scan)
}

// GetByID 按 session id 读取会话（不限状态）。
func (r *Registry) GetByID(ctx context.Context, sessionID string) (domain.Session, error) {
	row := r.h.db.QueryRowContext(ctx, r.h.rebind(
		`SELECT `+sessionCols+` FROM sessions WHERE id = ?`), sessionID)
	return scanSession(row.Scan)
}

// Claim 抢占终态：只有一个合法事件能赢。
func (r *Registry) Claim(ctx context.Context, key domain.SessionKey, event domain.Event, plan ports.EffectPlanner) (ports.ClaimResult, error) {
	tx, err := r.h.db.BeginTx(ctx, nil)
	if err != nil {
		return ports.ClaimResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	sess, err := queryActiveTx(ctx, r.h, tx, key)
	if errors.Is(err, ports.ErrNotFound) {
		last, lastErr := queryLastTx(ctx, r.h, tx, key)
		if lastErr == nil {
			if event.SessionID != "" && last.ID != event.SessionID {
				return ports.ClaimResult{Reason: ports.ClaimReplaced, Before: last, After: last}, nil
			}
			return ports.ClaimResult{Reason: ports.ClaimDone, Before: last, After: last}, nil
		}
		return ports.ClaimResult{Reason: ports.ClaimUnknown}, nil
	}
	if err != nil {
		return ports.ClaimResult{}, err
	}

	before := sess
	// session 世代校验：旧题按钮不可能作用到新会话。
	if event.SessionID != "" && event.SessionID != sess.ID {
		return ports.ClaimResult{Reason: ports.ClaimReplaced, Before: before, After: before}, nil
	}

	target, reason := decide(before, event)
	if reason != "" {
		return ports.ClaimResult{Reason: reason, Before: before, After: before}, nil
	}
	after := before
	after.State = target
	after.Version = before.Version + 1

	if _, err := tx.ExecContext(ctx, r.h.rebind(
		`UPDATE sessions SET state = ?, version = version + 1, updated_at = ? WHERE id = ? AND version = ?`),
		string(target), time.Now().Unix(), before.ID, before.Version); err != nil {
		return ports.ClaimResult{}, fmt.Errorf("写入会话终态失败: %w", err)
	}

	var effects []domain.Effect
	if plan != nil {
		effects = plan(before, after, event)
	}
	for i := range effects {
		if effects[i].ID == "" {
			id, err := domain.NewEffectID()
			if err != nil {
				return ports.ClaimResult{}, err
			}
			effects[i].ID = id
		}
		effects[i].SessionID = before.ID
		if effects[i].DueAt.IsZero() {
			effects[i].DueAt = time.Now()
		}
		if err := insertEffectTx(ctx, r.h, tx, effects[i]); err != nil {
			return ports.ClaimResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return ports.ClaimResult{}, err
	}
	return ports.ClaimResult{Won: true, Before: before, After: after, Effects: effects}, nil
}

// decide 是状态机核心：返回目标状态；reason 非空表示该事件不能赢得终态。
func decide(s domain.Session, ev domain.Event) (domain.SessionState, string) {
	switch ev.Kind {
	case domain.EventAnswer:
		if s.State == domain.StatePreparing {
			return "", ports.ClaimNotDue
		}
		if s.State != domain.StatePending {
			return "", ports.ClaimDone
		}
		if s.ExpiresAt.Unix() > 0 && !ev.At.Before(s.ExpiresAt.Add(time.Second)) {
			// 到点之后只允许 timeout 赢，避免"超时踢出"和"答对放行"同时生效。
			return "", ports.ClaimExpired
		}
		if !domain.MatchesHash(ev.OptionToken, s.CorrectTokenHash) {
			if _, ok := s.FindOption(ev.OptionToken); !ok {
				return "", ports.ClaimBadToken
			}
			return domain.StateWrong, ""
		}
		return domain.StatePassed, ""
	case domain.EventTimeout:
		if s.ExpiresAt.Unix() > 0 && ev.At.Before(s.ExpiresAt) {
			return "", ports.ClaimNotDue
		}
		return domain.StateTimedOut, ""
	case domain.EventAdminPass:
		if s.State == domain.StatePreparing {
			return "", ports.ClaimNotDue
		}
		return domain.StateAdminPass, ""
	case domain.EventAdminBan:
		return domain.StateAdminBan, ""
	case domain.EventSpoke:
		if s.State == domain.StatePreparing {
			// 还在准备阶段就发言：同样按违规处理。
			return domain.StateSpoke, ""
		}
		return domain.StateSpoke, ""
	case domain.EventLeft:
		return domain.StateLeft, ""
	case domain.EventCancel:
		return domain.StateCancelled, ""
	default:
		return "", ports.ClaimUnknown
	}
}

// ListActive 返回未终结会话；expiredOnly 时只返回已过期者。
func (r *Registry) ListActive(ctx context.Context, now time.Time, expiredOnly bool, limit int) ([]domain.Session, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT ` + sessionCols + ` FROM sessions WHERE state IN (?, ?)`
	args := []any{string(domain.StatePreparing), string(domain.StatePending)}
	if expiredOnly {
		q += ` AND expires_at > 0 AND expires_at <= ?`
		args = append(args, now.Unix())
	}
	q += ` ORDER BY expires_at ASC LIMIT ?`
	args = append(args, limit)

	rows, err := r.h.db.QueryContext(ctx, r.h.rebind(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Session
	for rows.Next() {
		s, err := scanSession(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListDueEffects 返回到期的副作用。
func (r *Registry) ListDueEffects(ctx context.Context, now time.Time, limit int) ([]domain.Effect, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.h.db.QueryContext(ctx, r.h.rebind(`
		SELECT id, session_id, kind, payload_json, due_at, attempts, last_error
		FROM effects WHERE due_at <= ? ORDER BY due_at ASC LIMIT ?`), now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Effect
	for rows.Next() {
		e, err := scanEffect(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AckEffect 标记副作用已成功。
func (r *Registry) AckEffect(ctx context.Context, effectID string) error {
	_, err := r.h.db.ExecContext(ctx, r.h.rebind(`DELETE FROM effects WHERE id = ?`), effectID)
	return err
}

// RetryEffect 记录失败并安排下次重试。
func (r *Registry) RetryEffect(ctx context.Context, effectID string, next time.Time, cause string) error {
	if len(cause) > 400 {
		cause = cause[:400]
	}
	_, err := r.h.db.ExecContext(ctx, r.h.rebind(
		`UPDATE effects SET attempts = attempts + 1, due_at = ?, last_error = ? WHERE id = ?`),
		next.Unix(), cause, effectID)
	return err
}

// CancelEffectsOf 作废某会话的未执行副作用（例如管理员永久封禁后撤销临时解封）。
func (r *Registry) CancelEffectsOf(ctx context.Context, sessionID string, kinds ...domain.EffectKind) (int, error) {
	if sessionID == "" {
		return 0, nil
	}
	q := `DELETE FROM effects WHERE session_id = ?`
	args := []any{sessionID}
	if len(kinds) > 0 {
		placeholders := make([]string, 0, len(kinds))
		for _, k := range kinds {
			placeholders = append(placeholders, "?")
			args = append(args, string(k))
		}
		q += ` AND kind IN (` + strings.Join(placeholders, ",") + `)`
	}
	res, err := r.h.db.ExecContext(ctx, r.h.rebind(q), args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// Close 由共享句柄负责关闭。
func (r *Registry) Close() error { return nil }

func queryActiveTx(ctx context.Context, h *SQL, tx *sql.Tx, key domain.SessionKey) (domain.Session, error) {
	q := `SELECT ` + sessionCols + ` FROM sessions
		WHERE chat_id = ? AND user_id = ? AND state IN (?, ?)
		ORDER BY started_at DESC LIMIT 1`
	if h.dialect == "mysql" {
		q += " FOR UPDATE"
	}
	row := tx.QueryRowContext(ctx, h.rebind(q), key.ChatID, key.UserID,
		string(domain.StatePreparing), string(domain.StatePending))
	return scanSession(row.Scan)
}

func queryLastTx(ctx context.Context, h *SQL, tx *sql.Tx, key domain.SessionKey) (domain.Session, error) {
	row := tx.QueryRowContext(ctx, h.rebind(`SELECT `+sessionCols+` FROM sessions
		WHERE chat_id = ? AND user_id = ? ORDER BY started_at DESC LIMIT 1`), key.ChatID, key.UserID)
	return scanSession(row.Scan)
}

func scanSession(scan func(...any) error) (domain.Session, error) {
	var (
		s         domain.Session
		options   string
		mode      string
		state     string
		started   int64
		expires   int64
		chHash    []byte
		rowChatID int64
	)
	err := scan(&s.ID, &rowChatID, &s.Key.UserID, &s.UpdateID, &mode, &state, &options, &chHash,
		&s.DatasetRevision, &s.ChallengeMessage.ChatID, &s.ChallengeMessage.MessageID,
		&s.AdminMessage.ChatID, &s.AdminMessage.MessageID,
		&s.JoinMessage.ChatID, &s.JoinMessage.MessageID,
		&s.RequestUserChatID, &started, &expires, &s.Version)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Session{}, ports.ErrNotFound
		}
		return domain.Session{}, err
	}
	s.Key.ChatID = rowChatID
	s.Mode = domain.SessionMode(mode)
	s.State = domain.SessionState(state)
	s.CorrectTokenHash = chHash
	s.StartedAt = time.Unix(started, 0)
	s.ExpiresAt = time.Unix(expires, 0)
	if options != "" {
		if err := json.Unmarshal([]byte(options), &s.Options); err != nil {
			return domain.Session{}, fmt.Errorf("解析会话选项失败: %w", err)
		}
	}
	return s, nil
}

func insertEffectTx(ctx context.Context, h *SQL, tx *sql.Tx, e domain.Effect) error {
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return fmt.Errorf("序列化副作用失败: %w", err)
	}
	_, err = tx.ExecContext(ctx, h.rebind(`
		INSERT INTO effects (id, session_id, kind, payload_json, due_at, attempts, last_error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		e.ID, e.SessionID, string(e.Kind), string(payload), e.DueAt.Unix(), e.Attempts, e.LastError, time.Now().Unix())
	return err
}

func scanEffect(scan func(...any) error) (domain.Effect, error) {
	var (
		e       domain.Effect
		kind    string
		payload string
		due     int64
	)
	if err := scan(&e.ID, &e.SessionID, &kind, &payload, &due, &e.Attempts, &e.LastError); err != nil {
		return domain.Effect{}, err
	}
	e.Kind = domain.EffectKind(kind)
	e.DueAt = time.Unix(due, 0)
	if payload != "" {
		if err := json.Unmarshal([]byte(payload), &e.Payload); err != nil {
			return domain.Effect{}, fmt.Errorf("解析副作用参数失败: %w", err)
		}
	}
	return e, nil
}

// ---------------------------------------------------------------- 延时删除队列

// DeletionQueue 是 SQL 版延时删除队列（lease/ack/nack）。
type DeletionQueue struct{ h *SQL }

// NewDeletionQueue 构造删除队列。
func NewDeletionQueue(h *SQL) *DeletionQueue { return &DeletionQueue{h: h} }

// Enqueue 入队；同一个 (chat,message) 重复入队是幂等的。
func (q *DeletionQueue) Enqueue(ctx context.Context, msg domain.MessageRef, dueAt time.Time) error {
	if !msg.Valid() {
		return fmt.Errorf("非法的消息引用 %+v", msg)
	}
	id := fmt.Sprintf("%d:%d", msg.ChatID, msg.MessageID)
	_, err := q.h.db.ExecContext(ctx, q.h.rebind(`
		INSERT INTO deletions (id, chat_id, message_id, due_at, lease_until, attempts, last_error, created_at)
		VALUES (?, ?, ?, ?, 0, 0, '', ?)`),
		id, msg.ChatID, msg.MessageID, dueAt.Unix(), time.Now().Unix())
	if err != nil {
		if isDuplicateErr(err) {
			// 重复入队：把到期时间更新为更早的那个，避免重复任务互相推迟。
			_, err = q.h.db.ExecContext(ctx, q.h.rebind(
				`UPDATE deletions SET due_at = ? WHERE id = ? AND due_at > ?`),
				dueAt.Unix(), id, dueAt.Unix())
			return err
		}
		return fmt.Errorf("入队删除任务失败: %w", err)
	}
	return nil
}

// LeaseDue 领取到期的删除任务并加租约。
func (q *DeletionQueue) LeaseDue(ctx context.Context, now time.Time, limit int, lease time.Duration) ([]ports.DeleteJob, error) {
	if limit <= 0 {
		limit = 50
	}
	tx, err := q.h.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	q2 := `SELECT id, chat_id, message_id, due_at, lease_until, attempts, last_error
		FROM deletions WHERE due_at <= ? AND (lease_until <= ?)
		ORDER BY due_at ASC LIMIT ?`
	if q.h.dialect == "mysql" {
		q2 += " FOR UPDATE SKIP LOCKED"
	}
	rows, err := tx.QueryContext(ctx, q.h.rebind(q2), now.Unix(), now.Unix(), limit)
	if err != nil {
		return nil, err
	}
	var jobs []ports.DeleteJob
	for rows.Next() {
		var (
			j       ports.DeleteJob
			due     int64
			leaseAt int64
		)
		if err := rows.Scan(&j.ID, &j.Message.ChatID, &j.Message.MessageID, &due, &leaseAt, &j.Attempts, &j.LastError); err != nil {
			rows.Close()
			return nil, err
		}
		j.DueAt = time.Unix(due, 0)
		j.LeaseUntil = time.Unix(leaseAt, 0)
		jobs = append(jobs, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	leaseUntil := now.Add(lease).Unix()
	for _, j := range jobs {
		if _, err := tx.ExecContext(ctx, q.h.rebind(
			`UPDATE deletions SET lease_until = ? WHERE id = ?`), leaseUntil, j.ID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return jobs, nil
}

// Ack 标记任务完成并删除。
func (q *DeletionQueue) Ack(ctx context.Context, jobID string) error {
	_, err := q.h.db.ExecContext(ctx, q.h.rebind(`DELETE FROM deletions WHERE id = ?`), jobID)
	return err
}

// Nack 安排重试。
func (q *DeletionQueue) Nack(ctx context.Context, jobID string, retryAt time.Time, cause string) error {
	if len(cause) > 400 {
		cause = cause[:400]
	}
	_, err := q.h.db.ExecContext(ctx, q.h.rebind(
		`UPDATE deletions SET attempts = attempts + 1, due_at = ?, lease_until = 0, last_error = ? WHERE id = ?`),
		retryAt.Unix(), cause, jobID)
	return err
}

// Close 由共享句柄负责关闭。
func (q *DeletionQueue) Close() error { return nil }
