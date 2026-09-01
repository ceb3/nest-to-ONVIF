package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/mustacheride/nest-to-ONVIF/internal/config"
	"github.com/mustacheride/nest-to-ONVIF/internal/events"
	"github.com/mustacheride/nest-to-ONVIF/internal/media"
	"github.com/mustacheride/nest-to-ONVIF/internal/scheduler"
	"github.com/mustacheride/nest-to-ONVIF/internal/sdm"
	"github.com/mustacheride/nest-to-ONVIF/internal/session"
	"github.com/mustacheride/nest-to-ONVIF/internal/supervisor"
	"github.com/mustacheride/nest-to-ONVIF/internal/viewer"
)

// logLevelFromEnv reads NEST_BRIDGE_LOG_LEVEL. An unset or unrecognised value
// yields info, so a typo quiets nothing rather than silencing the process.
func logLevelFromEnv() slog.Level {
	var level slog.Level
	if err := level.UnmarshalText([]byte(os.Getenv("NEST_BRIDGE_LOG_LEVEL"))); err != nil {
		return slog.LevelInfo
	}
	return level
}

// RunServe runs every configured camera until the process is signalled.
func RunServe(ctx context.Context, cfgPath, tokenPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if len(cfg.Cameras) == 0 {
		return fmt.Errorf("no cameras configured in %s", cfgPath)
	}

	tokenSource, err := sdm.NewTokenSource(ctx, cfg.Google, sdm.NewFileTokenStore(tokenPath))
	if err != nil {
		return err
	}
	client := sdm.NewClient(cfg.Google.ProjectID, tokenSource)
	clock := scheduler.NewRealClock()

	// One scheduler for the whole process. Google's quota is per project, not
	// per camera, so a scheduler per camera would let four cameras between them
	// exceed a ceiling each of them individually respects.
	sched := scheduler.NewScheduler(clock, scheduler.Options{
		GlobalQPM: streamGlobalQPM,
		DeviceQPH: streamDeviceQPH,
	})

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevelFromEnv()}))

	sup := supervisor.New(cfg.Cameras, func(cam config.Camera) (supervisor.Runner, error) {
		manager := session.NewManager(cam, client.StreamAPIFor(), sched, clock)
		manager.SetSinkFactory(func(c config.Camera) (session.TrackSink, error) {
			return media.NewPublisher(c, cfg.Media.RTSPBaseURL)
		})
		return manager, nil
	})
	sup.Logger = logger

	logger.Info("nest-bridge starting",
		"cameras", len(cfg.Cameras), "rtsp", cfg.Media.RTSPBaseURL)

	// Events are an enhancement; streaming is the product. Everything below
	// either starts or logs why it did not, and never returns an error.
	var wg sync.WaitGroup
	eventsRT := startEvents(ctx, cfg, clock, logger, &wg)

	if listen := cfg.ViewerListen(); listen != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			srv := viewer.NewServer(cfg, eventsRT.enabled, eventsRT.bus)
			if err := srv.ListenAndServe(ctx, listen); err != nil && ctx.Err() == nil {
				logger.Error("viewer stopped", "error", err)
			}
		}()
	}

	err = sup.Run(ctx)

	// The supervisor only returns once every camera has stopped, which means ctx
	// is already cancelled; stopEvents covers the case where it is not.
	eventsRT.stop()
	wg.Wait()
	return err
}

// eventRuntime holds optional Pub/Sub → ONVIF motion forwarding and the viewer
// event bus (always non-nil so the viewer can subscribe when enabled later).
type eventRuntime struct {
	stop    func()
	bus     *viewer.EventBus
	enabled bool
}

// startEvents brings up the Pub/Sub subscriber and optional ONVIF motion
// delivery, returning a handle that stops them.
func startEvents(ctx context.Context, cfg *config.Config, clock scheduler.Clock, logger *slog.Logger, wg *sync.WaitGroup) eventRuntime {
	rt := eventRuntime{stop: func() {}, bus: viewer.NewEventBus()}
	log := logger.With("component", "events")

	if !cfg.Events.Onvif {
		log.Info("nest detections → ONVIF motion disabled", "reason", "events.onvif is false")
		return rt
	}

	eventCameras := make([]config.Camera, 0, len(cfg.Cameras))
	for _, cam := range cfg.Cameras {
		if cam.EventsEnabled {
			eventCameras = append(eventCameras, cam)
		}
	}
	if len(eventCameras) == 0 {
		log.Info("nest detections → ONVIF motion disabled", "reason", "no cameras have event forwarding enabled")
		return rt
	}

	switch {
	case cfg.Google.PubSubSubscription == "":
		log.Info("nest detections → ONVIF motion disabled", "reason", "google.pubsub_subscription is unset")
		return rt
	case cfg.Google.ServiceAccountKey == "":
		log.Info("nest detections → ONVIF motion disabled", "reason", "google.service_account_key is unset")
		return rt
	}

	// The SDM user token cannot read Pub/Sub — sdm.service is the only scope the
	// SDM API issues — so events authenticate as a separate service account.
	tokenSource, err := events.TokenSourceFromKeyFile(ctx, cfg.Google.ServiceAccountKey)
	if err != nil {
		log.Warn("nest detections → ONVIF motion disabled", "error", err)
		return rt
	}

	eventsCtx, cancel := context.WithCancel(ctx)
	rt.stop = cancel

	trigger := events.NewTrigger()
	trigger.Logger = log
	dispatcher := events.NewDispatcher(trigger.Deliver)
	dispatcher.Logger = log

	emit := func(e events.Edge) {
		rt.bus.Publish(e)
		dispatcher.Send(e)
	}
	tracker := events.NewTracker(eventCameras, emit, clock)
	tracker.Logger = log

	devices := make([]string, 0, len(eventCameras))
	for _, cam := range eventCameras {
		devices = append(devices, cam.DeviceID)
	}
	sub := events.NewSubscriber(cfg.Google.PubSubSubscription, devices, tokenSource, tracker.Handle)
	sub.Logger = log

	log.Info("nest detections → ONVIF motion enabled", "devices", len(devices))
	rt.enabled = true

	runners := []func(context.Context){dispatcher.Run, tracker.Run, sub.Run}
	for _, run := range runners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run(eventsCtx)
		}()
	}

	return rt
}
