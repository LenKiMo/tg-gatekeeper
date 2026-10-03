package store

import (
	"context"
	"fmt"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
)

// MemGroupStore 是内存版群配置存储（仅用于测试）。
type MemGroupStore struct {
	data map[int64]domain.GroupConfig
}

// NewMemGroupStore 构造内存群配置存储。
func NewMemGroupStore() *MemGroupStore {
	return &MemGroupStore{data: map[int64]domain.GroupConfig{}}
}

// EnsureGroup 实现 ports.Store。
func (m *MemGroupStore) EnsureGroup(_ context.Context, chatID int64, defaults domain.GroupConfig) (domain.GroupConfig, error) {
	if cfg, ok := m.data[chatID]; ok {
		return cfg, nil
	}
	defaults.ChatID = chatID
	defaults.Revision = 1
	defaults.UpdatedAt = time.Now()
	m.data[chatID] = defaults
	return defaults, nil
}

// Probe 实现 ports.Store：内存实现必然可读写，仅做一次写入+删除以保持语义一致。
func (m *MemGroupStore) Probe(_ context.Context) error {
	const probe int64 = -987654321
	if _, ok := m.data[probe]; !ok {
		m.data[probe] = domain.GroupConfig{ChatID: probe}
	}
	delete(m.data, probe)
	return nil
}

// GetGroup 实现 ports.Store。
func (m *MemGroupStore) GetGroup(_ context.Context, chatID int64) (domain.GroupConfig, error) {
	cfg, ok := m.data[chatID]
	if !ok {
		return domain.GroupConfig{}, ports.ErrNotFound
	}
	return cfg, nil
}

// SaveGroup 实现 ports.Store（CAS）。
func (m *MemGroupStore) SaveGroup(_ context.Context, cfg domain.GroupConfig, expectedRevision uint64) (domain.GroupConfig, error) {
	cur, ok := m.data[cfg.ChatID]
	if expectedRevision == 0 {
		if ok {
			return domain.GroupConfig{}, ports.ErrConflict
		}
		cfg.Revision = 1
		cfg.UpdatedAt = time.Now()
		m.data[cfg.ChatID] = cfg
		return cfg, nil
	}
	if !ok || cur.Revision != expectedRevision {
		return domain.GroupConfig{}, ports.ErrConflict
	}
	cfg.Revision = expectedRevision + 1
	cfg.UpdatedAt = time.Now()
	m.data[cfg.ChatID] = cfg
	return cfg, nil
}

// Close 实现 ports.Store。
func (m *MemGroupStore) Close() error { return nil }

// MemRegistry 是内存版会话注册表（仅用于测试）。
type MemRegistry struct {
	sessions map[domain.SessionKey]domain.Session
	last     map[domain.SessionKey]domain.Session
	effects  map[string]domain.Effect
	order    []string
}

// NewMemRegistry 构造内存注册表。
func NewMemRegistry() *MemRegistry {
	return &MemRegistry{
		sessions: map[domain.SessionKey]domain.Session{},
		last:     map[domain.SessionKey]domain.Session{},
		effects:  map[string]domain.Effect{},
	}
}

// Begin 实现 ports.SessionRegistry。
func (m *MemRegistry) Begin(_ context.Context, s domain.Session) (ports.BeginResult, error) {
	if cur, ok := m.sessions[s.Key]; ok {
		if cur.UpdateID == s.UpdateID && cur.UpdateID != 0 {
			return ports.BeginResult{Session: cur, Duplicate: true}, nil
		}
		old := cur
		old.State = domain.StateCancelled
		m.last[s.Key] = old
		for id, e := range m.effects {
			if e.SessionID == cur.ID {
				delete(m.effects, id)
			}
		}
		res := ports.BeginResult{Session: s, Replaced: &old}
		m.sessions[s.Key] = s
		m.last[s.Key] = s
		return res, nil
	}
	if s.Version == 0 {
		s.Version = 1
	}
	m.sessions[s.Key] = s
	m.last[s.Key] = s
	return ports.BeginResult{Session: s}, nil
}

// AttachMessages 实现 ports.SessionRegistry。
func (m *MemRegistry) AttachMessages(_ context.Context, key domain.SessionKey, sessionID string,
	challenge, admin domain.MessageRef, expiresAt time.Time, expectedVersion uint64) (domain.Session, error) {
	s, ok := m.sessions[key]
	if !ok || s.ID != sessionID {
		return domain.Session{}, ports.ErrNotFound
	}
	if expectedVersion != 0 && s.Version != expectedVersion {
		return domain.Session{}, ports.ErrConflict
	}
	s.ChallengeMessage = challenge
	s.AdminMessage = admin
	s.ExpiresAt = expiresAt
	s.State = domain.StatePending
	s.Version++
	m.sessions[key] = s
	m.last[key] = s
	return s, nil
}

// Get 实现 ports.SessionRegistry。
func (m *MemRegistry) Get(_ context.Context, key domain.SessionKey) (domain.Session, error) {
	s, ok := m.sessions[key]
	if !ok {
		return domain.Session{}, ports.ErrNotFound
	}
	return s, nil
}

// GetByID 实现 ports.SessionRegistry。
func (m *MemRegistry) GetByID(_ context.Context, sessionID string) (domain.Session, error) {
	if s, ok := m.sessions[byIDLookup(m, sessionID)]; ok {
		return s, nil
	}
	for _, s := range m.last {
		if s.ID == sessionID {
			return s, nil
		}
	}
	return domain.Session{}, ports.ErrNotFound
}

// byIDLookup 找出 sessionID 对应的 key。
func byIDLookup(m *MemRegistry, sessionID string) domain.SessionKey {
	for key, s := range m.sessions {
		if s.ID == sessionID {
			return key
		}
	}
	return domain.SessionKey{}
}

