package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/ceb3/nest-to-ONVIF/internal/config"
	"github.com/ceb3/nest-to-ONVIF/internal/scheduler"
	"github.com/ceb3/nest-to-ONVIF/internal/sdm"
)

type State string

const (
	StateIdle       State = "idle"
	StateConnecting State = "connecting"
	StateLive       State = "live"
	StateRenewing   State = "renewing"
	StateBackoff    State = "backoff"
	StateFailed     State = "failed"
)

const (
	renewalFraction = 0.6
	maxRenewalDelay = 3 * time.Minute
	minRetryDelay   = time.Second
	maxBackoff      = 60 * time.Second
	stopTimeout     = 10 * time.Second
)

// StreamAPI is the subset of SDM stream commands needed by Manager.
type StreamAPI interface {
	Generate(ctx context.Context, deviceID, offerSDP string) (*sdm.StreamSession, error)
	Extend(ctx context.Context, deviceID, mediaSessionID string) (*sdm.StreamSession, error)
	Stop(ctx context.Context, deviceID, mediaSessionID string) error
}

// TrackInfo describes a remote track to a sink. It carries only what a sink
// needs, so that sinks stay testable without a live peer connection.
type TrackInfo struct {
	Kind     webrtc.RTPCodecType
	MimeType string
}

// TrackSink consumes RTP packets read from a remote track.
//
// WriteRTP may be called concurrently: pion dispatches one goroutine per
// remote track, and a camera with audio has two. Implementations must be
// safe for concurrent use.
//
// Implementations must keep WriteRTP safe after Close has returned. The
// manager wraps every sink so that writes starting after Close become no-ops,
// but that check and the call into the sink are not atomic: a write that
// passed the check can be descheduled and enter the sink afterwards, so it may
// run concurrently with Close and may still arrive once Close has completed.
// Close cannot wait it out, because closing is what interrupts a write stuck
// in an RTSP round trip. The usual way to satisfy this is to re-check a closed
// flag under the same lock Close holds, and to discard the write if it is set,
// which is what Publisher does.
type TrackSink interface {
	WriteRTP(info TrackInfo, pkt *rtp.Packet) error
}

// SinkCloser is implemented by sinks that hold resources. The manager closes
// the sink when the WebRTC connection ends.
type SinkCloser interface {
	Close() error
}

// ErrSinkFatal marks a sink error the sink cannot recover from, such as a dead
// downstream connection. The manager ends the WebRTC connection when it sees
// one, so the existing backoff and reconnect machinery runs. Any other sink
// error only stops the affected track pump: the remaining tracks, and the
// session itself, may still be healthy.
var ErrSinkFatal = errors.New("media sink is unusable")

// SinkStats reports how much media a sink handed downstream. Published is the
// count that matters to an operator: a session can look entirely healthy —
// packets arriving, renewals succeeding — while nothing at all is republished.
//
// Discarded and QueueDropped are kept apart deliberately. Discarded packets
// were never meant to be published, and on a camera with audio disabled that
// is every Opus packet, so a single combined counter would bury QueueDropped —
// the one form of genuine media loss the sink tolerates rather than failing on.
type SinkStats struct {
	Published    uint64
	Discarded    uint64
	QueueDropped uint64
	Failed       uint64
}

func (s SinkStats) add(other SinkStats) SinkStats {
	return SinkStats{
		Published:    s.Published + other.Published,
		Discarded:    s.Discarded + other.Discarded,
		QueueDropped: s.QueueDropped + other.QueueDropped,
		Failed:       s.Failed + other.Failed,
	}
}

// SinkReporter is implemented by sinks that can report their delivery counts.
// The manager accumulates them across connections.
type SinkReporter interface {
	SinkStats() SinkStats
}

// SinkFactory builds a sink for one WebRTC connection. It is called once per
// connection attempt, not once per renewal.
type SinkFactory func(cam config.Camera) (TrackSink, error)

