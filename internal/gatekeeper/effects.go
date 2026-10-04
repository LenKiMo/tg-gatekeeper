package gatekeeper

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
)

// effectLoop 是副作用 outbox 执行器 + 超时扫描器。
//
// 之所以把两者放在一个循环里：它们共享同一份"会话到点了吗"的判断，且都必须在
// 进程重启后自己恢复（启动时扫一遍 ListActive(expiredOnly=true) 就能补上被错过的超时）。
func (s *Service) effectLoop() {
	defer s.wg.Done()
	interval := 500 * time.Millisecond
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			s.scanExpired(ctx)
			s.runDueEffects(ctx)
			cancel()
		}
	}
}

// scanExpired 把已过期的会话推进到 timeout 终态。
func (s *Service) scanExpired(ctx context.Context) {
	limit := s.cfg.Session.RecoveryBatchSize
	if limit <= 0 {
		limit = 100
	}
	sessions, err := s.registry.ListActive(ctx, time.Now(), true, limit)
	if err != nil {
		s.log.Warn("扫描过期会话失败", "err", err)
		return
	}
	for _, sess := range sessions {
		group, err := s.Group(ctx, sess.Key.ChatID)
		if err != nil {
			s.log.Warn("读取群配置失败", "chat_id", sess.Key.ChatID, "err", err)
			continue
		}
		res, err := s.registry.Claim(ctx, sess.Key, domain.Event{
			Kind: domain.EventTimeout, SessionID: sess.ID, At: time.Now(),
		}, s.effectPlan(group, ""))
		if err := s.finishClaim(ctx, group, res, err, "verify.timeout"); err != nil {
			s.log.Warn("处理超时失败", "session_id", sess.ID, "err", err)
		}
	}
}

// runDueEffects 执行到期的副作用。
func (s *Service) runDueEffects(ctx context.Context) {
	effects, err := s.registry.ListDueEffects(ctx, time.Now(), 50)
	if err != nil {
		s.log.Warn("读取待执行副作用失败", "err", err)
		return
	}
	for _, ef := range effects {
		s.executeEffect(ctx, ef)
	}
}

func (s *Service) executeEffect(ctx context.Context, ef domain.Effect) {
	err := s.doEffect(ctx, ef)
	if err == nil {
		if ackErr := s.registry.AckEffect(ctx, ef.ID); ackErr != nil {
			s.log.Warn("确认副作用失败", "effect_id", ef.ID, "err", ackErr)
		}
		s.audit(ctx, ports.AuditEvent{
			Event: "effect.done", Effect: string(ef.Kind), SessionID: ef.SessionID,
			Attempt: ef.Attempts + 1, Result: "ok",
		})
		return
	}

	permanent := isPermanentEffectError(err)
	maxAttempts := s.cfg.Session.EffectMaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 20
	}
	if permanent || ef.Attempts+1 >= maxAttempts {
		// 永久失败或超过上限：不再重试，但要留下痕迹（dead letter）。
		_ = s.registry.AckEffect(ctx, ef.ID)
		s.audit(ctx, ports.AuditEvent{
			Event: "effect.dead_letter", Effect: string(ef.Kind), SessionID: ef.SessionID,
			Attempt: ef.Attempts + 1, Result: "dropped", ErrorClass: classify(err),
			Extra: map[string]string{"error": truncate(err.Error(), 200)},
		})
		s.log.Error("副作用放弃重试", "effect_id", ef.ID, "kind", ef.Kind, "err", err)
		return
	}

	next := time.Now().Add(s.backoff(ef.Attempts + 1))
	if rerr := s.registry.RetryEffect(ctx, ef.ID, next, truncate(err.Error(), 200)); rerr != nil {
		s.log.Warn("安排副作用重试失败", "effect_id", ef.ID, "err", rerr)
	}
	s.audit(ctx, ports.AuditEvent{
		Event: "effect.retry", Effect: string(ef.Kind), SessionID: ef.SessionID,
		Attempt: ef.Attempts + 1, Result: "retry", ErrorClass: classify(err),
		Extra: map[string]string{"next": next.Format(time.RFC3339)},
	})
}

