package scheduler

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClock advances only when Sleep is called, making rate limiting deterministic.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slept = append(f.slept, d)
	f.now = f.now.Add(d)
	return nil
}

func (f *fakeClock) totalSlept() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	var total time.Duration
	for _, d := range f.slept {
		total += d
	}
	return total
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func TestAllowsBurstUpToGlobalLimit(t *testing.T) {
	clock := newFakeClock()
	s := NewScheduler(clock, Options{GlobalQPM: 8, DeviceQPH: 90})

	for i := 0; i < 8; i++ {
		require.NoError(t, s.Do(context.Background(), "dev", PriorityConnect, func(context.Context) error { return nil }))
	}
	assert.Zero(t, clock.totalSlept(), "first 8 calls should not wait")
}

func TestThrottlesBeyondGlobalLimit(t *testing.T) {
	clock := newFakeClock()
	s := NewScheduler(clock, Options{GlobalQPM: 8, DeviceQPH: 90})

	for i := 0; i < 8; i++ {
		require.NoError(t, s.Do(context.Background(), "dev", PriorityConnect, func(context.Context) error { return nil }))
	}
	require.NoError(t, s.Do(context.Background(), "dev", PriorityConnect, func(context.Context) error { return nil }))

	assert.Positive(t, clock.totalSlept(), "the 9th call must wait for a token")
}

func TestEnforcesPerDeviceHourlyLimit(t *testing.T) {
	clock := newFakeClock()
	s := NewScheduler(clock, Options{GlobalQPM: 1000, DeviceQPH: 3})

	for i := 0; i < 3; i++ {
		require.NoError(t, s.Do(context.Background(), "cam", PriorityConnect, func(context.Context) error { return nil }))
	}
	before := clock.totalSlept()
	require.NoError(t, s.Do(context.Background(), "cam", PriorityConnect, func(context.Context) error { return nil }))
	assert.Greater(t, clock.totalSlept(), before, "4th call on the same device must wait")
}

func TestPerDeviceLimitIsIndependent(t *testing.T) {
	clock := newFakeClock()
	s := NewScheduler(clock, Options{GlobalQPM: 1000, DeviceQPH: 2})

	require.NoError(t, s.Do(context.Background(), "a", PriorityConnect, func(context.Context) error { return nil }))
	require.NoError(t, s.Do(context.Background(), "a", PriorityConnect, func(context.Context) error { return nil }))
	before := clock.totalSlept()
	require.NoError(t, s.Do(context.Background(), "b", PriorityConnect, func(context.Context) error { return nil }))
	assert.Equal(t, before, clock.totalSlept(), "a different device must not be throttled")
}

func TestRateLimitHalvesRate(t *testing.T) {
	clock := newFakeClock()
	s := NewScheduler(clock, Options{GlobalQPM: 8, DeviceQPH: 90})
	assert.Equal(t, 8, s.CurrentQPM())

	s.NoteRateLimited()
	assert.Equal(t, 4, s.CurrentQPM())

	s.NoteRateLimited()
	assert.Equal(t, 2, s.CurrentQPM())
}

func TestRateNeverDropsBelowOne(t *testing.T) {
	s := NewScheduler(newFakeClock(), Options{GlobalQPM: 2, DeviceQPH: 90})
	for i := 0; i < 10; i++ {
		s.NoteRateLimited()
	}
	assert.Equal(t, 1, s.CurrentQPM())
}

func TestPropagatesFunctionError(t *testing.T) {
	s := NewScheduler(newFakeClock(), Options{GlobalQPM: 8, DeviceQPH: 90})
	want := assert.AnError
	got := s.Do(context.Background(), "dev", PriorityConnect, func(context.Context) error { return want })
	assert.ErrorIs(t, got, want)
}

func TestRespectsCancelledContext(t *testing.T) {
	s := NewScheduler(newFakeClock(), Options{GlobalQPM: 8, DeviceQPH: 90})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := false
	err := s.Do(ctx, "dev", PriorityConnect, func(context.Context) error { called = true; return nil })
	require.Error(t, err)
	assert.False(t, called, "must not run the command with a cancelled context")
}

func TestRateRecoversAfterInterval(t *testing.T) {
	clock := newFakeClock()
	s := NewScheduler(clock, Options{
		GlobalQPM:        8,
		DeviceQPH:        90,
		RecoveryInterval: 5 * time.Minute,
	})

	s.NoteRateLimited()
	s.NoteRateLimited()
	assert.Equal(t, 2, s.CurrentQPM())

	clock.advance(5 * time.Minute)
	assert.Equal(t, 4, s.CurrentQPM())
	clock.advance(5 * time.Minute)
	assert.Equal(t, 8, s.CurrentQPM())
}

