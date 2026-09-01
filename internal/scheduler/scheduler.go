// Package scheduler serialises SDM commands within Google's published quotas.
//
// Google enforces 10 QPM for devices.executeCommand per project/user, and 100 QPH per
// camera or doorbell. Seven cameras reconnecting simultaneously would otherwise issue
// fourteen commands against the per-minute ceiling and cascade into repeated 429s, so
// every command in the process passes through one instance of this type.
//
// The per-trait limit of 5 QPM per project/user/device is not independently enforced
// here because commands do not carry a trait key. This limit can be exceeded when few
// devices are active: for example, one camera's connect-failure loop can consume the
// global allowance against that single device faster than the trait ceiling permits.
package scheduler

import (
	"container/heap"
	"context"
	"sync"
	"time"
)

const (
	defaultRecoveryInterval = 5 * time.Minute
	deviceSweepInterval     = time.Hour
)

// Priority determines command admission order. Lower values run first.
type Priority int

const (
	// PriorityRenewal outranks PriorityConnect: a late renewal drops a live stream,
	// whereas a late connection merely postpones one.
	PriorityRenewal Priority = 0
	PriorityConnect Priority = 1
)

// Clock supplies scheduler time and context-aware waiting.
type Clock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// NewRealClock returns a Clock backed by the system clock.
func NewRealClock() Clock { return realClock{} }

// Options configures scheduler quota ceilings and recovery.
type Options struct {
	// GlobalQPM is the ceiling across every device. Keep below Google's 10.
	GlobalQPM int
	// DeviceQPH is the ceiling for a single device. Keep below Google's 100.
	DeviceQPH int
	// RecoveryInterval is the quiet period between rate increases after a 429.
	RecoveryInterval time.Duration
}

type waiter struct {
	priority Priority
	seq      uint64
	ready    chan struct{}
	index    int
	granted  bool
}

type waiterQueue []*waiter

func (q waiterQueue) Len() int { return len(q) }

func (q waiterQueue) Less(i, j int) bool {
	if q[i].priority != q[j].priority {
		return q[i].priority < q[j].priority
	}
	return q[i].seq < q[j].seq
}

func (q waiterQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index, q[j].index = i, j
}

func (q *waiterQueue) Push(x any) {
	w := x.(*waiter)
	w.index = len(*q)
	*q = append(*q, w)
}

func (q *waiterQueue) Pop() any {
	old := *q
	n := len(old)
	w := old[n-1]
	old[n-1] = nil
	w.index = -1
	*q = old[:n-1]
	return w
}

// Scheduler serialises commands and tracks their quota windows.
type Scheduler struct {
	clock Clock
	opts  Options

	mu              sync.Mutex
	qpm             int
	lastRateLimited time.Time
	globalCalls     []time.Time
	deviceCalls     map[string][]time.Time
	lastDeviceSweep time.Time
	queue           waiterQueue
	seq             uint64
	holder          bool
}

// NewScheduler creates a command scheduler.
func NewScheduler(clock Clock, opts Options) *Scheduler {
	if clock == nil {
		clock = realClock{}
	}
	if opts.GlobalQPM <= 0 {
		opts.GlobalQPM = 8
	}
	if opts.DeviceQPH <= 0 {
		opts.DeviceQPH = 90
	}
	if opts.RecoveryInterval <= 0 {
		opts.RecoveryInterval = defaultRecoveryInterval
	}
	return &Scheduler{
		clock:       clock,
		opts:        opts,
		qpm:         opts.GlobalQPM,
		deviceCalls: make(map[string][]time.Time),
	}
}

// CurrentQPM reports the effective per-minute ceiling.
func (s *Scheduler) CurrentQPM() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recoverRate(s.clock.Now())
	return s.qpm
}

// NoteRateLimited halves the effective rate after a 429, to a floor of 1.
func (s *Scheduler) NoteRateLimited() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock.Now()
	s.recoverRate(now)
	if s.qpm > 1 {
		s.qpm /= 2
	}
	if s.qpm < 1 {
		s.qpm = 1
	}
	s.lastRateLimited = now
}

