package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ceb3/nest-to-ONVIF/internal/config"
	"github.com/ceb3/nest-to-ONVIF/internal/scheduler"
	"github.com/ceb3/nest-to-ONVIF/internal/sdm"
)

func testManager() *Manager {
	return NewManager(config.Camera{DeviceID: "dev", Name: "Cam"}, nil, nil, nil)
}

// Renewing at 60% of the lifetime leaves room for one retry before expiry.
func TestRenewalDelayIsSixtyPercentOfLifetime(t *testing.T) {
	m := testManager()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expires := now.Add(5 * time.Minute)

	assert.Equal(t, 3*time.Minute, m.RenewalDelay(expires, now))
}

func TestRenewalDelayNeverNegative(t *testing.T) {
	m := testManager()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	assert.Zero(t, m.RenewalDelay(now.Add(-time.Minute), now))
}

func TestRenewalDelayHandlesMissingExpiry(t *testing.T) {
	m := testManager()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Google always sends expiresAt, but a zero value must not schedule a renewal
	// 1.2 million years in the past.
	assert.Zero(t, m.RenewalDelay(time.Time{}, now))
}

func TestRenewalDelayClampsMalformedDistantExpiry(t *testing.T) {
	m := testManager()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	assert.Equal(t, 3*time.Minute, m.RenewalDelay(now.Add(24*time.Hour), now))
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	m := testManager()
	assert.Equal(t, 1*time.Second, m.backoffFor(0))
	assert.Equal(t, 2*time.Second, m.backoffFor(1))
	assert.Equal(t, 4*time.Second, m.backoffFor(2))
	assert.Equal(t, 60*time.Second, m.backoffFor(20), "backoff must cap at 60s")
}

func TestInitialStateIsIdle(t *testing.T) {
	assert.Equal(t, StateIdle, testManager().State())
}

type advancingClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func (c *advancingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *advancingClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return nil
}

func (c *advancingClock) recordedSleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

type fakeStreamAPI struct {
	generate func(context.Context, string, string) (*sdm.StreamSession, error)
	extend   func(context.Context, string, string) (*sdm.StreamSession, error)
	stop     func(context.Context, string, string) error
}

type fakeSink struct {
	mu       sync.Mutex
	writeErr error
	closed   int
	writes   int
}

func (f *fakeSink) WriteRTP(TrackInfo, *rtp.Packet) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	return f.writeErr
}

func (f *fakeSink) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func (f *fakeSink) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeSink) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes
}

func (f *fakeStreamAPI) Generate(ctx context.Context, deviceID, offer string) (*sdm.StreamSession, error) {
	if f.generate == nil {
		panic("unexpected Generate call")
	}
	return f.generate(ctx, deviceID, offer)
}

func (f *fakeStreamAPI) Extend(ctx context.Context, deviceID, sessionID string) (*sdm.StreamSession, error) {
	if f.extend == nil {
		panic("unexpected Extend call")
	}
	return f.extend(ctx, deviceID, sessionID)
}

func (f *fakeStreamAPI) Stop(ctx context.Context, deviceID, sessionID string) error {
	if f.stop == nil {
		panic("unexpected Stop call")
	}
	return f.stop(ctx, deviceID, sessionID)
}

func answerOffer(offer string) (string, error) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return "", err
	}
	defer func() { _ = pc.Close() }()
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offer,
	}); err != nil {
		return "", err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return "", err
	}
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return "", err
	}
	<-gatherComplete
	return pc.LocalDescription().SDP, nil
}

func testScheduler(clock scheduler.Clock) *scheduler.Scheduler {
	return scheduler.NewScheduler(clock, scheduler.Options{GlobalQPM: 100, DeviceQPH: 100})
}

