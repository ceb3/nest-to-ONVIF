// Package supervisor runs every configured camera at once and keeps each one
// running for as long as the process lives.
//
// The stream command runs a single camera for a fixed duration and reports on
// it, which is what verification needed. A deployment needs the opposite: all
// cameras, indefinitely, with one camera's failure isolated from the rest.
// Protect marks a camera offline the moment its RTSP path goes idle, so the
// question is not whether a session drops but how quickly the next one starts.
//
// Cameras are independent here, with one exception: they share the scheduler
// the caller built their managers with, so Google's per-project quota is
// accounted for across all of them rather than per camera. That sharing also
// means renewals outrank new connections globally, so a camera reconnecting
// cannot starve a live camera's renewal and drop it.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ceb3/nest-to-ONVIF/internal/config"
)

// Runner is the part of a session manager the supervisor depends on.
type Runner interface {
	Run(ctx context.Context) error
}

// Factory builds the runner for one camera. It is called again on every
// restart, so a runner does not have to be reusable after Run returns.
type Factory func(config.Camera) (Runner, error)

// Defaults for restart pacing. The first retry is quick because the common
// case is a dropped session that will reconnect immediately; the cap keeps a
// camera that is genuinely broken — unsupported by SDM, say — from consuming
// quota that the working cameras need.
const (
	DefaultRestartDelay    = 2 * time.Second
	DefaultMaxRestartDelay = 2 * time.Minute
	DefaultHealthyAfter    = 5 * time.Minute
)

// Supervisor runs a set of cameras until its context is cancelled.
type Supervisor struct {
	cameras []config.Camera
	factory Factory

	// RestartDelay is the delay before the first restart, doubling on each
	// consecutive failure up to MaxRestartDelay.
	RestartDelay    time.Duration
	MaxRestartDelay time.Duration

	// HealthyAfter is how long a run must last to be treated as a recovery,
	// which resets that camera's backoff. Without it a camera that flaps stays
	// pinned at the cap forever once it has been failing for a while.
	HealthyAfter time.Duration

	// Sleep is the delay mechanism, replaceable in tests. It must return early
	// with an error when ctx is cancelled.
	Sleep func(ctx context.Context, d time.Duration) error

	Logger *slog.Logger
}

// New builds a supervisor for cameras, using factory to construct each runner.
func New(cameras []config.Camera, factory Factory) *Supervisor {
	return &Supervisor{
		cameras:         cameras,
		factory:         factory,
		RestartDelay:    DefaultRestartDelay,
		MaxRestartDelay: DefaultMaxRestartDelay,
		HealthyAfter:    DefaultHealthyAfter,
		Sleep:           sleepCtx,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run starts every camera and blocks until ctx is cancelled and all of them
// have stopped. Cancellation is the expected way to stop, so it is not an
// error. Run only fails for a fault that affects every camera.
func (s *Supervisor) Run(ctx context.Context) error {
	if len(s.cameras) == 0 {
		return errors.New("no cameras configured; nothing to supervise")
	}

	log := s.Logger
	if log == nil {
		log = slog.Default()
	}

	log.Info("starting cameras", "count", len(s.cameras))

	var wg sync.WaitGroup
	for _, cam := range s.cameras {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.superviseOne(ctx, cam, log.With("camera", cam.Name))
		}()
	}
	wg.Wait()

	log.Info("all cameras stopped")
	return nil
}

// superviseOne keeps a single camera running until ctx is cancelled.
func (s *Supervisor) superviseOne(ctx context.Context, cam config.Camera, log *slog.Logger) {
	delay := s.RestartDelay
	for {
		if ctx.Err() != nil {
			return
		}

		started := time.Now()
		err := s.runOnce(ctx, cam)
		lasted := time.Since(started)

		if ctx.Err() != nil {
			log.Info("stopped", "reason", "shutting down")
			return
		}

		switch {
		case err != nil:
			log.Warn("camera stopped", "after", lasted.Round(time.Second), "error", err)
		default:
			// Run returning nil still means this camera is no longer publishing,
			// which Protect sees as offline, so it is restarted either way.
			log.Warn("camera stopped without error", "after", lasted.Round(time.Second))
		}

		// A run that lasted is evidence the camera works and that whatever
		// failed was transient, so the next failure starts from the short delay
		// again rather than the cap it had climbed to.
		if lasted >= s.HealthyAfter {
			delay = s.RestartDelay
		}

		log.Info("restarting", "in", delay)
		if err := s.Sleep(ctx, delay); err != nil {
			return
		}

		if delay = delay * 2; delay > s.MaxRestartDelay {
			delay = s.MaxRestartDelay
		}
	}
}

// runOnce builds a runner and runs it once, converting a panic in either into
// an error so that one camera cannot take the process down with it.
func (s *Supervisor) runOnce(ctx context.Context, cam config.Camera) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()

	runner, err := s.factory(cam)
	if err != nil {
		return fmt.Errorf("building session: %w", err)
	}
	if runner == nil {
		return errors.New("session factory returned no runner")
	}
	return runner.Run(ctx)
}