func (s *Service) backoff(attempt int) time.Duration {
	base := time.Duration(s.cfg.Session.EffectRetryInitialSeconds) * time.Second
	if base <= 0 {
		base = 2 * time.Second
	}
	max := time.Duration(s.cfg.Session.EffectRetryMaxSeconds) * time.Second
	if max <= 0 {
		max = 5 * time.Minute
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	if d > max {
		d = max
	}
	return d
}

func (s *Service) doEffect(ctx context.Context, ef domain.Effect) error {
	p := ef.Payload
	chatID := ParseInt(p["chat_id"])
	userID := ParseInt(p["user_id"])

	switch ef.Kind {
	case domain.EffectRestorePermissions:
		return s.api.RestorePermissions(ctx, chatID, userID)
	case domain.EffectBan:
		return s.api.Ban(ctx, chatID, userID, p["revoke"] == "1", ParseInt(p["until"]))
	case domain.EffectUnban:
		return s.api.Unban(ctx, chatID, userID, true)
	case domain.EffectApprove:
		return s.api.ApproveJoinRequest(ctx, chatID, userID)
	case domain.EffectDecline:
		return s.api.DeclineJoinRequest(ctx, chatID, userID)
	case domain.EffectSetTag:
		if p["tag"] == "" {
			return nil
		}
		return s.api.SetMemberTag(ctx, chatID, userID, p["tag"])
	case domain.EffectWelcome:
		ref, err := s.api.SendText(ctx, chatID, p["text"], true, nil)
		if err != nil {
			return err
		}
		return s.enqueueDeletion(ctx, ref, ParseInt(p["delete_after"]))
	case domain.EffectDeleteMessage:
		msgID := ParseInt(p["message_id"])
		if msgID == 0 {
			return nil
		}
		return s.enqueueDeletion(ctx, domain.MessageRef{ChatID: chatID, MessageID: msgID}, ParseInt(p["delay"]))
	case domain.EffectNotifyAdmin:
		_, err := s.api.SendText(ctx, chatID, p["text"], true, nil)
		return err
	default:
		return fmt.Errorf("未知的副作用类型 %q", ef.Kind)
	}
}

// enqueueDeletion 把删除动作交给持久队列：删除失败要重试，且不能在重启后丢失。
func (s *Service) enqueueDeletion(ctx context.Context, ref domain.MessageRef, delaySeconds int64) error {
	if !ref.Valid() || s.deletion == nil {
		return nil
	}
	if delaySeconds < 0 {
		delaySeconds = 0
	}
	return s.deletion.Enqueue(ctx, ref, time.Now().Add(time.Duration(delaySeconds)*time.Second))
}

// deletionLoop 是延时删除 worker。
func (s *Service) deletionLoop() {
	defer s.wg.Done()
	interval := time.Duration(s.cfg.DeletionWorker.PollIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 2 * time.Second
	}
	lease := time.Duration(s.cfg.DeletionWorker.LeaseSeconds) * time.Second
	if lease <= 0 {
		lease = 60 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			s.processDeletions(ctx, lease)
			cancel()
		}
	}
}

func (s *Service) processDeletions(ctx context.Context, lease time.Duration) {
	if s.deletion == nil {
		return
	}
	batch := s.cfg.DeletionWorker.BatchSize
	if batch <= 0 {
		batch = 50
	}
	jobs, err := s.deletion.LeaseDue(ctx, time.Now(), batch, lease)
	if err != nil {
		s.log.Warn("领取删除任务失败", "err", err)
		return
	}
	abandonAfter := time.Duration(s.cfg.DeletionWorker.AbandonAfterSeconds) * time.Second
	for _, job := range jobs {
		err := s.api.DeleteMessage(ctx, job.Message)
		switch {
		case err == nil:
			if ackErr := s.deletion.Ack(ctx, job.ID); ackErr != nil {
				s.log.Warn("确认删除任务失败", "job_id", job.ID, "err", ackErr)
			}
		case isPermanentDeleteError(err), abandonAfter > 0 && time.Since(job.DueAt) > abandonAfter:
			// Telegram 明确表示消息已不存在/不能再删，或已超过放弃阈值：按完成处理，
			// 避免一个永远失败的任务被无限重试。
			_ = s.deletion.Ack(ctx, job.ID)
			s.audit(ctx, ports.AuditEvent{
				Event: "delete.abandoned", ChatID: job.Message.ChatID, Result: "dropped",
				ErrorClass: classify(err), Extra: map[string]string{"message_id": itoa(int64(job.Message.MessageID))},
			})
		default:
			next := time.Now().Add(s.deletionBackoff(job.Attempts + 1))
			if nerr := s.deletion.Nack(ctx, job.ID, next, truncate(err.Error(), 200)); nerr != nil {
				s.log.Warn("安排删除重试失败", "job_id", job.ID, "err", nerr)
			}
		}
	}
}

func (s *Service) deletionBackoff(attempt int) time.Duration {
	base := time.Duration(s.cfg.DeletionWorker.RetryInitialSeconds) * time.Second
	if base <= 0 {
		base = 5 * time.Second
	}
	max := time.Duration(s.cfg.DeletionWorker.RetryMaxSeconds) * time.Second
	if max <= 0 {
		max = 5 * time.Minute
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	if d > max {
		d = max
	}
	return d
}

// isPermanentEffectError 判断"重试也不会成功"的错误。
func isPermanentEffectError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "not enough rights"),
		strings.Contains(msg, "user not found"),
		strings.Contains(msg, "chat not found"),
		strings.Contains(msg, "bot was blocked"),
		strings.Contains(msg, "peer_id_invalid"),
		strings.Contains(msg, "user_is_bot"),
		strings.Contains(msg, "participant_missing"),
		strings.Contains(msg, "hide_requester_missing"),
		strings.Contains(msg, "400"):
		return true
	}
	return false
}

func isPermanentDeleteError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "message to delete not found"),
		strings.Contains(msg, "message can't be deleted"),
		strings.Contains(msg, "message_id_invalid"),
		strings.Contains(msg, "not enough rights to delete"),
		strings.Contains(msg, "chat not found"),
		strings.Contains(msg, "user not found"),
		strings.Contains(msg, "message is not modified"):
		return true
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ErrNoop 表示无需处理（供上层判断）。
var ErrNoop = errors.New("noop")