func TestRenewLoopSchedulesFromEachAbsoluteExpiry(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := &advancingClock{now: start}
	sched := scheduler.NewScheduler(clock, scheduler.Options{GlobalQPM: 8, DeviceQPH: 90})
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	api := &fakeStreamAPI{extend: func(context.Context, string, string) (*sdm.StreamSession, error) {
		calls++
		if calls == 2 {
			cancel()
		}
		return &sdm.StreamSession{
			MediaSessionID: "session",
			ExpiresAt:      clock.Now().Add(5 * time.Minute),
		}, nil
	}}
	m := NewManager(config.Camera{DeviceID: "dev", Name: "Cam"}, api, sched, clock)
	sess := &sdm.StreamSession{MediaSessionID: "session", ExpiresAt: start.Add(5 * time.Minute)}

	err := m.renewLoop(ctx, sess, make(chan struct{}))

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 2, calls)
	assert.Equal(t, []time.Duration{3 * time.Minute, 3 * time.Minute}, clock.recordedSleeps())
}

func TestRenewLoopRegeneratesAfterUnsupportedExtension(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := &advancingClock{now: start}
	ctx, cancel := context.WithCancel(context.Background())
	generateCalls := 0
	extendCalls := 0
	api := &fakeStreamAPI{
		generate: func(_ context.Context, _, offer string) (*sdm.StreamSession, error) {
			generateCalls++
			if generateCalls == 2 {
				cancel()
				return nil, context.Canceled
			}
			answer, err := answerOffer(offer)
			if err != nil {
				return nil, err
			}
			return &sdm.StreamSession{
				AnswerSDP:      answer,
				MediaSessionID: "session",
				ExpiresAt:      clock.Now().Add(5 * time.Minute),
			}, nil
		},
		extend: func(context.Context, string, string) (*sdm.StreamSession, error) {
			extendCalls++
			return nil, sdm.ErrExtendUnsupported
		},
		stop: func(context.Context, string, string) error { return nil },
	}
	m := NewManager(config.Camera{DeviceID: "dev", Name: "Cam"}, api, testScheduler(clock), clock)

	err := m.Run(ctx)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 2, generateCalls, "unsupported extension must regenerate")
	assert.Equal(t, 1, extendCalls, "unsupported extension must not retry Extend")
}

func TestRunResetsBackoffAfterSuccessfulRenewal(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := &advancingClock{now: start}
	ctx, cancel := context.WithCancel(context.Background())
	generateCalls := 0
	extendCalls := 0
	api := &fakeStreamAPI{
		generate: func(_ context.Context, _, offer string) (*sdm.StreamSession, error) {
			generateCalls++
			if generateCalls <= 2 {
				return nil, errors.New("connect failed")
			}
			if generateCalls == 4 {
				cancel()
				return nil, context.Canceled
			}
			answer, err := answerOffer(offer)
			if err != nil {
				return nil, err
			}
			return &sdm.StreamSession{
				AnswerSDP:      answer,
				MediaSessionID: "session",
				ExpiresAt:      clock.Now().Add(5 * time.Minute),
			}, nil
		},
		extend: func(context.Context, string, string) (*sdm.StreamSession, error) {
			extendCalls++
			if extendCalls == 1 {
				return &sdm.StreamSession{
					MediaSessionID: "renewed-session",
					ExpiresAt:      clock.Now().Add(5 * time.Minute),
				}, nil
			}
			return nil, errors.New("renewal failed")
		},
		stop: func(context.Context, string, string) error { return nil },
	}
	m := NewManager(config.Camera{DeviceID: "dev", Name: "Cam"}, api, testScheduler(clock), clock)

	err := m.Run(ctx)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, []time.Duration{
		time.Second,
		2 * time.Second,
		3 * time.Minute,
		3 * time.Minute,
		time.Second,
	}, clock.recordedSleeps(), "a successful renewal resets backoff")
}

func TestRunTracksFailedGenerateAttempts(t *testing.T) {
	clock := &advancingClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	ctx, cancel := context.WithCancel(context.Background())
	generateCalls := 0
	api := &fakeStreamAPI{
		generate: func(context.Context, string, string) (*sdm.StreamSession, error) {
			generateCalls++
			if generateCalls == 3 {
				cancel()
			}
			return nil, errors.New("connect failed")
		},
	}
	m := NewManager(config.Camera{DeviceID: "dev", Name: "Cam"}, api, testScheduler(clock), clock)

	err := m.Run(ctx)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, ConnectionStats{Failed: 3}, m.ConnectionStats())
}

