package supervisor_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mustacheride/nest-to-ONVIF/internal/config"
	"github.com/mustacheride/nest-to-ONVIF/internal/supervisor"
)

func cameras(names ...string) []config.Camera {
	out := make([]config.Camera, 0, len(names))
	for _, n := range names {
		out = append(out, config.Camera{Name: n})
	}
	return out
}

// fakeRunner stands in for a session manager. Run blocks until told otherwise.
type fakeRunner struct {
	mu      sync.Mutex
	runs    int
	release chan error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{release: make(chan error, 16)}
}

func (f *fakeRunner) Run(ctx context.Context) error {
	f.mu.Lock()
	f.runs++
	f.mu.Unlock()
	select {
	case err := <-f.release:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeRunner) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs
}

func eventually(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	require.Eventually(t, cond, 2*time.Second, time.Millisecond, msg)
}

func TestRunStartsEveryCameraConcurrently(t *testing.T) {
	runners := map[string]*fakeRunner{}
	var mu sync.Mutex

	sup := supervisor.New(cameras("a", "b", "c"), func(cam config.Camera) (supervisor.Runner, error) {
		mu.Lock()
		defer mu.Unlock()
		r := newFakeRunner()
		runners[cam.Name] = r
		return r, nil
	})
	sup.RestartDelay = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	eventually(t, "all three cameras must be running at once", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(runners) == 3
	})

	cancel()
	require.NoError(t, <-done)
}

// A camera that cannot stay up must not take the others down with it.
func TestOneFailingCameraDoesNotStopTheOthers(t *testing.T) {
	failing, healthy := newFakeRunner(), newFakeRunner()

	sup := supervisor.New(cameras("failing", "healthy"), func(cam config.Camera) (supervisor.Runner, error) {
		if cam.Name == "failing" {
			return failing, nil
		}
		return healthy, nil
	})
	sup.RestartDelay = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	for i := 0; i < 3; i++ {
		failing.release <- errors.New("boom")
	}
	eventually(t, "the failing camera must be retried", func() bool {
		return failing.runCount() >= 3
	})
	assert.Equal(t, 1, healthy.runCount(), "the healthy camera must not be restarted")

	cancel()
	require.NoError(t, <-done)
}

// Manager.Run returning without an error still means the camera stopped, so it
// has to be restarted just the same.
func TestACameraThatStopsCleanlyIsStillRestarted(t *testing.T) {
	runner := newFakeRunner()
	sup := supervisor.New(cameras("a"), func(config.Camera) (supervisor.Runner, error) {
		return runner, nil
	})
	sup.RestartDelay = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	runner.release <- nil
	eventually(t, "a clean stop must still restart", func() bool { return runner.runCount() >= 2 })

	cancel()
	require.NoError(t, <-done)
}

// A camera whose factory fails cannot be constructed now, but the fault may be
// transient, so it is retried rather than abandoned.
func TestAFactoryFailureIsRetriedRatherThanFatal(t *testing.T) {
	var attempts atomic.Int64
	healthy := newFakeRunner()

	sup := supervisor.New(cameras("broken", "healthy"), func(cam config.Camera) (supervisor.Runner, error) {
		if cam.Name == "broken" {
			attempts.Add(1)
			return nil, errors.New("cannot build")
		}
		return healthy, nil
	})
	sup.RestartDelay = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	eventually(t, "the failing factory must be retried", func() bool { return attempts.Load() >= 3 })
	assert.Equal(t, 1, healthy.runCount(), "the healthy camera is unaffected")

	cancel()
	require.NoError(t, <-done)
}

// A camera failing immediately and forever must not spin: the delay has to grow.
func TestRestartDelayBacksOffAndIsCapped(t *testing.T) {
	var mu sync.Mutex
	var delays []time.Duration

	runner := newFakeRunner()
	sup := supervisor.New(cameras("a"), func(config.Camera) (supervisor.Runner, error) {
		return runner, nil
	})
	sup.RestartDelay = 10 * time.Millisecond
	sup.MaxRestartDelay = 40 * time.Millisecond
	sup.Sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		delays = append(delays, d)
		mu.Unlock()
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	for i := 0; i < 6; i++ {
		runner.release <- errors.New("boom")
	}
	eventually(t, "enough restarts to observe the cap", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(delays) >= 5
	})
	cancel()
	require.NoError(t, <-done)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 10*time.Millisecond, delays[0])
	assert.Equal(t, 20*time.Millisecond, delays[1])
	assert.Equal(t, 40*time.Millisecond, delays[2])
	for i, d := range delays {
		assert.LessOrEqual(t, d, 40*time.Millisecond, "delay %d exceeded the cap", i)
	}
}

// Backoff that never resets would leave a flapping camera stuck at the cap even
// after it recovers, so a run that lasted has to clear it.
func TestBackoffResetsAfterACameraStaysUp(t *testing.T) {
	var mu sync.Mutex
	var delays []time.Duration

	runner := newFakeRunner()
	sup := supervisor.New(cameras("a"), func(config.Camera) (supervisor.Runner, error) {
		return runner, nil
	})
	sup.RestartDelay = 10 * time.Millisecond
	sup.MaxRestartDelay = 80 * time.Millisecond
	sup.HealthyAfter = 30 * time.Millisecond
	sup.Sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		delays = append(delays, d)
		mu.Unlock()
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	runner.release <- errors.New("boom")
	runner.release <- errors.New("boom")
	eventually(t, "two escalating delays", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(delays) >= 2
	})

	// Let the third run last longer than HealthyAfter before failing.
	time.Sleep(50 * time.Millisecond)
	runner.release <- errors.New("boom")
	eventually(t, "a third delay after the healthy run", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(delays) >= 3
	})
	cancel()
	require.NoError(t, <-done)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 10*time.Millisecond, delays[0])
	assert.Equal(t, 20*time.Millisecond, delays[1])
	assert.Equal(t, 10*time.Millisecond, delays[2],
		"a run that lasted must reset the backoff")
}

func TestRunReturnsOnlyAfterEveryCameraHasStopped(t *testing.T) {
	a, b := newFakeRunner(), newFakeRunner()
	sup := supervisor.New(cameras("a", "b"), func(cam config.Camera) (supervisor.Runner, error) {
		if cam.Name == "a" {
			return a, nil
		}
		return b, nil
	})
	sup.RestartDelay = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	eventually(t, "both started", func() bool { return a.runCount() == 1 && b.runCount() == 1 })
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "a cancelled shutdown is not a failure")
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestRunRejectsAnEmptyCameraList(t *testing.T) {
	sup := supervisor.New(nil, func(config.Camera) (supervisor.Runner, error) {
		return newFakeRunner(), nil
	})

	err := sup.Run(context.Background())

	require.Error(t, err, "starting with nothing to supervise is a configuration error")
	assert.Contains(t, err.Error(), "no cameras")
}
