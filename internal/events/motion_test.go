package events

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ceb3/nest-to-ONVIF/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeClock advances only when the test says so, which is what makes the edge
// assertions deterministic; the same shape the scheduler tests use.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 8, 30, 20, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.advance(d)
	return nil
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

const (
	deviceLingering = "enterprises/REDACTED-ENTERPRISE/devices/REDACTED-DEVICE-2"
	deviceDoorbell  = "enterprises/REDACTED-ENTERPRISE/devices/REDACTED-DEVICE-1"
)

func testCameras() []config.Camera {
	return []config.Camera{
		{
			DeviceID: deviceDoorbell,
			Name:     "Front doorbell",
			ONVIF:    config.ONVIFConfig{IP: "192.0.2.8"},
		},
		{
			DeviceID: deviceLingering,
			Name:     "Driveway",
			Linger:   30 * time.Second,
			ONVIF:    config.ONVIFConfig{IP: "192.0.2.9"},
		},
	}
}

type edgeRecorder struct {
	mu    sync.Mutex
	edges []Edge
}

func (r *edgeRecorder) emit(e Edge) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.edges = append(r.edges, e)
}

func (r *edgeRecorder) all() []Edge {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Edge(nil), r.edges...)
}

func (r *edgeRecorder) states() []bool {
	edges := r.all()
	out := make([]bool, 0, len(edges))
	for _, e := range edges {
		out = append(out, e.On)
	}
	return out
}

func newTestTracker(t *testing.T, rec *edgeRecorder, clock *fakeClock) *Tracker {
	t.Helper()
	tr := NewTracker(testCameras(), rec.emit, clock)
	tr.Logger = testLogger()
	return tr
}

func TestTrackerRaisesOnFirstDetection(t *testing.T) {
	rec := &edgeRecorder{}
	clock := newFakeClock()
	tr := newTestTracker(t, rec, clock)

	tr.Handle(Event{DeviceID: deviceLingering, Kind: KindMotion, At: clock.Now()})

	require.Len(t, rec.edges, 1)
	assert.True(t, rec.edges[0].On)
	assert.Equal(t, "Driveway", rec.edges[0].Camera.Name)
}

// The behaviour Protect cares about: a burst must collapse into one motion
// window rather than a marker per Pub/Sub message.
func TestTrackerBurstProducesExactlyOneOnAndOneOffEdge(t *testing.T) {
	rec := &edgeRecorder{}
	clock := newFakeClock()
	tr := newTestTracker(t, rec, clock)

	for i := 0; i < 10; i++ {
		tr.Handle(Event{DeviceID: deviceLingering, Kind: KindMotion, At: clock.Now()})
		clock.advance(100 * time.Millisecond)
		tr.Sweep(clock.Now())
	}
	assert.Equal(t, []bool{true}, rec.states())

	// The last hold was taken at t+0.9s, so the level may only fall 30s after
	// that, not 30s after the first.
	clock.advance(29 * time.Second)
	tr.Sweep(clock.Now())
	assert.Equal(t, []bool{true}, rec.states(), "level fell before the last hold cleared")

	clock.advance(time.Second)
	tr.Sweep(clock.Now())
	assert.Equal(t, []bool{true, false}, rec.states())
}

func TestTrackerHoldsLevelForFullLinger(t *testing.T) {
	rec := &edgeRecorder{}
	clock := newFakeClock()
	tr := newTestTracker(t, rec, clock)

	tr.Handle(Event{DeviceID: deviceLingering, Kind: KindMotion, At: clock.Now()})
	clock.advance(29 * time.Second)
	tr.Sweep(clock.Now())
	assert.Equal(t, []bool{true}, rec.states())

	clock.advance(time.Second)
	tr.Sweep(clock.Now())
	assert.Equal(t, []bool{true, false}, rec.states())
}

// Cameras without an explicit event.linger still get the default motion window.
func TestTrackerAppliesDefaultLingerToCameraWithNoneConfigured(t *testing.T) {
	rec := &edgeRecorder{}
	clock := newFakeClock()
	tr := newTestTracker(t, rec, clock)

	tr.Handle(Event{DeviceID: deviceDoorbell, Kind: KindChime, At: clock.Now()})
	require.Equal(t, []bool{true}, rec.states())

	clock.advance(DefaultLinger - time.Second)
	tr.Sweep(clock.Now())
	assert.Equal(t, []bool{true}, rec.states())

	clock.advance(time.Second)
	tr.Sweep(clock.Now())
	assert.Equal(t, []bool{true, false}, rec.states())
}

// Continuous detections would otherwise renew the level forever; the failsafe
// bounds it so a stuck camera cannot show motion indefinitely.
func TestTrackerFailsafeForcesLevelDown(t *testing.T) {
	rec := &edgeRecorder{}
	clock := newFakeClock()
	tr := newTestTracker(t, rec, clock)
	tr.MaxDuration = 2 * time.Minute

	for elapsed := time.Duration(0); elapsed < 5*time.Minute; elapsed += 10 * time.Second {
		tr.Handle(Event{DeviceID: deviceLingering, Kind: KindMotion, At: clock.Now()})
		clock.advance(10 * time.Second)
		tr.Sweep(clock.Now())
	}

	require.Equal(t, []bool{true, false}, rec.states()[:2])
	assert.Equal(t, 2*time.Minute, rec.edges[1].At.Sub(rec.edges[0].At))
}