type immediateDropClock struct {
	mu          sync.Mutex
	now         time.Time
	backoffs    []time.Duration
	closePeer   func()
	cancelAfter int
	cancel      context.CancelFunc
}

func (c *immediateDropClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *immediateDropClock) Sleep(ctx context.Context, d time.Duration) error {
	if d > maxBackoff {
		c.closePeer()
		<-ctx.Done()
		return ctx.Err()
	}

	c.mu.Lock()
	c.backoffs = append(c.backoffs, d)
	c.now = c.now.Add(d)
	count := len(c.backoffs)
	c.mu.Unlock()
	if count == c.cancelAfter {
		c.cancel()
		return ctx.Err()
	}
	return nil
}

func (c *immediateDropClock) recordedBackoffs() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.backoffs...)
}

func TestRunEscalatesBackoffAcrossImmediateLiveConnectionLosses(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	clock := &immediateDropClock{now: start, cancelAfter: 7, cancel: cancel}
	api := &fakeStreamAPI{
		generate: func(_ context.Context, _, offer string) (*sdm.StreamSession, error) {
			answer, err := answerOffer(offer)
			if err != nil {
				return nil, err
			}
			return &sdm.StreamSession{
				AnswerSDP:      answer,
				MediaSessionID: "session",
				ExpiresAt:      clock.Now().Add(5 * time.Minute),
			}, nil
		},
		extend: func(context.Context, string, string) (*sdm.StreamSession, error) {
			panic("immediate connection loss must prevent renewal")
		},
		stop: func(context.Context, string, string) error { return nil },
	}
	m := NewManager(config.Camera{DeviceID: "dev", Name: "Cam"}, api, testScheduler(clock), clock)
	var peerMu sync.Mutex
	var peer *webrtc.PeerConnection
	m.newPeerConnection = func(audio bool) (*webrtc.PeerConnection, error) {
		next, err := NewPeerConnection(audio)
		peerMu.Lock()
		peer = next
		peerMu.Unlock()
		return next, err
	}
	clock.closePeer = func() {
		peerMu.Lock()
		current := peer
		peerMu.Unlock()
		require.NoError(t, current.Close())
	}

	err := m.Run(ctx)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, []time.Duration{
		time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		32 * time.Second,
		60 * time.Second,
	}, clock.recordedBackoffs())
}

type blockingClock struct {
	now     time.Time
	started chan time.Duration
	wg      sync.WaitGroup
}

func (c *blockingClock) Now() time.Time { return c.now }

func (c *blockingClock) Sleep(ctx context.Context, d time.Duration) error {
	c.wg.Add(1)
	defer c.wg.Done()
	c.started <- d
	<-ctx.Done()
	return ctx.Err()
}

func TestRunCancellationStopsRemoteClosesPeerAndJoinsGoroutines(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := &blockingClock{now: start, started: make(chan time.Duration, 1)}
	stopCalled := make(chan struct{}, 1)
	api := &fakeStreamAPI{
		generate: func(_ context.Context, _, offer string) (*sdm.StreamSession, error) {
			answer, err := answerOffer(offer)
			if err != nil {
				return nil, err
			}
			return &sdm.StreamSession{
				AnswerSDP:      answer,
				MediaSessionID: "session",
				ExpiresAt:      start.Add(5 * time.Minute),
			}, nil
		},
		extend: func(context.Context, string, string) (*sdm.StreamSession, error) {
			panic("unexpected Extend call")
		},
		stop: func(context.Context, string, string) error {
			stopCalled <- struct{}{}
			return nil
		},
	}
	m := NewManager(config.Camera{DeviceID: "dev", Name: "Cam"}, api, testScheduler(clock), clock)
	var pc *webrtc.PeerConnection
	m.newPeerConnection = func(audio bool) (*webrtc.PeerConnection, error) {
		var err error
		pc, err = NewPeerConnection(audio)
		return pc, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- m.Run(ctx) }()

	assert.Equal(t, 3*time.Minute, <-clock.started)
	cancel()
	cancel()

	require.ErrorIs(t, <-runDone, context.Canceled)
	<-stopCalled
	clock.wg.Wait()
	require.NotNil(t, pc)
	assert.Equal(t, webrtc.PeerConnectionStateClosed, pc.ConnectionState())
}

