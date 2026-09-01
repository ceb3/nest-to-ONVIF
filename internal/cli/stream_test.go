package cli

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mustacheride/nest-to-ONVIF/internal/config"
	"github.com/mustacheride/nest-to-ONVIF/internal/media"
	"github.com/mustacheride/nest-to-ONVIF/internal/session"
)

func TestResolveCameraByFriendlyName(t *testing.T) {
	cameras := []config.Camera{
		{Name: "Front Door", DeviceID: "front"},
		{Name: "Driveway", DeviceID: "driveway"},
	}

	camera, err := resolveCamera(cameras, "Driveway", "config.yaml")

	require.NoError(t, err)
	assert.Equal(t, "driveway", camera.DeviceID)
}

func TestResolveCameraListsAvailableNames(t *testing.T) {
	cameras := []config.Camera{
		{Name: "Front Door"},
		{Name: "Driveway"},
	}

	_, err := resolveCamera(cameras, "Garage", "config.yaml")

	require.EqualError(t, err,
		`no camera named "Garage" in config.yaml; available cameras: "Front Door", "Driveway"`)
}

func TestStreamSchedulerLimitsDoNotExceedGoogleCeilings(t *testing.T) {
	assert.Less(t, streamGlobalQPM, googleGlobalQPMCeiling)
	assert.Less(t, streamDeviceQPH, googleDeviceQPHCeiling)
}

type failingConnectStreamManager struct{}

func (failingConnectStreamManager) SetSinkFactory(session.SinkFactory) {}

func (failingConnectStreamManager) SetRenewalObserver(session.RenewalObserver) {}

func (failingConnectStreamManager) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (failingConnectStreamManager) DrainRenewalObserver(context.Context) int { return 0 }
func (failingConnectStreamManager) Packets() uint64                          { return 0 }
func (failingConnectStreamManager) State() session.State                     { return session.StateBackoff }
func (failingConnectStreamManager) RenewalStats() session.RenewalStats       { return session.RenewalStats{} }
func (failingConnectStreamManager) SinkStats() session.SinkStats             { return session.SinkStats{} }
func (failingConnectStreamManager) ConnectionStats() session.ConnectionStats {
	return session.ConnectionStats{Failed: 3}
}

// deadSinkStreamManager models the failure this command used to report as a
// success: SDM is happy, RTP is arriving, renewals keep landing, and the RTSP
// sink has published nothing at all.
type deadSinkStreamManager struct {
	failingConnectStreamManager
}

func (deadSinkStreamManager) Packets() uint64 { return 5000 }
func (deadSinkStreamManager) State() session.State {
	return session.StateLive
}

func (deadSinkStreamManager) RenewalStats() session.RenewalStats {
	return session.RenewalStats{Succeeded: 3}
}

func (deadSinkStreamManager) ConnectionStats() session.ConnectionStats {
	return session.ConnectionStats{Succeeded: 1}
}

func (deadSinkStreamManager) SinkStats() session.SinkStats {
	return session.SinkStats{Discarded: 12, QueueDropped: 7, Failed: 4}
}

type healthySinkStreamManager struct {
	deadSinkStreamManager
}

func (healthySinkStreamManager) SinkStats() session.SinkStats {
	return session.SinkStats{Published: 4998, Discarded: 2}
}

type sinkFactoryStreamManager struct {
	failingConnectStreamManager
	sinkFactory session.SinkFactory
}

func (m *sinkFactoryStreamManager) SetSinkFactory(factory session.SinkFactory) {
	m.sinkFactory = factory
}

func TestRunStreamInstallsSinkFactory(t *testing.T) {
	camera := config.Camera{Name: "Front Door", DeviceID: "front"}
	manager := &sinkFactoryStreamManager{}

	err := runStreamWithManager(
		context.Background(),
		camera,
		config.MediaConfig{RTSPBaseURL: "rtsp://127.0.0.1:8554"},
		time.Millisecond,
		manager,
		io.Discard,
	)

	require.Error(t, err)
	require.NotNil(t, manager.sinkFactory)

	first, err := manager.sinkFactory(camera)
	require.NoError(t, err)
	second, err := manager.sinkFactory(camera)
	require.NoError(t, err)

	assert.IsType(t, &media.Publisher{}, first)
	assert.IsType(t, &media.Publisher{}, second)
	assert.NotSame(t, first, second)
}