// Claim 实现 ports.SessionRegistry。
func (m *MemRegistry) Claim(_ context.Context, key domain.SessionKey, event domain.Event, plan ports.EffectPlanner) (ports.ClaimResult, error) {
	s, ok := m.sessions[key]
	if !ok {
		if last, ok := m.last[key]; ok {
			if event.SessionID != "" && last.ID != event.SessionID {
				return ports.ClaimResult{Reason: ports.ClaimReplaced, Before: last, After: last}, nil
			}
			return ports.ClaimResult{Reason: ports.ClaimDone, Before: last, After: last}, nil
		}
		return ports.ClaimResult{Reason: ports.ClaimUnknown}, nil
	}
	if event.SessionID != "" && event.SessionID != s.ID {
		return ports.ClaimResult{Reason: ports.ClaimReplaced, Before: s, After: s}, nil
	}
	target, reason := decide(s, event)
	if reason != "" {
		return ports.ClaimResult{Reason: reason, Before: s, After: s}, nil
	}
	before := s
	after := s
	after.State = target
	after.Version++
	var effects []domain.Effect
	if plan != nil {
		effects = plan(before, after, event)
	}
	now := time.Now()
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
			effects[i].DueAt = now
		}
		m.effects[effects[i].ID] = effects[i]
		m.order = append(m.order, effects[i].ID)
	}
	delete(m.sessions, key)
	m.last[key] = after
	return ports.ClaimResult{Won: true, Before: before, After: after, Effects: effects}, nil
}

// ListActive 实现 ports.SessionRegistry。
func (m *MemRegistry) ListActive(_ context.Context, now time.Time, expiredOnly bool, limit int) ([]domain.Session, error) {
	var out []domain.Session
	for _, s := range m.sessions {
		if expiredOnly && (s.ExpiresAt.IsZero() || s.ExpiresAt.After(now)) {
			continue
		}
		out = append(out, s)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// ListDueEffects 实现 ports.SessionRegistry。
func (m *MemRegistry) ListDueEffects(_ context.Context, now time.Time, limit int) ([]domain.Effect, error) {
	var out []domain.Effect
	for _, id := range m.order {
		e, ok := m.effects[id]
		if !ok || e.DueAt.After(now) {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// AckEffect 实现 ports.SessionRegistry。
func (m *MemRegistry) AckEffect(_ context.Context, effectID string) error {
	delete(m.effects, effectID)
	return nil
}

// RetryEffect 实现 ports.SessionRegistry。
func (m *MemRegistry) RetryEffect(_ context.Context, effectID string, next time.Time, cause string) error {
	e, ok := m.effects[effectID]
	if !ok {
		return nil
	}
	e.Attempts++
	e.DueAt = next
	e.LastError = cause
	m.effects[effectID] = e
	return nil
}

// CancelEffectsOf 实现 ports.SessionRegistry。
func (m *MemRegistry) CancelEffectsOf(_ context.Context, sessionID string, kinds ...domain.EffectKind) (int, error) {
	n := 0
	for id, e := range m.effects {
		if e.SessionID != sessionID {
			continue
		}
		if len(kinds) > 0 {
			match := false
			for _, k := range kinds {
				if e.Kind == k {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		delete(m.effects, id)
		n++
	}
	return n, nil
}

// Close 实现 ports.SessionRegistry。
func (m *MemRegistry) Close() error { return nil }

// MemDeletionQueue 是内存版延时删除队列（仅用于测试）。
type MemDeletionQueue struct {
	jobs map[string]ports.DeleteJob
}

// NewMemDeletionQueue 构造内存删除队列。
func NewMemDeletionQueue() *MemDeletionQueue {
	return &MemDeletionQueue{jobs: map[string]ports.DeleteJob{}}
}

// Enqueue 实现 ports.DeletionQueue。
func (q *MemDeletionQueue) Enqueue(_ context.Context, msg domain.MessageRef, dueAt time.Time) error {
	if !msg.Valid() {
		return fmt.Errorf("非法的消息引用 %+v", msg)
	}
	id := fmt.Sprintf("%d:%d", msg.ChatID, msg.MessageID)
	if cur, ok := q.jobs[id]; ok && cur.DueAt.Before(dueAt) {
		return nil
	}
	q.jobs[id] = ports.DeleteJob{ID: id, Message: msg, DueAt: dueAt}
	return nil
}

// LeaseDue 实现 ports.DeletionQueue。
func (q *MemDeletionQueue) LeaseDue(_ context.Context, now time.Time, limit int, lease time.Duration) ([]ports.DeleteJob, error) {
	var out []ports.DeleteJob
	for id, j := range q.jobs {
		if j.DueAt.After(now) || j.LeaseUntil.After(now) {
			continue
		}
		j.LeaseUntil = now.Add(lease)
		q.jobs[id] = j
		out = append(out, j)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Ack 实现 ports.DeletionQueue。
func (q *MemDeletionQueue) Ack(_ context.Context, jobID string) error {
	delete(q.jobs, jobID)
	return nil
}

// Nack 实现 ports.DeletionQueue。
func (q *MemDeletionQueue) Nack(_ context.Context, jobID string, retryAt time.Time, cause string) error {
	j, ok := q.jobs[jobID]
	if !ok {
		return nil
	}
	j.Attempts++
	j.DueAt = retryAt
	j.LeaseUntil = time.Time{}
	j.LastError = cause
	q.jobs[jobID] = j
	return nil
}

// Close 实现 ports.DeletionQueue。
func (q *MemDeletionQueue) Close() error { return nil }