// The factory is invoked once per connection attempt, and the sink it returns
// is closed exactly once when that connection ends.
func TestManagerCreatesAndClosesSinkPerConnection(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := &blockingClock{now: start, started: make(chan time.Duration, 1)}
	api := &fakeStreamAPI{
		generate: func(_ context.Context, _, offer string) (*sdm.StreamSession, error) {
			answer, err := answerOffer(offer)
			if err != nil {
				return nil, err
			}
			return &sdm.StreamSession{
				AnswerSDP:      answer,
				MediaSessionID: "session",
				ExpiresAt:      start.Add(5 * time.Minute),
			}, nil
		},
		extend: func(context.Context, string, string) (*sdm.StreamSession, error) {
			panic("unexpected Extend call")
		},
		stop: func(context.Context, string, string) error { return nil },
	}
	m := NewManager(config.Camera{DeviceID: "dev", Name: "Cam"}, api, testScheduler(clock), clock)
	sink := &fakeSink{}
	var factoryMu sync.Mutex
	factoryCalls := 0
	m.SetSinkFactory(func(config.Camera) (TrackSink, error) {
		factoryMu.Lock()
		defer factoryMu.Unlock()
		factoryCalls++
		return sink, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- m.Run(ctx) }()

	assert.Equal(t, 3*time.Minute, <-clock.started)
	cancel()

	require.ErrorIs(t, <-runDone, context.Canceled)
	clock.wg.Wait()
	factoryMu.Lock()
	assert.Equal(t, 1, factoryCalls)
	factoryMu.Unlock()
	assert.Equal(t, 1, sink.closeCount())
}

// A factory error aborts the connection attempt. The manager must not proceed
// to CreateOffer or spend an SDM command on a connection it cannot sink.
func TestManagerFactoryErrorFailsConnectionWithoutCallingGenerate(t *testing.T) {
	factoryErr := errors.New("sink unavailable")
	clock := &advancingClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	api := &fakeStreamAPI{}
	m := NewManager(config.Camera{DeviceID: "dev", Name: "Cam"}, api, testScheduler(clock), clock)
	m.SetSinkFactory(func(config.Camera) (TrackSink, error) {
		return nil, factoryErr
	})

	err := m.runOnce(context.Background())

	require.ErrorIs(t, err, factoryErr)
}

func TestManagerNilSinkFactoryResultFailsConnectionWithoutCallingGenerate(t *testing.T) {
	generateCalls := 0
	api := &fakeStreamAPI{
		generate: func(context.Context, string, string) (*sdm.StreamSession, error) {
			generateCalls++
			return nil, errors.New("unexpected Generate call")
		},
	}
	clock := &advancingClock{}
	m := NewManager(config.Camera{DeviceID: "dev", Name: "Cam"}, api, testScheduler(clock), clock)
	m.SetSinkFactory(func(config.Camera) (TrackSink, error) {
		return nil, nil
	})

	err := m.runOnce(context.Background())

	assert.EqualError(t, err, "create media sink: factory returned no sink")
	assert.Zero(t, generateCalls)
}

// Behaviour is unchanged when no factory is set, which is how the existing
// verification tooling runs.
func TestManagerWithoutSinkFactoryConnectsNormally(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := &blockingClock{now: start, started: make(chan time.Duration, 1)}
	api := &fakeStreamAPI{
		generate: func(_ context.Context, _, offer string) (*sdm.StreamSession, error) {
			answer, err := answerOffer(offer)
			if err != nil {
				return nil, err
			}
			return &sdm.StreamSession{
				AnswerSDP:      answer,
				MediaSessionID: "session",
				ExpiresAt:      start.Add(5 * time.Minute),
			}, nil
		},
		extend: func(context.Context, string, string) (*sdm.StreamSession, error) {
			panic("unexpected Extend call")
		},
		stop: func(context.Context, string, string) error { return nil },
	}
	m := NewManager(config.Camera{DeviceID: "dev", Name: "Cam"}, api, testScheduler(clock), clock)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- m.Run(ctx) }()

	assert.Equal(t, 3*time.Minute, <-clock.started)
	assert.Equal(t, StateLive, m.State())
	cancel()

	require.ErrorIs(t, <-runDone, context.Canceled)
	clock.wg.Wait()
}

