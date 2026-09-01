package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mustacheride/nest-to-ONVIF/internal/config"
	"github.com/mustacheride/nest-to-ONVIF/internal/media"
	"github.com/mustacheride/nest-to-ONVIF/internal/scheduler"
	"github.com/mustacheride/nest-to-ONVIF/internal/sdm"
	"github.com/mustacheride/nest-to-ONVIF/internal/session"
)

const (
	googleGlobalQPMCeiling = 10
	googleDeviceQPHCeiling = 100

	// Stay below Google's 10 QPM and 100 QPH ceilings so minor timing
	// jitter does not turn the stream experiment into a quota violation.
	streamGlobalQPM = 8
	streamDeviceQPH = 90

	renewalObserverDrainTimeout = time.Second
)

type streamSummary struct {
	Camera               string
	RequestedDuration    time.Duration
	Elapsed              time.Duration
	EndReason            string
	SurvivedFullDuration bool
	Packets              uint64
	ConnectionsSucceeded uint64
	ConnectionsFailed    uint64
	RenewalsSucceeded    uint64
	RenewalsFailed       uint64
	UnsupportedFailures  uint64
	RateLimitFailures    uint64
	ObserverUndelivered  uint64
	SinkPublished        uint64
	SinkDiscarded        uint64
	SinkQueueDropped     uint64
	SinkFailed           uint64
}

func resolveCamera(cameras []config.Camera, name, cfgPath string) (*config.Camera, error) {
	for i := range cameras {
		if cameras[i].Name == name {
			return &cameras[i], nil
		}
	}
	available := make([]string, 0, len(cameras))
	for _, camera := range cameras {
		available = append(available, fmt.Sprintf("%q", camera.Name))
	}
	if len(available) == 0 {
		return nil, fmt.Errorf("no camera named %q in %s; no cameras are configured", name, cfgPath)
	}
	return nil, fmt.Errorf("no camera named %q in %s; available cameras: %s",
		name, cfgPath, strings.Join(available, ", "))
}

func formatStreamSummary(summary streamSummary) string {
	return fmt.Sprintf(
		"Stream summary:\n"+
			"  camera=%q\n"+
			"  requested_duration=%s\n"+
			"  elapsed=%s\n"+
			"  end_reason=%s\n"+
			"  survived_full_duration=%t\n"+
			"  packets=%d\n"+
			"  connections_succeeded=%d\n"+
			"  connections_failed=%d\n"+
			"  renewals_succeeded=%d\n"+
			"  renewals_failed=%d\n"+
			"  unsupported_failures=%d\n"+
			"  rate_limit_failures=%d\n"+
			"  observer_events_undelivered=%d\n"+
			"  sink_published=%d\n"+
			"  sink_discarded=%d\n"+
			"  sink_queue_dropped=%d\n"+
			"  sink_failed=%d\n",
		summary.Camera,
		summary.RequestedDuration,
		summary.Elapsed.Round(time.Second),
		summary.EndReason,
		summary.SurvivedFullDuration,
		summary.Packets,
		summary.ConnectionsSucceeded,
		summary.ConnectionsFailed,
		summary.RenewalsSucceeded,
		summary.RenewalsFailed,
		summary.UnsupportedFailures,
		summary.RateLimitFailures,
		summary.ObserverUndelivered,
		summary.SinkPublished,
		summary.SinkDiscarded,
		summary.SinkQueueDropped,
		summary.SinkFailed,
	)
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *lockedWriter) printf(format string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = fmt.Fprintf(w.w, format, args...)
}

type streamManager interface {
	SetSinkFactory(session.SinkFactory)
	SetRenewalObserver(session.RenewalObserver)
	Run(context.Context) error
	DrainRenewalObserver(context.Context) int
	State() session.State
	Packets() uint64
	ConnectionStats() session.ConnectionStats
	RenewalStats() session.RenewalStats
	SinkStats() session.SinkStats
}

func RunStream(ctx context.Context, cfgPath, tokenPath, cameraName string, dur time.Duration) error {
	return runStream(ctx, cfgPath, tokenPath, cameraName, dur, os.Stdout)
}