// guardedSink makes a TrackSink safe to close while track pumps are still
// running. pion never joins its OnTrack goroutines, so a pump can outlive
// the peer connection that spawned it; without this, a closed RTSP session
// would receive writes.
//
// The closed flag is atomic rather than mutex-guarded on purpose. A reader
// lock held for the duration of each write would make Close wait for every
// write in flight, and a write can be in flight for as long as an RTSP announce
// takes. Closing the sink is precisely what cuts that announce short, so making
// Close wait for it would deadlock teardown against the round trip it is trying
// to abandon.
type guardedSink struct {
	closed    atomic.Bool
	closeOnce sync.Once
	inner     TrackSink
}

func (g *guardedSink) WriteRTP(info TrackInfo, pkt *rtp.Packet) error {
	if g.closed.Load() {
		return nil
	}
	return g.inner.WriteRTP(info, pkt)
}

// Close makes every later write a no-op and releases the inner sink. It does
// not drain: a write that had already passed the closed check may still be
// running, and may enter the inner sink even after Close has returned. Sinks
// are required to be safe for that, as documented on TrackSink.
func (g *guardedSink) Close() error {
	g.closed.Store(true)
	var err error
	g.closeOnce.Do(func() {
		if closer, ok := g.inner.(SinkCloser); ok {
			err = closer.Close()
		}
	})
	return err
}

func (g *guardedSink) stats() SinkStats {
	if reporter, ok := g.inner.(SinkReporter); ok {
		return reporter.SinkStats()
	}
	return SinkStats{}
}

type RenewalOutcome string

const (
	RenewalSucceeded RenewalOutcome = "succeeded"
	RenewalFailed    RenewalOutcome = "failed"
)

type RenewalFailure string

const (
	RenewalFailureNone        RenewalFailure = ""
	RenewalFailureOrdinary    RenewalFailure = "ordinary"
	RenewalFailureUnsupported RenewalFailure = "unsupported"
	RenewalFailureRateLimited RenewalFailure = "rate_limited"
)

// RenewalEvent reports only the timing and classified outcome of an extension attempt.
// It deliberately excludes session identifiers, SDP, credentials, and server messages.
type RenewalEvent struct {
	At      time.Time
	Outcome RenewalOutcome
	Failure RenewalFailure
}

type RenewalObserver func(RenewalEvent)

type RenewalStats struct {
	Succeeded   uint64
	Failed      uint64
	Unsupported uint64
	RateLimited uint64
}

type ConnectionStats struct {
	Succeeded uint64
	Failed    uint64
}

type Manager struct {
	cam   config.Camera
	api   StreamAPI
	sched *scheduler.Scheduler
	clock scheduler.Clock
	log   *slog.Logger

	newPeerConnection func(bool) (*webrtc.PeerConnection, error)

	mu          sync.RWMutex
	state       State
	sessID      string
	expires     time.Time
	failures    int
	packets     uint64
	connects    ConnectionStats
	renewals    RenewalStats
	sinkFactory SinkFactory
	sink        *guardedSink
	sinkStats   SinkStats

	observerMu        sync.Mutex
	observer          RenewalObserver
	observerQueue     []RenewalEvent
	observerPending   int
	observerDrained   chan struct{}
	observerRunning   bool
	observerInFlight  bool
	observerClosed    bool
	observerAbandoned bool
}

func NewManager(cam config.Camera, api StreamAPI, sched *scheduler.Scheduler, clock scheduler.Clock) *Manager {
	if clock == nil {
		clock = scheduler.NewRealClock()
	}
	return &Manager{
		cam:               cam,
		api:               api,
		sched:             sched,
		clock:             clock,
		log:               slog.Default().With("camera", cam.Name),
		state:             StateIdle,
		newPeerConnection: NewPeerConnection,
	}
}

func (m *Manager) State() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

func (m *Manager) Packets() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.packets
}

func (m *Manager) RenewalStats() RenewalStats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.renewals
}

func (m *Manager) ConnectionStats() ConnectionStats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.connects
}

// SinkStats reports sink delivery totals across every connection this manager
// has made, including the one currently running.
func (m *Manager) SinkStats() SinkStats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	total := m.sinkStats
	if m.sink != nil {
		total = total.add(m.sink.stats())
	}
	return total
}

func (m *Manager) SetSinkFactory(f SinkFactory) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sinkFactory = f
}