// The V3 invariant: renewal extends the session in place, so the sink must
// survive it untouched.
func TestSinkSurvivesRenewalUntouched(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := &advancingClock{now: start}
	ctx, cancel := context.WithCancel(context.Background())
	sink := &fakeSink{}
	factoryCalls := 0
	closeCountAfterRenewal := -1
	extendCalls := 0
	api := &fakeStreamAPI{
		generate: func(_ context.Context, _, offer string) (*sdm.StreamSession, error) {
			answer, err := answerOffer(offer)
			if err != nil {
				return nil, err
			}
			return &sdm.StreamSession{
				AnswerSDP:      answer,
				MediaSessionID: "session",
				ExpiresAt:      start.Add(5 * time.Minute),
			}, nil
		},
		extend: func(context.Context, string, string) (*sdm.StreamSession, error) {
			extendCalls++
			if extendCalls == 1 {
				return &sdm.StreamSession{
					MediaSessionID: "renewed-session",
					ExpiresAt:      clock.Now().Add(5 * time.Minute),
				}, nil
			}
			closeCountAfterRenewal = sink.closeCount()
			cancel()
			return nil, context.Canceled
		},
		stop: func(context.Context, string, string) error { return nil },
	}
	m := NewManager(config.Camera{DeviceID: "dev", Name: "Cam"}, api, testScheduler(clock), clock)
	m.SetSinkFactory(func(config.Camera) (TrackSink, error) {
		factoryCalls++
		return sink, nil
	})

	err := m.Run(ctx)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, factoryCalls)
	assert.Equal(t, 0, closeCountAfterRenewal)
	assert.Equal(t, 1, sink.closeCount())
}

type reportingSink struct {
	fakeSink
	stats SinkStats
}

func (r *reportingSink) SinkStats() SinkStats { return r.stats }

// scriptedRTPSource yields a fixed number of packets and then ends the track.
type scriptedRTPSource struct {
	mu      sync.Mutex
	packets int
	reads   int
}

func (s *scriptedRTPSource) ReadRTP() (*rtp.Packet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.reads > s.packets {
		return nil, io.EOF
	}
	return &rtp.Packet{}, nil
}

func (s *scriptedRTPSource) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

var pumpTrackInfo = TrackInfo{Kind: webrtc.RTPCodecTypeVideo, MimeType: webrtc.MimeTypeH264}

// A fatal sink error must end the connection rather than only the pump.
// Without this the session stays live, keeps renewing, keeps spending SDM
// quota, and publishes nothing while reporting success.
func TestPumpTrackFatalSinkErrorEndsConnection(t *testing.T) {
	m := testManager()
	inner := &fakeSink{writeErr: fmt.Errorf("%w: RTSP session is gone", ErrSinkFatal)}
	src := &scriptedRTPSource{packets: 5}
	lost := make(chan struct{})
	var once sync.Once

	m.pumpTrack(pumpTrackInfo, src, &guardedSink{inner: inner},
		func() { once.Do(func() { close(lost) }) })

	select {
	case <-lost:
	default:
		t.Fatal("a fatal sink error left the connection running")
	}
	assert.Equal(t, 1, src.readCount(), "the pump must stop at the first fatal error")
	assert.Equal(t, uint64(1), m.Packets())
}

