// Package dispatch 实现按 key 串行、按 lane 限并发的任务分派器。
//
// 关键取舍（对应设计文档 3.6）：
//   - critical 通道采用有界背压而不是"拒绝"：入群、申请、回调、待验证发言这类更新
//     丢了就无法补偿，因此容量满时 Submit 阻塞等待，宁可拖慢 long poll 也不丢；
//   - command/notice 满载时快速失败，返回"繁忙"提示，避免提示本身放大拥塞；
//   - 同一个 DispatchKey 严格 FIFO（用 key 队列而不是 key 锁，避免锁调度导致乱序）。
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
)

// ErrQueueFull 表示非关键通道已满。
var ErrQueueFull = errors.New("队列已满")

// ErrStopped 表示分派器已停止接收新任务。
var ErrStopped = errors.New("分派器已停止接收任务")

// Reporter 用于记录 panic 与背压告警。
type Reporter interface {
	Warn(msg string, fields map[string]any)
}

type job struct {
	ctx  context.Context
	name string
	task ports.Task
	at   time.Time
}

type keyQueue struct {
	mu      sync.Mutex
	jobs    []*job
	running bool
	queued  int
}

type lane struct {
	name     string
	ch       chan *keyQueue
	keys     map[ports.DispatchKey]*keyQueue
	keysMu   sync.Mutex
	workers  int
	totalCap int
	queued   atomic.Int64
	perKey   int
}

// Dispatcher 是 ports.Dispatcher 的默认实现。
type Dispatcher struct {
	lanes    map[ports.Lane]*lane
	stop     atomic.Bool
	wg       sync.WaitGroup
	reporter Reporter
	warnAt   int

	critMaxWait atomic.Int64 // 纳秒
}

// New 构造分派器并启动 worker。
func New(cfg config.Dispatcher, reporter Reporter) *Dispatcher {
	d := &Dispatcher{
		lanes:    map[ports.Lane]*lane{},
		reporter: reporter,
		warnAt:   cfg.CriticalWarnAt,
	}
	d.lanes[ports.LaneCritical] = newLane("critical", cfg.CriticalCapacity, cfg.CriticalWorkers, 0)
	d.lanes[ports.LaneCommand] = newLane("command", cfg.CommandTotalCapacity, cfg.CommandWorkers, cfg.CommandPerKeyCapacity)
	d.lanes[ports.LaneNotice] = newLane("notice", cfg.NoticeTotalCapacity, cfg.NoticeWorkers, cfg.NoticeTotalCapacity)
	for _, l := range d.lanes {
		for i := 0; i < l.workers; i++ {
			d.wg.Add(1)
			go d.work(l)
		}
	}
	return d
}

func newLane(name string, totalCap, workers, perKey int) *lane {
	if workers <= 0 {
		workers = 1
	}
	if totalCap <= 0 {
		totalCap = 64
	}
	return &lane{
		name:     name,
		ch:       make(chan *keyQueue, workers),
		keys:     map[ports.DispatchKey]*keyQueue{},
		workers:  workers,
		totalCap: totalCap,
		perKey:   perKey,
	}
}

// Submit 提交任务。critical 会背压，其它 lane 满载返回 ErrQueueFull。
func (d *Dispatcher) Submit(ctx context.Context, ln ports.Lane, key ports.DispatchKey, task ports.Task) error {
	if d.stop.Load() {
		return ErrStopped
	}
	if task == nil {
		return nil
	}
	l, ok := d.lanes[ln]
	if !ok {
		return fmt.Errorf("未知的 lane %q", ln)
	}
	j := &job{ctx: ctx, name: string(ln), task: task, at: time.Now()}

	if ln == ports.LaneCritical {
		// 关键通道：有界背压。通道满时在 Submit 里等待，绝不丢事件。
		start := time.Now()
		if err := d.enqueue(l, key, j, true); err != nil {
			return err
		}
		if wait := time.Since(start); wait > 20*time.Millisecond {
			d.recordWait(wait)
		}
		return nil
	}

	// 非关键通道：先做容量判断，避免无界内存增长。
	if l.queued.Load() >= int64(l.totalCap) {
		return ErrQueueFull
	}
	kq := d.keyState(l, key)
	kq.mu.Lock()
	perKeyFull := l.perKey > 0 && kq.queued >= l.perKey
	kq.mu.Unlock()
	if perKeyFull {
		return ErrQueueFull
	}
	if l.queued.Add(1) > int64(l.totalCap) {
		l.queued.Add(-1)
		return ErrQueueFull
	}
	l.queued.Add(-1)
	return d.enqueue(l, key, j, false)
}