func TestRateRecoveryNeverExceedsConfiguredLimit(t *testing.T) {
	clock := newFakeClock()
	s := NewScheduler(clock, Options{
		GlobalQPM:        8,
		DeviceQPH:        90,
		RecoveryInterval: time.Minute,
	})

	s.NoteRateLimited()
	clock.advance(time.Hour)
	assert.Equal(t, 8, s.CurrentQPM())
}

func TestRateLimitRestartsRecoveryInterval(t *testing.T) {
	clock := newFakeClock()
	s := NewScheduler(clock, Options{
		GlobalQPM:        8,
		DeviceQPH:        90,
		RecoveryInterval: 5 * time.Minute,
	})

	s.NoteRateLimited()
	s.NoteRateLimited()
	clock.advance(4 * time.Minute)
	s.NoteRateLimited()
	assert.Equal(t, 1, s.CurrentQPM())

	clock.advance(time.Minute)
	assert.Equal(t, 1, s.CurrentQPM(), "recovery must be measured from the latest rate limit")
	clock.advance(4 * time.Minute)
	assert.Equal(t, 2, s.CurrentQPM())
}

func TestRenewalOutranksQueuedConnect(t *testing.T) {
	s := NewScheduler(newFakeClock(), Options{GlobalQPM: 8, DeviceQPH: 90})
	releaseHolder := make(chan struct{})
	holderStarted := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- s.Do(context.Background(), "holder", PriorityConnect, func(context.Context) error {
			close(holderStarted)
			<-releaseHolder
			return nil
		})
	}()
	<-holderStarted

	order := make(chan string, 2)
	connectDone := make(chan error, 1)
	go func() {
		connectDone <- s.Do(context.Background(), "connect", PriorityConnect, func(context.Context) error {
			order <- "connect"
			return nil
		})
	}()
	waitForQueueLength(t, s, 1)

	renewalDone := make(chan error, 1)
	go func() {
		renewalDone <- s.Do(context.Background(), "renewal", PriorityRenewal, func(context.Context) error {
			order <- "renewal"
			return nil
		})
	}()
	waitForQueueLength(t, s, 2)

	close(releaseHolder)
	assert.Equal(t, "renewal", receive(t, order))
	assert.Equal(t, "connect", receive(t, order))
	require.NoError(t, receive(t, holderDone))
	require.NoError(t, receive(t, renewalDone))
	require.NoError(t, receive(t, connectDone))
}

func TestSamePriorityUsesFIFOOrder(t *testing.T) {
	s := NewScheduler(newFakeClock(), Options{GlobalQPM: 8, DeviceQPH: 90})
	releaseHolder := make(chan struct{})
	holderStarted := make(chan struct{})
	go func() {
		_ = s.Do(context.Background(), "holder", PriorityConnect, func(context.Context) error {
			close(holderStarted)
			<-releaseHolder
			return nil
		})
	}()
	<-holderStarted

	order := make(chan int, 2)
	go func() {
		_ = s.Do(context.Background(), "first", PriorityConnect, func(context.Context) error {
			order <- 1
			return nil
		})
	}()
	waitForQueueLength(t, s, 1)
	go func() {
		_ = s.Do(context.Background(), "second", PriorityConnect, func(context.Context) error {
			order <- 2
			return nil
		})
	}()
	waitForQueueLength(t, s, 2)

	close(releaseHolder)
	assert.Equal(t, 1, receive(t, order))
	assert.Equal(t, 2, receive(t, order))
}

func TestQueuedCancellationRemovesWaiter(t *testing.T) {
	s := NewScheduler(newFakeClock(), Options{GlobalQPM: 8, DeviceQPH: 90})
	releaseHolder := make(chan struct{})
	holderStarted := make(chan struct{})
	go func() {
		_ = s.Do(context.Background(), "holder", PriorityConnect, func(context.Context) error {
			close(holderStarted)
			<-releaseHolder
			return nil
		})
	}()
	<-holderStarted

	ctx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		waiterDone <- s.Do(ctx, "queued", PriorityConnect, func(context.Context) error {
			t.Error("cancelled queued command must not run")
			return nil
		})
	}()
	waitForQueueLength(t, s, 1)
	cancel()

	assert.ErrorIs(t, receive(t, waiterDone), context.Canceled)
	waitForQueueLength(t, s, 0)
	close(releaseHolder)
}