// An error that only affects one track leaves the connection alone: the other
// track, and the SDM session, may still be healthy.
func TestPumpTrackNonFatalSinkErrorStopsOnlyThePump(t *testing.T) {
	m := testManager()
	inner := &fakeSink{writeErr: errors.New("unsupported RTP codec")}
	src := &scriptedRTPSource{packets: 5}

	m.pumpTrack(pumpTrackInfo, src, &guardedSink{inner: inner},
		func() { t.Error("a recoverable sink error must not end the connection") })

	assert.Equal(t, 1, src.readCount())
}

func TestPumpTrackWithoutSinkCountsEveryPacket(t *testing.T) {
	m := testManager()
	src := &scriptedRTPSource{packets: 3}

	m.pumpTrack(pumpTrackInfo, src, nil,
		func() { t.Error("no sink means no sink failure") })

	assert.Equal(t, uint64(3), m.Packets())
}

// Signalling connectionLost is what actually ends the connection, so the
// existing backoff and reconnect machinery runs.
func TestRenewLoopEndsWhenConnectionIsLost(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := &blockingClock{now: start, started: make(chan time.Duration, 1)}
	api := &fakeStreamAPI{}
	m := NewManager(config.Camera{DeviceID: "dev", Name: "Cam"}, api, testScheduler(clock), clock)
	connectionLost := make(chan struct{})

	done := make(chan error, 1)
	go func() {
		done <- m.renewLoop(context.Background(), &sdm.StreamSession{
			MediaSessionID: "session",
			ExpiresAt:      start.Add(5 * time.Minute),
		}, connectionLost)
	}()

	assert.Equal(t, 3*time.Minute, <-clock.started)
	close(connectionLost)

	require.EqualError(t, <-done, "peer connection lost")
	clock.wg.Wait()
}

// Sink counters outlive the connection that produced them, so a run can be
// judged on whether media actually reached the RTSP server.
func TestManagerAccumulatesSinkStatsAcrossConnections(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := &blockingClock{now: start, started: make(chan time.Duration, 1)}
	api := &fakeStreamAPI{
		generate: func(_ context.Context, _, offer string) (*sdm.StreamSession, error) {
			answer, err := answerOffer(offer)
			if err != nil {
				return nil, err
			}
			return &sdm.StreamSession{
				AnswerSDP:      answer,
				MediaSessionID: "session",
				ExpiresAt:      start.Add(5 * time.Minute),
			}, nil
		},
		stop: func(context.Context, string, string) error { return nil },
	}
	m := NewManager(config.Camera{DeviceID: "dev", Name: "Cam"}, api, testScheduler(clock), clock)
	sink := &reportingSink{stats: SinkStats{Published: 7, Discarded: 2, QueueDropped: 3, Failed: 1}}
	m.SetSinkFactory(func(config.Camera) (TrackSink, error) { return sink, nil })

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- m.Run(ctx) }()

	assert.Equal(t, 3*time.Minute, <-clock.started)
	assert.Equal(t, SinkStats{Published: 7, Discarded: 2, QueueDropped: 3, Failed: 1}, m.SinkStats(),
		"a live connection's sink must be visible while it runs")
	cancel()

	require.ErrorIs(t, <-runDone, context.Canceled)
	clock.wg.Wait()
	assert.Equal(t, SinkStats{Published: 7, Discarded: 2, QueueDropped: 3, Failed: 1}, m.SinkStats())
}

func TestManagerSinkStatsAreZeroWithoutASink(t *testing.T) {
	assert.Equal(t, SinkStats{}, testManager().SinkStats())
}

// Writes arriving after Close are dropped rather than reaching a torn-down
// sink, and Close is idempotent.
func TestGuardedSinkDropsWritesAfterClose(t *testing.T) {
	inner := &fakeSink{}
	sink := &guardedSink{inner: inner}
	info := TrackInfo{Kind: webrtc.RTPCodecTypeVideo, MimeType: "video/H264"}
	pkt := &rtp.Packet{}

	require.NoError(t, sink.WriteRTP(info, pkt))
	require.NoError(t, sink.Close())
	require.NoError(t, sink.Close())
	require.NoError(t, sink.WriteRTP(info, pkt))

	assert.Equal(t, 1, inner.writeCount())
	assert.Equal(t, 1, inner.closeCount())
}