// SetRenewalObserver installs optional measurement instrumentation. Observer calls
// are serialized, run asynchronously, and are isolated from panics.
func (m *Manager) SetRenewalObserver(observer RenewalObserver) {
	m.observerMu.Lock()
	m.observer = observer
	m.observerMu.Unlock()
}

func (m *Manager) emitRenewal(event RenewalEvent) {
	m.observerMu.Lock()
	if m.observer == nil || m.observerClosed {
		m.observerMu.Unlock()
		return
	}
	if m.observerPending == 0 {
		m.observerDrained = make(chan struct{})
	}
	m.observerQueue = append(m.observerQueue, event)
	m.observerPending++
	if m.observerRunning {
		m.observerMu.Unlock()
		return
	}
	m.observerRunning = true
	m.observerMu.Unlock()

	go func() {
		for {
			m.observerMu.Lock()
			if m.observerAbandoned || len(m.observerQueue) == 0 {
				m.observerRunning = false
				m.observerMu.Unlock()
				return
			}
			event := m.observerQueue[0]
			m.observerQueue = m.observerQueue[1:]
			observer := m.observer
			m.observerInFlight = true
			m.observerMu.Unlock()

			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						m.log.Warn("renewal observer panicked")
					}
				}()
				observer(event)
			}()

			m.observerMu.Lock()
			m.observerInFlight = false
			m.observerPending--
			if m.observerPending == 0 {
				close(m.observerDrained)
			}
			m.observerMu.Unlock()
		}
	}()
}

// DrainRenewalObserver seals observer delivery and waits for queued callbacks.
// It returns the number still undelivered if ctx expires and abandons queued work.
func (m *Manager) DrainRenewalObserver(ctx context.Context) int {
	m.observerMu.Lock()
	m.observerClosed = true
	if m.observerPending == 0 {
		m.observerMu.Unlock()
		return 0
	}
	drained := m.observerDrained
	m.observerMu.Unlock()

	select {
	case <-drained:
		return 0
	case <-ctx.Done():
		m.observerMu.Lock()
		undelivered := m.observerPending
		if undelivered == 0 {
			m.observerMu.Unlock()
			return 0
		}
		m.observerAbandoned = true
		m.observerQueue = nil
		if m.observerInFlight {
			m.observerPending = 1
		} else {
			m.observerPending = 0
			close(m.observerDrained)
		}
		m.observerMu.Unlock()
		return undelivered
	}
}

func (m *Manager) setState(state State) {
	m.mu.Lock()
	m.state = state
	m.mu.Unlock()
	m.log.Info("session state changed", "state", state)
}

// RenewalDelay returns 60% of the session lifetime remaining at now.
func (m *Manager) RenewalDelay(expiresAt, now time.Time) time.Duration {
	if expiresAt.IsZero() {
		return 0
	}
	lifetime := expiresAt.Sub(now)
	if lifetime <= 0 {
		return 0
	}
	delay := time.Duration(float64(lifetime) * renewalFraction)
	if delay > maxRenewalDelay {
		return maxRenewalDelay
	}
	return delay
}

func (m *Manager) backoffFor(failures int) time.Duration {
	if failures < 0 {
		failures = 0
	}
	if failures >= 6 {
		return maxBackoff
	}
	delay := time.Second << failures
	if delay > maxBackoff {
		return maxBackoff
	}
	return delay
}

func (m *Manager) retryDelay(err error, failures int) time.Duration {
	var rateLimitErr *sdm.RateLimitError
	if errors.As(err, &rateLimitErr) && rateLimitErr.HasRetryAfter {
		if rateLimitErr.RetryAfter < minRetryDelay {
			return minRetryDelay
		}
		return rateLimitErr.RetryAfter
	}
	return m.backoffFor(failures)
}

// Run drives the camera session until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := m.runOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			m.mu.Lock()
			m.failures++
			failures := m.failures
			m.mu.Unlock()

			m.setState(StateFailed)
			m.setState(StateBackoff)
			if sleepErr := m.clock.Sleep(ctx, m.retryDelay(err, failures-1)); sleepErr != nil {
				return sleepErr
			}
			continue
		}

	}
}

