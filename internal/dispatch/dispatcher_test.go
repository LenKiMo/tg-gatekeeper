package dispatch

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
)

func testDispatcher() *Dispatcher {
	cfg := config.Default().Dispatcher
	cfg.CommandTotalCapacity = 4
	cfg.CommandPerKeyCapacity = 2
	cfg.CriticalCapacity = 8
	cfg.CriticalWorkers = 4
	cfg.CommandWorkers = 2
	return New(cfg, nil)
}

// TestSameKeyIsFIFO 验证同一 key 的任务严格按提交顺序执行。
func TestSameKeyIsFIFO(t *testing.T) {
	d := testDispatcher()
	defer func() { _ = d.Drain(context.Background()) }()

	key := ports.DispatchKey{ChatID: -100, UserID: 7}
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	wg.Add(20)
	for i := 0; i < 20; i++ {
		n := i
		if err := d.Submit(context.Background(), ports.LaneCritical, key, func(context.Context) error {
			defer wg.Done()
			time.Sleep(time.Millisecond)
			mu.Lock()
			order = append(order, n)
			mu.Unlock()
			return nil
		}); err != nil {
			t.Fatalf("提交失败: %v", err)
		}
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	for i, v := range order {
		if i != v {
			t.Fatalf("同 key 顺序被打乱：位置 %d 得到 %d（顺序 %v）", i, v, order)
		}
	}
}

// TestDifferentKeysRunConcurrently 验证不同 key 之间可以并行（否则会被一个慢用户堵住）。
func TestDifferentKeysRunConcurrently(t *testing.T) {
	d := testDispatcher()
	defer func() { _ = d.Drain(context.Background()) }()

	var running atomic.Int32
	var peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		key := ports.DispatchKey{ChatID: int64(-100 - i)}
		if err := d.Submit(context.Background(), ports.LaneCritical, key, func(context.Context) error {
			defer wg.Done()
			cur := running.Add(1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
			running.Add(-1)
			return nil
		}); err != nil {
			t.Fatalf("提交失败: %v", err)
		}
	}
	wg.Wait()
	if peak.Load() < 2 {
		t.Fatalf("不同 key 的任务没有并行执行（峰值并发 %d）", peak.Load())
	}
}

// TestCommandLaneRejectsWhenFull 验证命令通道满载时快速失败而不是无界排队。
func TestCommandLaneRejectsWhenFull(t *testing.T) {
	cfg := config.Default().Dispatcher
	cfg.CommandTotalCapacity = 2
	cfg.CommandWorkers = 1
	d := New(cfg, nil)
	defer func() { _ = d.Drain(context.Background()) }()

	block := make(chan struct{})
	defer close(block)
	// 占满 worker。
	if err := d.Submit(context.Background(), ports.LaneCommand, ports.DispatchKey{ChatID: -1}, func(context.Context) error {
		<-block
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)

	var full error
	for i := 0; i < 10; i++ {
		err := d.Submit(context.Background(), ports.LaneCommand, ports.DispatchKey{ChatID: int64(-100 - i)}, func(context.Context) error {
			return nil
		})
		if errors.Is(err, ErrQueueFull) {
			full = err
			break
		}
	}
	if full == nil {
		t.Fatalf("命令通道应当出现队列已满错误")
	}
}

// TestCriticalLaneBackpressureInsteadOfDrop 验证关键通道不丢事件（背压等待）。
func TestCriticalLaneBackpressureInsteadOfDrop(t *testing.T) {
	cfg := config.Default().Dispatcher
	cfg.CriticalCapacity = 2
	cfg.CriticalWorkers = 1
	d := New(cfg, nil)

	var done atomic.Int32
	release := make(chan struct{})
	key := ports.DispatchKey{ChatID: -100}
	if err := d.Submit(context.Background(), ports.LaneCritical, key, func(context.Context) error {
		<-release
		done.Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	submitted := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 关键通道应当在容量满时阻塞等待，而不是返回错误。
			err := d.Submit(context.Background(), ports.LaneCritical, key, func(context.Context) error {
				done.Add(1)
				return nil
			})
			submitted <- err
		}()
	}
	time.Sleep(30 * time.Millisecond)
	if done.Load() != 0 {
		t.Fatalf("被阻塞的首任务不应完成")
	}
	close(release)
	wg.Wait()
	close(submitted)
	for err := range submitted {
		if err != nil {
			t.Fatalf("关键通道不应返回错误: %v", err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for done.Load() < 9 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if done.Load() != 9 {
		t.Fatalf("关键任务完成数 = %d，期望 9（一个都不能丢）", done.Load())
	}
	_ = d.Drain(context.Background())
}

// TestPanicDoesNotKillWorker 验证任务 panic 不会拖垮整个分派器。
func TestPanicDoesNotKillWorker(t *testing.T) {
	d := testDispatcher()
	defer func() { _ = d.Drain(context.Background()) }()

	key := ports.DispatchKey{ChatID: -100, UserID: 1}
	if err := d.Submit(context.Background(), ports.LaneCritical, key, func(context.Context) error {
		panic("boom")
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	if err := d.Submit(context.Background(), ports.LaneCritical, key, func(context.Context) error {
		close(done)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("panic 之后的同 key 任务没有继续执行（worker 被拖垮）")
	}
}

// TestStopAdmissionAndDrain 验证优雅退出：停止接收后新任务被拒，在途任务被排空。
func TestStopAdmissionAndDrain(t *testing.T) {
	d := testDispatcher()
	var done atomic.Int32
	if err := d.Submit(context.Background(), ports.LaneCritical, ports.DispatchKey{ChatID: -1}, func(context.Context) error {
		time.Sleep(20 * time.Millisecond)
		done.Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	d.StopAdmission()
	if err := d.Submit(context.Background(), ports.LaneCritical, ports.DispatchKey{ChatID: -2}, func(context.Context) error { return nil }); !errors.Is(err, ErrStopped) {
		t.Fatalf("停止后提交应当返回 ErrStopped，实际 %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Drain(ctx); err != nil {
		t.Fatalf("排空失败: %v", err)
	}
	if done.Load() != 1 {
		t.Fatalf("在途任务没有执行完成")
	}
}

// TestStats 验证指标可读（运维排障用）。
func TestStats(t *testing.T) {
	d := testDispatcher()
	defer func() { _ = d.Drain(context.Background()) }()
	block := make(chan struct{})
	defer close(block)
	if err := d.Submit(context.Background(), ports.LaneCritical, ports.DispatchKey{ChatID: -1}, func(context.Context) error {
		<-block
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	st := d.Stats()
	if st.CriticalQueued < 1 {
		t.Fatalf("critical 排队数应当 >= 1，实际 %d", st.CriticalQueued)
	}
}