// blockUntilClosedSink reproduces the publisher's discipline: WriteRTP blocks
// on a network round trip that only Close can cut short.
type blockUntilClosedSink struct {
	mu        sync.Mutex
	writes    int
	started   chan struct{}
	released  chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

func (s *blockUntilClosedSink) WriteRTP(TrackInfo, *rtp.Packet) error {
	s.mu.Lock()
	s.writes++
	s.mu.Unlock()
	s.startOnce.Do(func() { close(s.started) })
	<-s.released
	return nil
}

func (s *blockUntilClosedSink) Close() error {
	s.closeOnce.Do(func() { close(s.released) })
	return nil
}

func (s *blockUntilClosedSink) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// Close must reach the inner sink while a write is still in flight. The
// publisher's Close is what interrupts an announce, and the guard is its only
// caller, so holding the write lock across it would make that interrupt
// unreachable and leave teardown waiting out the round trip.
//
// The media package pairs this with TestPublisherCloseInterruptsAnnounceRetry,
// which drives a real publisher through the same discipline.
func TestGuardedSinkCloseIsNotBlockedByInFlightWrite(t *testing.T) {
	inner := &blockUntilClosedSink{
		started:  make(chan struct{}),
		released: make(chan struct{}),
	}
	sink := &guardedSink{inner: inner}
	info := TrackInfo{Kind: webrtc.RTPCodecTypeVideo, MimeType: webrtc.MimeTypeH264}

	writeDone := make(chan error, 1)
	go func() { writeDone <- sink.WriteRTP(info, &rtp.Packet{}) }()
	select {
	case <-inner.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the write never started")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- sink.Close() }()

	select {
	case err := <-closeDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Close queued behind the in-flight write")
	}
	select {
	case err := <-writeDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not release the in-flight write")
	}

	// The guard's contract survives: a write starting after Close is a no-op
	// and never reaches the inner sink.
	require.NoError(t, sink.WriteRTP(info, &rtp.Packet{}))
	assert.Equal(t, 1, inner.writeCount())
}

func TestRetryDelayClampsNonPositiveRetryAfter(t *testing.T) {
	m := testManager()
	for _, retryAfter := range []time.Duration{0, -time.Second} {
		err := &sdm.RateLimitError{RetryAfter: retryAfter, HasRetryAfter: true}
		assert.Equal(t, time.Second, m.retryDelay(err, 4))
	}
}

func TestRetryDelayHonorsPositiveRetryAfter(t *testing.T) {
	m := testManager()
	err := &sdm.RateLimitError{RetryAfter: 17 * time.Second, HasRetryAfter: true}
	assert.Equal(t, 17*time.Second, m.retryDelay(err, 4))
	assert.Equal(t, 16*time.Second, m.retryDelay(errors.New("other"), 4))
}

func TestRateLimitNotifiesScheduler(t *testing.T) {
	clock := &advancingClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	sched := scheduler.NewScheduler(clock, scheduler.Options{GlobalQPM: 8, DeviceQPH: 90})
	m := NewManager(config.Camera{DeviceID: "dev", Name: "Cam"}, nil, sched, clock)

	m.noteIfRateLimited(sdm.ErrRateLimited)

	assert.Equal(t, 4, sched.CurrentQPM())
}

func TestRenewalObserverEmitsOneSuccess(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := &advancingClock{now: start}
	ctx, cancel := context.WithCancel(context.Background())
	api := &fakeStreamAPI{extend: func(context.Context, string, string) (*sdm.StreamSession, error) {
		cancel()
		return &sdm.StreamSession{
			MediaSessionID: "session",
			ExpiresAt:      clock.Now().Add(5 * time.Minute),
		}, nil
	}}
	m := NewManager(config.Camera{DeviceID: "dev", Name: "Cam"}, api, testScheduler(clock), clock)
	events := make(chan RenewalEvent, 2)
	m.SetRenewalObserver(func(event RenewalEvent) { events <- event })

	err := m.renewLoop(ctx, &sdm.StreamSession{
		MediaSessionID: "session",
		ExpiresAt:      start.Add(5 * time.Minute),
	}, make(chan struct{}))

	require.ErrorIs(t, err, context.Canceled)
	event := <-events
	assert.Equal(t, RenewalSucceeded, event.Outcome)
	assert.Equal(t, RenewalFailureNone, event.Failure)
	assert.Equal(t, start.Add(3*time.Minute), event.At)
	assert.Equal(t, RenewalStats{Succeeded: 1}, m.RenewalStats())
	select {
	case extra := <-events:
		t.Fatalf("unexpected extra renewal event: %+v", extra)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestRenewalObserverClassifiesFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want RenewalFailure
	}{
		{name: "ordinary", err: errors.New("extend failed"), want: RenewalFailureOrdinary},
		{name: "unsupported", err: sdm.ErrExtendUnsupported, want: RenewalFailureUnsupported},
		{name: "rate limited", err: sdm.ErrRateLimited, want: RenewalFailureRateLimited},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			clock := &advancingClock{now: start}
			api := &fakeStreamAPI{extend: func(context.Context, string, string) (*sdm.StreamSession, error) {
				return nil, tt.err
			}}
			m := NewManager(config.Camera{DeviceID: "dev", Name: "Cam"}, api, testScheduler(clock), clock)
			events := make(chan RenewalEvent, 1)
			m.SetRenewalObserver(func(event RenewalEvent) { events <- event })

			err := m.renewLoop(context.Background(), &sdm.StreamSession{
				MediaSessionID: "session",
				ExpiresAt:      start.Add(5 * time.Minute),
			}, make(chan struct{}))

			require.ErrorIs(t, err, tt.err)
			event := <-events
			assert.Equal(t, RenewalFailed, event.Outcome)
			assert.Equal(t, tt.want, event.Failure)
		})
	}
}