func TestRenewalOvertakesConnectWaitingForDeviceQuota(t *testing.T) {
	clock := newBlockingClock()
	s := NewScheduler(clock, Options{GlobalQPM: 1000, DeviceQPH: 1})
	require.NoError(t, s.Do(context.Background(), "a", PriorityConnect, func(context.Context) error { return nil }))

	order := make(chan string, 2)
	connectDone := make(chan error, 1)
	go func() {
		connectDone <- s.Do(context.Background(), "a", PriorityConnect, func(context.Context) error {
			order <- "connect"
			return nil
		})
	}()
	clock.waitForSleep(t)

	renewalDone := make(chan error, 1)
	go func() {
		renewalDone <- s.Do(context.Background(), "b", PriorityRenewal, func(context.Context) error {
			order <- "renewal"
			return nil
		})
	}()

	assert.Equal(t, "renewal", receive(t, order))
	clock.advanceAndWake(time.Hour)
	assert.Equal(t, "connect", receive(t, order))
	require.NoError(t, receive(t, renewalDone))
	require.NoError(t, receive(t, connectDone))
}

func TestQuotaBlockedWaiterKeepsArrivalOrderWhenRequeued(t *testing.T) {
	clock := newBlockingClock()
	s := NewScheduler(clock, Options{GlobalQPM: 1000, DeviceQPH: 1})
	require.NoError(t, s.Do(context.Background(), "a", PriorityConnect, func(context.Context) error { return nil }))

	order := make(chan string, 3)
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- s.Do(context.Background(), "a", PriorityConnect, func(context.Context) error {
			order <- "first"
			return nil
		})
	}()
	clock.waitForSleep(t)

	holderStarted := make(chan struct{})
	releaseHolder := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- s.Do(context.Background(), "b", PriorityConnect, func(context.Context) error {
			close(holderStarted)
			<-releaseHolder
			order <- "holder"
			return nil
		})
	}()
	receive(t, holderStarted)

	const laterCount = 8
	laterDone := make(chan error, laterCount)
	for i := 0; i < laterCount; i++ {
		deviceID := string(rune('c' + i))
		go func() {
			laterDone <- s.Do(context.Background(), deviceID, PriorityConnect, func(context.Context) error {
				order <- "later"
				return nil
			})
		}()
		waitForQueueLength(t, s, i+1)
	}

	clock.advanceAndWake(time.Hour)
	waitForQueueLength(t, s, laterCount+1)

	close(releaseHolder)
	assert.Equal(t, "holder", receive(t, order))
	assert.Equal(t, "first", receive(t, order))
	require.NoError(t, receive(t, holderDone))
	require.NoError(t, receive(t, firstDone))
	for i := 0; i < laterCount; i++ {
		assert.Equal(t, "later", receive(t, order))
		require.NoError(t, receive(t, laterDone))
	}
}

func TestDropsIdleDeviceCallKeys(t *testing.T) {
	clock := newFakeClock()
	s := NewScheduler(clock, Options{GlobalQPM: 1000, DeviceQPH: 90})
	require.NoError(t, s.Do(context.Background(), "idle", PriorityConnect, func(context.Context) error { return nil }))

	clock.advance(time.Hour)
	require.NoError(t, s.Do(context.Background(), "active", PriorityConnect, func(context.Context) error { return nil }))

	s.mu.Lock()
	_, exists := s.deviceCalls["idle"]
	s.mu.Unlock()
	assert.False(t, exists, "an expired idle device must be removed during a later sweep")
}

type blockingClock struct {
	mu           sync.Mutex
	now          time.Time
	sleepStarted chan time.Duration
	wake         chan struct{}
}

func newBlockingClock() *blockingClock {
	return &blockingClock{
		now:          time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		sleepStarted: make(chan time.Duration, 1),
		wake:         make(chan struct{}, 1),
	}
}

func (f *blockingClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *blockingClock) Sleep(ctx context.Context, d time.Duration) error {
	select {
	case f.sleepStarted <- d:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-f.wake:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *blockingClock) waitForSleep(t *testing.T) {
	t.Helper()
	assert.Equal(t, time.Hour, receive(t, f.sleepStarted))
}

func (f *blockingClock) advanceAndWake(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
	f.wake <- struct{}{}
}

func waitForQueueLength(t *testing.T, s *Scheduler, want int) {
	t.Helper()
	for i := 0; i < 100_000; i++ {
		s.mu.Lock()
		got := s.queue.Len()
		s.mu.Unlock()
		if got == want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("queue length did not reach %d", want)
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for goroutine")
		var zero T
		return zero
	}
}