// recoverRate doubles the rate for each quiet recovery interval, up to the configured
// ceiling. The caller must hold s.mu.
func (s *Scheduler) recoverRate(now time.Time) {
	if s.lastRateLimited.IsZero() || s.qpm >= s.opts.GlobalQPM {
		return
	}
	elapsed := now.Sub(s.lastRateLimited)
	if elapsed < s.opts.RecoveryInterval {
		return
	}

	steps := int(elapsed / s.opts.RecoveryInterval)
	for steps > 0 && s.qpm < s.opts.GlobalQPM {
		s.qpm *= 2
		if s.qpm > s.opts.GlobalQPM {
			s.qpm = s.opts.GlobalQPM
		}
		s.lastRateLimited = s.lastRateLimited.Add(s.opts.RecoveryInterval)
		steps--
	}
}

func prune(times []time.Time, cutoff time.Time) []time.Time {
	kept := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	return kept
}

// waitFor returns how long the caller must sleep before a slot is free, or zero.
// The caller must hold s.mu.
func (s *Scheduler) waitFor(deviceID string) time.Duration {
	now := s.clock.Now()
	s.recoverRate(now)

	s.globalCalls = prune(s.globalCalls, now.Add(-time.Minute))
	deviceCutoff := now.Add(-time.Hour)
	if s.lastDeviceSweep.IsZero() || now.Sub(s.lastDeviceSweep) >= deviceSweepInterval {
		for id, calls := range s.deviceCalls {
			calls = prune(calls, deviceCutoff)
			if len(calls) == 0 {
				delete(s.deviceCalls, id)
				continue
			}
			s.deviceCalls[id] = calls
		}
		s.lastDeviceSweep = now
	} else {
		calls := prune(s.deviceCalls[deviceID], deviceCutoff)
		if len(calls) == 0 {
			delete(s.deviceCalls, deviceID)
		} else {
			s.deviceCalls[deviceID] = calls
		}
	}

	var wait time.Duration
	if len(s.globalCalls) >= s.qpm {
		if d := time.Minute - now.Sub(s.globalCalls[0]); d > wait {
			wait = d
		}
	}
	if calls := s.deviceCalls[deviceID]; len(calls) >= s.opts.DeviceQPH {
		if d := time.Hour - now.Sub(calls[0]); d > wait {
			wait = d
		}
	}
	return wait
}

// Do runs fn once a quota slot is available. Calls are admitted one at a time, ordered
// by priority then arrival, so renewals overtake queued connections.
func (s *Scheduler) Do(ctx context.Context, deviceID string, p Priority, fn func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w := &waiter{
		priority: p,
		index:    -1,
	}
	if err := s.acquire(ctx, w); err != nil {
		return err
	}
	ownsSlot := true
	defer func() {
		if ownsSlot {
			s.release()
		}
	}()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.mu.Lock()
		wait := s.waitFor(deviceID)
		if wait <= 0 {
			now := s.clock.Now()
			s.globalCalls = append(s.globalCalls, now)
			s.deviceCalls[deviceID] = append(s.deviceCalls[deviceID], now)
			s.mu.Unlock()
			return fn(ctx)
		}
		s.mu.Unlock()

		s.release()
		ownsSlot = false
		if err := s.clock.Sleep(ctx, wait); err != nil {
			return err
		}
		if err := s.acquire(ctx, w); err != nil {
			return err
		}
		ownsSlot = true
	}
}

func (s *Scheduler) acquire(ctx context.Context, w *waiter) error {
	s.mu.Lock()
	if w.seq == 0 {
		s.seq++
		w.seq = s.seq
	}
	w.ready = make(chan struct{})
	w.index = -1
	w.granted = false
	if !s.holder {
		s.holder = true
		s.mu.Unlock()
		return nil
	}
	heap.Push(&s.queue, w)
	s.mu.Unlock()

	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		if !w.granted {
			heap.Remove(&s.queue, w.index)
			s.mu.Unlock()
			return ctx.Err()
		}
		s.mu.Unlock()

		// The slot was handed over concurrently with cancellation. Pass it on so the
		// queue cannot stall.
		s.release()
		return ctx.Err()
	}
}

func (s *Scheduler) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queue.Len() == 0 {
		s.holder = false
		return
	}
	next := heap.Pop(&s.queue).(*waiter)
	next.granted = true
	close(next.ready)
}