func runStream(
	ctx context.Context,
	cfgPath, tokenPath, cameraName string,
	dur time.Duration,
	out io.Writer,
) error {
	if dur <= 0 {
		return fmt.Errorf("stream duration must be positive, got %s", dur)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	camera, err := resolveCamera(cfg.Cameras, cameraName, cfgPath)
	if err != nil {
		return err
	}

	opts := scheduler.Options{GlobalQPM: streamGlobalQPM, DeviceQPH: streamDeviceQPH}

	tokenSource, err := sdm.NewTokenSource(ctx, cfg.Google, sdm.NewFileTokenStore(tokenPath))
	if err != nil {
		return err
	}
	client := sdm.NewClient(cfg.Google.ProjectID, tokenSource)
	clock := scheduler.NewRealClock()
	sched := scheduler.NewScheduler(clock, opts)
	manager := session.NewManager(*camera, client.StreamAPIFor(), sched, clock)

	return runStreamWithManager(ctx, *camera, cfg.Media, dur, manager, out)
}

func runStreamWithManager(
	ctx context.Context,
	camera config.Camera,
	mediaConfig config.MediaConfig,
	dur time.Duration,
	manager streamManager,
	out io.Writer,
) error {
	manager.SetSinkFactory(func(cam config.Camera) (session.TrackSink, error) {
		return media.NewPublisher(cam, mediaConfig.RTSPBaseURL)
	})
	return monitorStream(ctx, camera.Name, dur, manager, out)
}

func monitorStream(
	ctx context.Context,
	cameraName string,
	dur time.Duration,
	manager streamManager,
	out io.Writer,
) error {
	writer := &lockedWriter{w: out}
	manager.SetRenewalObserver(func(event session.RenewalEvent) {
		if event.Outcome == session.RenewalSucceeded {
			writer.printf("[%s] renewal=succeeded\n", event.At.Format(time.TimeOnly))
			return
		}
		writer.printf("[%s] renewal=failed cause=%s\n",
			event.At.Format(time.TimeOnly), event.Failure)
	})

	runCtx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()
	started := time.Now()

	tickerDone := make(chan struct{})
	go func() {
		defer close(tickerDone)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		var last uint64
		for {
			select {
			case <-runCtx.Done():
				return
			case now := <-ticker.C:
				packets := manager.Packets()
				writer.printf("[%s] state=%s packets=%d (+%d in 30s)\n",
					now.Format(time.TimeOnly), manager.State(), packets, packets-last)
				last = packets
			}
		}
	}()

	writer.printf("Streaming %q for %s. Renewals occur at 60%% of each 5-minute session.\n",
		cameraName, dur)
	runErr := manager.Run(runCtx)
	<-tickerDone
	drainCtx, drainCancel := context.WithTimeout(context.Background(), renewalObserverDrainTimeout)
	observerUndelivered := manager.DrainRenewalObserver(drainCtx)
	drainCancel()

	connections := manager.ConnectionStats()
	renewals := manager.RenewalStats()
	packets := manager.Packets()
	sink := manager.SinkStats()
	// Receiving RTP and renewing the SDM session prove only that the upstream
	// half worked. A run whose sink published nothing republished nothing, and
	// must not be reported as a success.
	sessionUp := packets > 0 || renewals.Succeeded > 0
	published := sink.Published > 0
	established := sessionUp && published
	timedOut := ctx.Err() == nil && errors.Is(runCtx.Err(), context.DeadlineExceeded)
	survived := timedOut && established

	endReason := "ended unexpectedly"
	if ctx.Err() != nil {
		endReason = "interrupted"
	} else if survived {
		endReason = "duration elapsed"
	} else if timedOut && sessionUp {
		endReason = "duration elapsed without publishing media"
	} else if timedOut {
		endReason = "duration elapsed without live session"
	} else if runErr != nil {
		endReason = runErr.Error()
	}
	writer.printf("%s", formatStreamSummary(streamSummary{
		Camera:               cameraName,
		RequestedDuration:    dur,
		Elapsed:              time.Since(started),
		EndReason:            endReason,
		SurvivedFullDuration: survived,
		Packets:              packets,
		ConnectionsSucceeded: connections.Succeeded,
		ConnectionsFailed:    connections.Failed,
		RenewalsSucceeded:    renewals.Succeeded,
		RenewalsFailed:       renewals.Failed,
		UnsupportedFailures:  renewals.Unsupported,
		RateLimitFailures:    renewals.RateLimited,
		ObserverUndelivered:  uint64(observerUndelivered),
		SinkPublished:        sink.Published,
		SinkDiscarded:        sink.Discarded,
		SinkQueueDropped:     sink.QueueDropped,
		SinkFailed:           sink.Failed,
	}))

	if !sessionUp {
		return errors.New("no live session established")
	}
	if ctx.Err() != nil {
		// The operator ended the run, so an unfinished publish is not a fault.
		return nil
	}
	if !published {
		return fmt.Errorf(
			"no media published to RTSP (sink discarded %d, queue-dropped %d, failed %d)",
			sink.Discarded, sink.QueueDropped, sink.Failed)
	}
	if survived {
		return nil
	}
	if runErr != nil {
		return runErr
	}
	return errors.New("stream manager ended unexpectedly")
}