// After the failsafe fires, a later detection must still raise the level again.
func TestTrackerRearmsAfterFailsafe(t *testing.T) {
	rec := &edgeRecorder{}
	clock := newFakeClock()
	tr := newTestTracker(t, rec, clock)
	tr.MaxDuration = time.Minute

	tr.Handle(Event{DeviceID: deviceLingering, Kind: KindMotion, At: clock.Now()})
	clock.advance(time.Minute)
	tr.Sweep(clock.Now())
	require.Equal(t, []bool{true, false}, rec.states())

	clock.advance(time.Second)
	tr.Handle(Event{DeviceID: deviceLingering, Kind: KindMotion, At: clock.Now()})
	assert.Equal(t, []bool{true, false, true}, rec.states())
}

func TestTrackerIgnoresUnconfiguredDevice(t *testing.T) {
	rec := &edgeRecorder{}
	clock := newFakeClock()
	tr := newTestTracker(t, rec, clock)

	tr.Handle(Event{DeviceID: "enterprises/REDACTED-ENTERPRISE/devices/REDACTED-DEVICE-OUT-OF-SCOPE",
		Kind: KindMotion, At: clock.Now()})

	assert.Empty(t, rec.edges)
}

func TestTrackerCamerasAreIndependent(t *testing.T) {
	rec := &edgeRecorder{}
	clock := newFakeClock()
	tr := newTestTracker(t, rec, clock)

	tr.Handle(Event{DeviceID: deviceLingering, Kind: KindMotion, At: clock.Now()})
	tr.Handle(Event{DeviceID: deviceDoorbell, Kind: KindPerson, At: clock.Now()})
	require.Len(t, rec.edges, 2)

	clock.advance(31 * time.Second)
	tr.Sweep(clock.Now())
	require.Len(t, rec.edges, 3)
	assert.Equal(t, "Driveway", rec.edges[2].Camera.Name)
	assert.False(t, rec.edges[2].On)
}

func TestTrackerRunSweepsUntilCancelled(t *testing.T) {
	rec := &edgeRecorder{}
	clock := newFakeClock()
	tr := newTestTracker(t, rec, clock)
	tr.SweepInterval = time.Second

	tr.Handle(Event{DeviceID: deviceLingering, Kind: KindMotion, At: clock.Now()})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); tr.Run(ctx) }()

	assert.Eventually(t, func() bool {
		return len(rec.states()) == 2
	}, 2*time.Second, time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return on cancellation")
	}
}

// Delivery

type triggerStub struct {
	mu      sync.Mutex
	states  []string
	paths   []string
	release chan struct{}
}

func (s *triggerStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.release != nil {
			<-s.release
		}
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.Path)
		s.states = append(s.states, r.URL.Query().Get("state"))
		s.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *triggerStub) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.states...)
}

func TestTriggerPostsMotionState(t *testing.T) {
	stub := &triggerStub{}
	srv := stub.server(t)

	tr := NewTrigger(WithTriggerHTTPClient(srv.Client()), WithTriggerBaseURL(srv.URL))
	tr.Logger = testLogger()

	cam := config.Camera{Name: "Driveway", ONVIF: config.ONVIFConfig{IP: "192.0.2.9"}}
	require.NoError(t, tr.Deliver(context.Background(), Edge{Camera: cam, On: true}))
	require.NoError(t, tr.Deliver(context.Background(), Edge{Camera: cam, On: false}))

	assert.Equal(t, []string{"on", "off"}, stub.seen())
	assert.Equal(t, []string{"/trigger/motion", "/trigger/motion"}, stub.paths)
}

func TestTriggerReportsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	tr := NewTrigger(WithTriggerHTTPClient(srv.Client()), WithTriggerBaseURL(srv.URL))
	tr.Logger = testLogger()

	err := tr.Deliver(context.Background(), Edge{Camera: config.Camera{Name: "Driveway",
		ONVIF: config.ONVIFConfig{IP: "192.0.2.9"}}, On: true})
	require.Error(t, err)
}

// A camera that never answers must not hold up the other three, which is why
// each camera gets its own delivery goroutine.
func TestDispatcherWedgedCameraDoesNotBlockOthers(t *testing.T) {
	slow := &triggerStub{release: make(chan struct{})}
	slowSrv := slow.server(t)
	fast := &triggerStub{}
	fastSrv := fast.server(t)

	delivered := make(chan string, 8)
	d := NewDispatcher(func(ctx context.Context, e Edge) error {
		base := fastSrv.URL
		if e.Camera.Name == "Wedged" {
			base = slowSrv.URL
		}
		tr := NewTrigger(WithTriggerHTTPClient(http.DefaultClient), WithTriggerBaseURL(base))
		tr.Logger = testLogger()
		err := tr.Deliver(ctx, e)
		delivered <- e.Camera.Name
		return err
	})
	d.Logger = testLogger()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	d.Send(Edge{Camera: config.Camera{Name: "Wedged"}, On: true})
	d.Send(Edge{Camera: config.Camera{Name: "Healthy"}, On: true})

	select {
	case name := <-delivered:
		assert.Equal(t, "Healthy", name)
	case <-time.After(2 * time.Second):
		t.Fatal("healthy camera was blocked by the wedged one")
	}
	close(slow.release)
}