func TestRunStreamSinkFactoryUsesConfiguredRTSPBaseURL(t *testing.T) {
	camera := config.Camera{Name: "Front Door", DeviceID: "front"}
	manager := &sinkFactoryStreamManager{}

	err := runStreamWithManager(
		context.Background(),
		camera,
		config.MediaConfig{RTSPBaseURL: "http://not-rtsp.example"},
		time.Millisecond,
		manager,
		io.Discard,
	)

	require.Error(t, err)
	require.NotNil(t, manager.sinkFactory)
	_, err = manager.sinkFactory(camera)
	require.EqualError(t, err, "invalid RTSP publish URL")
}

func TestRunStreamReportsFailureWhenConnectAlwaysFails(t *testing.T) {
	var out strings.Builder

	err := monitorStream(
		context.Background(),
		"Front Door",
		10*time.Millisecond,
		failingConnectStreamManager{},
		&out,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no live session established")
	assert.Contains(t, out.String(), "end_reason=duration elapsed without live session")
	assert.Contains(t, out.String(), "survived_full_duration=false")
	assert.Contains(t, out.String(), "connections_succeeded=0")
	assert.Contains(t, out.String(), "connections_failed=3")
}

// A run whose sink never published must not exit reporting success, however
// healthy the SDM half of the session looked.
func TestRunStreamReportsFailureWhenSinkNeverPublished(t *testing.T) {
	var out strings.Builder

	err := monitorStream(
		context.Background(),
		"Front Door",
		10*time.Millisecond,
		deadSinkStreamManager{},
		&out,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no media published to RTSP")
	assert.Contains(t, out.String(), "end_reason=duration elapsed without publishing media")
	assert.Contains(t, out.String(), "survived_full_duration=false")
	assert.Contains(t, out.String(), "sink_published=0")
	// Intentional discards and genuine queue loss must be separately visible;
	// on an audio-disabled camera the discards would otherwise bury the loss.
	assert.Contains(t, out.String(), "sink_discarded=12")
	assert.Contains(t, out.String(), "sink_queue_dropped=7")
	assert.Contains(t, out.String(), "sink_failed=4")
}

func TestRunStreamReportsSuccessWhenSinkPublished(t *testing.T) {
	var out strings.Builder

	err := monitorStream(
		context.Background(),
		"Front Door",
		10*time.Millisecond,
		healthySinkStreamManager{},
		&out,
	)

	require.NoError(t, err)
	assert.Contains(t, out.String(), "end_reason=duration elapsed")
	assert.Contains(t, out.String(), "survived_full_duration=true")
	assert.Contains(t, out.String(), "sink_published=4998")
}

func TestFormatStreamSummary(t *testing.T) {
	summary := streamSummary{
		Camera:               "Front Door",
		RequestedDuration:    time.Hour,
		Elapsed:              59*time.Minute + 59*time.Second,
		EndReason:            "duration elapsed",
		SurvivedFullDuration: true,
		Packets:              123456,
		ConnectionsSucceeded: 2,
		ConnectionsFailed:    1,
		RenewalsSucceeded:    19,
		RenewalsFailed:       1,
		UnsupportedFailures:  1,
		RateLimitFailures:    0,
		ObserverUndelivered:  2,
		SinkPublished:        123400,
		SinkDiscarded:        56,
		SinkQueueDropped:     9,
		SinkFailed:           0,
	}

	got := formatStreamSummary(summary)

	for _, line := range []string{
		`camera="Front Door"`,
		"requested_duration=1h0m0s",
		"elapsed=59m59s",
		"end_reason=duration elapsed",
		"survived_full_duration=true",
		"packets=123456",
		"connections_succeeded=2",
		"connections_failed=1",
		"renewals_succeeded=19",
		"renewals_failed=1",
		"unsupported_failures=1",
		"rate_limit_failures=0",
		"observer_events_undelivered=2",
		"sink_published=123400",
		"sink_discarded=56",
		"sink_queue_dropped=9",
		"sink_failed=0",
	} {
		assert.True(t, strings.Contains(got, line), "missing %q in:\n%s", line, got)
	}
}
