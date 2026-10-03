package provider

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"

	// 驱动注册：SQLite 用纯 Go 实现（CGO_ENABLED=0 也能出单二进制）。
	_ "github.com/go-sql-driver/mysql"
	_ "modernc.org/sqlite"
)

// sqlProvider 从 SQL 数据库读取数据集。
//
// query 需要返回带别名的列：id,label,image_ref,pool,groups_json,difficulty,weight,enabled。
// 只允许单条 SELECT，快照在只读事务中完成。
type sqlProvider struct {
	name    string
	driver  string
	dsn     string
	query   string
	timeout time.Duration

	db *sql.DB
}

func newSQL(name string, def config.ProviderDef) (ports.Source, error) {
	d := def.SQL
	driver := strings.ToLower(strings.TrimSpace(d.Driver))
	switch driver {
	case "sqlite", "sqlite3", "":
		driver = "sqlite"
	case "mysql", "mariadb":
		driver = "mysql"
	default:
		return nil, fmt.Errorf("provider %s 的 sql.driver 不支持 %q", name, d.Driver)
	}
	timeout := time.Duration(d.QueryTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &sqlProvider{
		name:    name,
		driver:  driver,
		dsn:     d.DSN,
		query:   d.Query,
		timeout: timeout,
	}, nil
}

func (p *sqlProvider) ID() string { return "sql:" + p.driver }

func (p *sqlProvider) Snapshot(ctx context.Context) (domain.Snapshot, error) {
	ctx = snapshotCtx(ctx)
	if p.db == nil {
		db, err := sql.Open(p.driver, p.dsn)
		if err != nil {
			return domain.Snapshot{}, fmt.Errorf("打开数据集数据库失败: %w", err)
		}
		db.SetMaxOpenConns(4)
		db.SetConnMaxLifetime(10 * time.Minute)
		p.db = db
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	rows, err := p.db.QueryContext(ctx, p.query)
	if err != nil {
		return domain.Snapshot{}, fmt.Errorf("执行数据集查询失败: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return domain.Snapshot{}, fmt.Errorf("读取查询列失败: %w", err)
	}
	idx := map[string]int{}
	for i, c := range cols {
		idx[strings.ToLower(strings.TrimSpace(c))] = i
	}
	get := func(name string, fallback int) int {
		if i, ok := idx[name]; ok {
			return i
		}
		return fallback
	}
	labelIdx := get("label", 1)
	imageIdx := get("image_ref", get("image", 2))
	idIdx := get("id", 0)
	poolIdx := get("pool", -1)
	groupsIdx := get("groups_json", -1)
	diffIdx := get("difficulty", -1)
	weightIdx := get("weight", -1)
	enabledIdx := get("enabled", -1)

	var recs []record
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return domain.Snapshot{}, err
		}
		raw := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return domain.Snapshot{}, fmt.Errorf("扫描数据集行失败: %w", err)
		}
		rec := record{
			ID:         cellString(raw, idIdx),
			Label:      cellString(raw, labelIdx),
			Image:      cellString(raw, imageIdx),
			Pool:       cellString(raw, poolIdx),
			Difficulty: cellInt(raw, diffIdx),
			Weight:     cellFloat(raw, weightIdx),
		}
		if g := cellString(raw, groupsIdx); g != "" {
			var groups []string
			if err := json.Unmarshal([]byte(g), &groups); err != nil {
				groups = splitList(g)
			}
			rec.Groups = groups
		}
		if enabledIdx >= 0 {
			v := cellBool(raw, enabledIdx)
			rec.Enabled = &v
		}
		recs = append(recs, rec)
	}
	if err := rows.Err(); err != nil {
		return domain.Snapshot{}, fmt.Errorf("读取数据集失败: %w", err)
	}
	if len(recs) == 0 {
		return domain.Snapshot{}, fmt.Errorf("数据集查询没有返回任何行")
	}
	snap := buildSnapshot(p.ID(), recs, "default")
	return snap, nil
}

func (p *sqlProvider) Close() error {
	if p.db != nil {
		return p.db.Close()
	}
	return nil
}

func cellString(raw []any, idx int) string {
	if idx < 0 || idx >= len(raw) {
		return ""
	}
	switch v := raw[idx].(type) {
	case nil:
		return ""
	case []byte:
		return strings.TrimSpace(string(v))
	case string:
		return strings.TrimSpace(v)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func cellInt(raw []any, idx int) int {
	s := cellString(raw, idx)
	if s == "" {
		return 0
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return v
}

func cellFloat(raw []any, idx int) float64 {
	s := cellString(raw, idx)
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

func cellBool(raw []any, idx int) bool {
	if idx < 0 || idx >= len(raw) {
		return false
	}
	switch v := raw[idx].(type) {
	case bool:
		return v
	case int64:
		return v != 0
	case []byte:
		return parseBool(string(v))
	case string:
		return parseBool(v)
	default:
		return parseBool(fmt.Sprint(v))
	}
}

func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "t", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func splitList(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '|' || r == ';' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