func (d *Dispatcher) enqueue(l *lane, key ports.DispatchKey, j *job, block bool) error {
	kq := d.keyState(l, key)
	kq.mu.Lock()
	first := !kq.running
	kq.jobs = append(kq.jobs, j)
	kq.queued++
	kq.running = true
	kq.mu.Unlock()

	l.queued.Add(1)
	if !first {
		return nil
	}
	// 首任务负责把整个 key 队列交给 worker。
	if block {
		l.ch <- kq
		return nil
	}
	// 非关键通道：投递不成功就回滚并快速失败，绝不在 update 循环里阻塞。
	select {
	case l.ch <- kq:
		return nil
	default:
		kq.mu.Lock()
		if n := len(kq.jobs); n > 0 {
			kq.jobs = kq.jobs[:n-1]
			kq.queued--
		}
		if len(kq.jobs) == 0 {
			kq.running = false
		}
		kq.mu.Unlock()
		l.queued.Add(-1)
		return ErrQueueFull
	}
}

func (d *Dispatcher) keyState(l *lane, key ports.DispatchKey) *keyQueue {
	l.keysMu.Lock()
	defer l.keysMu.Unlock()
	kq, ok := l.keys[key]
	if !ok {
		kq = &keyQueue{}
		l.keys[key] = kq
	}
	return kq
}

func (d *Dispatcher) work(l *lane) {
	defer d.wg.Done()
	for kq := range l.ch {
		d.drainKeyQueue(l, kq)
	}
}

func (d *Dispatcher) drainKeyQueue(l *lane, kq *keyQueue) {
	for {
		kq.mu.Lock()
		if len(kq.jobs) == 0 {
			kq.running = false
			kq.queued = 0
			kq.mu.Unlock()
			return
		}
		j := kq.jobs[0]
		kq.jobs = kq.jobs[1:]
		kq.queued--
		kq.mu.Unlock()

		d.run(j)
		l.queued.Add(-1)
	}
}

// run 执行任务，并在四层边界之一做 panic 兜底：panic 不能杀死 worker，
// 也不能把会话当成成功（这里只记录，状态仍由注册表决定）。
func (d *Dispatcher) run(j *job) {
	defer func() {
		if r := recover(); r != nil {
			d.report("dispatcher 任务 panic", map[string]any{
				"task":  j.name,
				"panic": fmt.Sprint(r),
				"stack": string(debug.Stack()),
			})
		}
	}()
	ctx := j.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if err := j.task(ctx); err != nil {
		d.report("dispatcher 任务返回错误", map[string]any{"task": j.name, "error": err.Error()})
	}
}

func (d *Dispatcher) report(msg string, fields map[string]any) {
	if d.reporter != nil {
		d.reporter.Warn(msg, fields)
	}
}

func (d *Dispatcher) recordWait(w time.Duration) {
	for {
		cur := d.critMaxWait.Load()
		if int64(w) <= cur {
			return
		}
		if d.critMaxWait.CompareAndSwap(cur, int64(w)) {
			if d.warnAt > 0 {
				d.report("critical 通道出现背压", map[string]any{"wait": w.String()})
			}
			return
		}
	}
}

// StopAdmission 停止接收新任务（优雅退出的第一步）。
func (d *Dispatcher) StopAdmission() { d.stop.Store(true) }

// Drain 等待在途任务结束，然后关闭各 lane 的 worker。
func (d *Dispatcher) Drain(ctx context.Context) error {
	deadline := time.Now().Add(30 * time.Second)
	if dl, ok := ctx.Deadline(); ok {
		deadline = dl
	}
	for {
		if d.allIdle() {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待分派器排空超时")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	for _, l := range d.lanes {
		close(l.ch)
	}
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("等待 worker 退出超时")
	}
}

func (d *Dispatcher) allIdle() bool {
	for _, l := range d.lanes {
		if l.queued.Load() > 0 {
			return false
		}
	}
	return true
}

// Stats 返回运行指标。
func (d *Dispatcher) Stats() ports.DispatcherStats {
	st := ports.DispatcherStats{}
	if crit := d.lanes[ports.LaneCritical]; crit != nil {
		st.CriticalQueued = int(crit.queued.Load())
	}
	if l := d.lanes[ports.LaneCommand]; l != nil {
		st.CommandQueued = int(l.queued.Load())
	}
	if l := d.lanes[ports.LaneNotice]; l != nil {
		st.NoticeQueued = int(l.queued.Load())
	}
	st.CriticalMaxWait = time.Duration(d.critMaxWait.Load())
	return st
}
