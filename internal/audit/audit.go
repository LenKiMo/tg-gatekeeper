// Package audit 写结构化审计日志（JSONL）。
//
// 审计记录里绝不出现 bio 原文、正确答案、图片 URL、bot token 或 DSN。安全终态
// （放行/封禁/拦截）即使文件写失败也必须落到 stdout/stderr，不能被静默丢弃。
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
)

// Logger 是 ports.Audit 的默认实现。
type Logger struct {
	cfg      config.Audit
	file     *os.File
	path     string
	mu       sync.Mutex
	size     int64
	maxBytes int64
	keep     int
	disabled bool
}

// New 打开审计输出。file 为空表示只写 stdout。
func New(cfg config.Audit) (*Logger, error) {
	l := &Logger{
		cfg:      cfg,
		path:     cfg.File,
		maxBytes: int64(cfg.RotateMaxMegabytes) << 20,
		keep:     cfg.RotateKeepFiles,
		disabled: !cfg.Enabled,
	}
	if l.disabled || cfg.File == "" {
		return l, nil
	}
	if err := os.MkdirAll(filepath.Dir(cfg.File), 0o750); err != nil {
		return nil, fmt.Errorf("创建审计日志目录失败: %w", err)
	}
	mode := os.FileMode(0o640)
	if cfg.FileMode != "" {
		var v uint32
		if _, err := fmt.Sscanf(cfg.FileMode, "%o", &v); err == nil {
			mode = os.FileMode(v)
		}
	}
	f, err := os.OpenFile(cfg.File, os.O_APPEND|os.O_CREATE|os.O_WRONLY, mode)
	if err != nil {
		return nil, fmt.Errorf("打开审计日志失败: %w", err)
	}
	st, _ := f.Stat()
	if st != nil {
		l.size = st.Size()
	}
	l.file = f
	return l, nil
}

// Log 实现 ports.Audit。
func (l *Logger) Log(_ context.Context, ev ports.AuditEvent) {
	if l.disabled {
		return
	}
	rec := map[string]any{
		"time":     time.Now().Format(time.RFC3339Nano),
		"event":    ev.Event,
		"chat_id":  ev.ChatID,
		"user_id":  ev.UserID,
		"actor_id": ev.ActorID,
		"result":   ev.Result,
	}
	setIfNotEmpty(rec, "session_id", ev.SessionID)
	setIfNotEmpty(rec, "mode", ev.Mode)
	setIfNotEmpty(rec, "old_state", ev.OldState)
	setIfNotEmpty(rec, "new_state", ev.NewState)
	setIfNotEmpty(rec, "dataset_revision", ev.DatasetRevision)
	setIfNotEmpty(rec, "effect", ev.Effect)
	setIfNotEmpty(rec, "error_class", ev.ErrorClass)
	if ev.Attempt > 0 {
		rec["attempt"] = ev.Attempt
	}
	if ev.DisplayName != "" && l.cfg.IncludeDisplayName {
		rec["display_name"] = ev.DisplayName
	}
	for k, v := range ev.Extra {
		if k == "entry_id" && l.cfg.HashEntityID {
			rec[k] = hashValue(v)
			continue
		}
		rec[k] = v
	}

	line, err := json.Marshal(rec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[audit] 序列化失败: %v\n", err)
		return
	}
	line = append(line, '\n')

	if l.cfg.Stdout {
		_, _ = os.Stdout.Write(line)
	}
	l.writeFile(line)
}

func (l *Logger) writeFile(line []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return
	}
	n, err := l.file.Write(line)
	if err != nil {
		// 审计写失败不能阻塞门禁，但必须让运维看见。
		fmt.Fprintf(os.Stderr, "[audit] 写入 %s 失败: %v\n", l.path, err)
		return
	}
	l.size += int64(n)
	if l.maxBytes > 0 && l.size >= l.maxBytes {
		l.rotateLocked()
	}
}

func (l *Logger) rotateLocked() {
	if l.file == nil {
		return
	}
	_ = l.file.Close()
	stamp := time.Now().Format("20060102T150405")
	rotated := fmt.Sprintf("%s.%s", l.path, stamp)
	if err := os.Rename(l.path, rotated); err != nil {
		fmt.Fprintf(os.Stderr, "[audit] 轮转失败: %v\n", err)
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[audit] 重新打开日志失败: %v\n", err)
		l.file = nil
		return
	}
	l.file = f
	l.size = 0
	l.pruneLocked()
}

// pruneLocked 只保留最近的 N 份轮转文件。
func (l *Logger) pruneLocked() {
	if l.keep <= 0 {
		return
	}
	dir := filepath.Dir(l.path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := filepath.Base(l.path) + "."
	var rotated []string
	for _, e := range entries {
		if e.IsDir() || len(e.Name()) <= len(prefix) || e.Name()[:len(prefix)] != prefix {
			continue
		}
		rotated = append(rotated, filepath.Join(dir, e.Name()))
	}
	if len(rotated) <= l.keep {
		return
	}
	// 文件名带时间戳，字典序即时间序。
	sortStrings(rotated)
	for _, p := range rotated[:len(rotated)-l.keep] {
		_ = os.Remove(p)
	}
}

// Close 关闭文件。
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		err := l.file.Close()
		l.file = nil
		return err
	}
	return nil
}

func setIfNotEmpty(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}

func hashValue(v string) string {
	sum := sha256.Sum256([]byte(v))
	return "h:" + hex.EncodeToString(sum[:])[:12]
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