func (m *Manager) runOnce(ctx context.Context) error {
	m.setState(StateConnecting)

	pc, err := m.newPeerConnection(m.cam.Audio)
	if err != nil {
		return err
	}

	var sink *guardedSink
	defer func() {
		_ = pc.Close()
		if sink != nil {
			_ = sink.Close()
			// Read the counts after closing, so every write the guard admitted
			// has been accounted for bar any still returning from the sink.
			m.mu.Lock()
			m.sinkStats = m.sinkStats.add(sink.stats())
			m.sink = nil
			m.mu.Unlock()
		}
	}()

	m.mu.RLock()
	factory := m.sinkFactory
	m.mu.RUnlock()
	if factory != nil {
		s, sinkErr := factory(m.cam)
		if sinkErr != nil {
			return fmt.Errorf("create media sink: %w", sinkErr)
		}
		if s == nil {
			return errors.New("create media sink: factory returned no sink")
		}
		sink = &guardedSink{inner: s}
		m.mu.Lock()
		m.sink = sink
		m.mu.Unlock()
	}

	connectionLost := make(chan struct{})
	var connectionLostOnce sync.Once
	loseConnection := func() { connectionLostOnce.Do(func() { close(connectionLost) }) }

	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		m.pumpTrack(
			TrackInfo{Kind: track.Kind(), MimeType: track.Codec().MimeType},
			trackRTPSource{track: track},
			sink,
			loseConnection,
		)
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed,
			webrtc.PeerConnectionStateDisconnected:
			loseConnection()
		}
	})

	offer, err := CreateOffer(ctx, pc)
	if err != nil {
		return err
	}

	var sess *sdm.StreamSession
	attempted := false
	err = m.sched.Do(ctx, m.cam.DeviceID, scheduler.PriorityConnect, func(commandCtx context.Context) error {
		attempted = true
		var generateErr error
		sess, generateErr = m.api.Generate(commandCtx, m.cam.DeviceID, offer)
		return generateErr
	})
	if err != nil {
		if attempted {
			m.recordConnection(false)
		}
		m.noteIfRateLimited(err)
		return err
	}
	if sess == nil {
		m.recordConnection(false)
		return errors.New("generate stream returned no session")
	}
	m.recordConnection(true)

	defer m.stopRemote(ctx, sess)

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sess.AnswerSDP,
	}); err != nil {
		// Pion parser errors are deliberately not propagated because they may
		// contain fragments of the answer SDP and its ICE credentials.
		return errors.New("apply remote description failed")
	}

	m.mu.Lock()
	m.sessID = sess.MediaSessionID
	m.expires = sess.ExpiresAt
	m.mu.Unlock()
	m.setState(StateLive)

	return m.renewLoop(ctx, sess, connectionLost)
}

// rtpSource is the part of a remote track the pump reads. Extracting it lets
// the pump's failure handling be tested without a live peer connection.
type rtpSource interface {
	ReadRTP() (*rtp.Packet, error)
}

type trackRTPSource struct {
	track *webrtc.TrackRemote
}

func (t trackRTPSource) ReadRTP() (*rtp.Packet, error) {
	pkt, _, err := t.track.ReadRTP()
	return pkt, err
}

// pumpTrack forwards one remote track to the sink until the track ends or the
// sink refuses the media. A fatal sink error ends the whole connection through
// loseConnection: without that, the session would stay live, keep renewing,
// keep spending SDM quota, and publish nothing.
func (m *Manager) pumpTrack(
	info TrackInfo,
	src rtpSource,
	sink *guardedSink,
	loseConnection func(),
) {
	for {
		pkt, readErr := src.ReadRTP()
		if readErr != nil {
			return
		}
		m.mu.Lock()
		m.packets++
		m.mu.Unlock()
		if sink == nil {
			continue
		}
		writeErr := sink.WriteRTP(info, pkt)
		if writeErr == nil {
			continue
		}
		if errors.Is(writeErr, ErrSinkFatal) {
			m.log.Error("media sink failed; ending connection",
				"kind", info.Kind, "error", writeErr)
			loseConnection()
			return
		}
		m.log.Error("sink write failed", "kind", info.Kind, "error", writeErr)
		return
	}
}