func TestNilAndPanickingRenewalObserversAreSafe(t *testing.T) {
	m := testManager()
	assert.NotPanics(t, func() {
		m.SetRenewalObserver(nil)
		m.emitRenewal(RenewalEvent{Outcome: RenewalSucceeded})
	})

	done := make(chan struct{})
	m.SetRenewalObserver(func(RenewalEvent) {
		defer close(done)
		panic("observer bug")
	})
	assert.NotPanics(t, func() {
		m.emitRenewal(RenewalEvent{Outcome: RenewalSucceeded})
	})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("panicking observer was not invoked")
	}
}

func TestSlowRenewalObserverDoesNotDelayStateMachine(t *testing.T) {
	m := testManager()
	started := make(chan struct{})
	release := make(chan struct{})
	m.SetRenewalObserver(func(RenewalEvent) {
		close(started)
		<-release
	})

	returned := make(chan struct{})
	go func() {
		m.emitRenewal(RenewalEvent{Outcome: RenewalSucceeded})
		close(returned)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("observer was not invoked")
	}
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("slow observer delayed event producer")
	}
	close(release)
}

func TestRenewalObserverDrainOrdersEventsBeforeSummary(t *testing.T) {
	m := testManager()
	var got []string
	m.SetRenewalObserver(func(event RenewalEvent) {
		got = append(got, string(event.Outcome))
	})

	m.emitRenewal(RenewalEvent{Outcome: RenewalSucceeded})
	m.emitRenewal(RenewalEvent{Outcome: RenewalFailed})
	m.emitRenewal(RenewalEvent{Outcome: RenewalSucceeded})

	assert.Zero(t, m.DrainRenewalObserver(context.Background()))
	got = append(got, "summary")
	assert.Equal(t, []string{"succeeded", "failed", "succeeded", "summary"}, got)
}

func TestRenewalObserverDrainBoundedWhenCallbackBlocks(t *testing.T) {
	m := testManager()
	started := make(chan struct{})
	release := make(chan struct{})
	m.SetRenewalObserver(func(RenewalEvent) {
		close(started)
		<-release
	})
	m.emitRenewal(RenewalEvent{Outcome: RenewalSucceeded})

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("observer was not invoked")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.Equal(t, 1, m.DrainRenewalObserver(ctx))
	close(release)
}