func (m *Manager) stopRemote(ctx context.Context, sess *sdm.StreamSession) {
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()

	err := m.sched.Do(stopCtx, m.cam.DeviceID, scheduler.PriorityConnect, func(commandCtx context.Context) error {
		return m.api.Stop(commandCtx, m.cam.DeviceID, sess.MediaSessionID)
	})
	if err != nil {
		m.noteIfRateLimited(err)
	}
}

func (m *Manager) renewLoop(
	ctx context.Context,
	sess *sdm.StreamSession,
	connectionLost <-chan struct{},
) error {
	for {
		delay := m.RenewalDelay(sess.ExpiresAt, m.clock.Now())
		if err := m.waitForRenewal(ctx, delay, connectionLost); err != nil {
			return err
		}

		m.setState(StateRenewing)
		var next *sdm.StreamSession
		attempted := false
		err := m.sched.Do(ctx, m.cam.DeviceID, scheduler.PriorityRenewal, func(commandCtx context.Context) error {
			attempted = true
			var extendErr error
			next, extendErr = m.api.Extend(commandCtx, m.cam.DeviceID, sess.MediaSessionID)
			return extendErr
		})
		if err != nil {
			m.noteIfRateLimited(err)
			if attempted {
				event := RenewalEvent{
					At:      m.clock.Now(),
					Outcome: RenewalFailed,
					Failure: classifyRenewalFailure(err),
				}
				m.recordRenewal(event)
				m.emitRenewal(event)
			}
			return err
		}
		if next == nil {
			err := errors.New("extend stream returned no session")
			event := RenewalEvent{
				At:      m.clock.Now(),
				Outcome: RenewalFailed,
				Failure: RenewalFailureOrdinary,
			}
			m.recordRenewal(event)
			m.emitRenewal(event)
			return err
		}

		sess.ExpiresAt = next.ExpiresAt
		if next.MediaSessionID != "" {
			sess.MediaSessionID = next.MediaSessionID
		}
		m.mu.Lock()
		m.sessID = sess.MediaSessionID
		m.expires = sess.ExpiresAt
		m.failures = 0
		m.mu.Unlock()
		event := RenewalEvent{
			At:      m.clock.Now(),
			Outcome: RenewalSucceeded,
			Failure: RenewalFailureNone,
		}
		m.recordRenewal(event)
		m.emitRenewal(event)
		m.setState(StateLive)
	}
}

func (m *Manager) recordRenewal(event RenewalEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if event.Outcome == RenewalSucceeded {
		m.renewals.Succeeded++
		return
	}
	m.renewals.Failed++
	switch event.Failure {
	case RenewalFailureUnsupported:
		m.renewals.Unsupported++
	case RenewalFailureRateLimited:
		m.renewals.RateLimited++
	}
}

func (m *Manager) recordConnection(succeeded bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if succeeded {
		m.connects.Succeeded++
		return
	}
	m.connects.Failed++
}

func classifyRenewalFailure(err error) RenewalFailure {
	switch {
	case errors.Is(err, sdm.ErrExtendUnsupported):
		return RenewalFailureUnsupported
	case errors.Is(err, sdm.ErrRateLimited):
		return RenewalFailureRateLimited
	default:
		return RenewalFailureOrdinary
	}
}

func (m *Manager) waitForRenewal(
	ctx context.Context,
	delay time.Duration,
	connectionLost <-chan struct{},
) error {
	sleepCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sleepDone := make(chan error, 1)
	go func() {
		sleepDone <- m.clock.Sleep(sleepCtx, delay)
	}()

	select {
	case err := <-sleepDone:
		return err
	case <-ctx.Done():
		cancel()
		<-sleepDone
		return ctx.Err()
	case <-connectionLost:
		cancel()
		<-sleepDone
		return errors.New("peer connection lost")
	}
}

func (m *Manager) noteIfRateLimited(err error) {
	if errors.Is(err, sdm.ErrRateLimited) {
		m.sched.NoteRateLimited()
		m.log.Warn("rate limited by Google; reducing command rate",
			"qpm", m.sched.CurrentQPM())
	}
}
