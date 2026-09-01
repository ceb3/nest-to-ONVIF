package events

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/mustacheride/nest-to-ONVIF/internal/config"
	"github.com/mustacheride/nest-to-ONVIF/internal/scheduler"
)

// DefaultLinger applies to any camera that configures none. A window shorter
// than Protect's own polling interval of roughly a second would risk the edge
// pair being pulled as a single no-op.
const DefaultLinger = 60 * time.Second

// DefaultMaxDuration bounds how long the level may stay up however many
// detections arrive. Nest reports detections, never their end, so without this
// a camera watching a busy street would hold motion on for the process's
// lifetime and every marker after the first would be lost inside one window.
const DefaultMaxDuration = 5 * time.Minute

const (
	defaultSweepInterval  = time.Second
	defaultTriggerTimeout = 3 * time.Second
)

// Edge is a change in a camera's motion level, the only thing worth delivering.
//
// Kind and ThreadID describe the detection that raised the level and are set on
// rising edges only: a level falls because nothing more arrived, so there is no
// detection to attribute it to.
type Edge struct {
	Camera   config.Camera
	On       bool
	At       time.Time
	Kind     Kind
	ThreadID string
	// DetectedAt is the time Google reported, as opposed to At, which is when
	// this process saw it.
	DetectedAt time.Time
}

// Tracker converts detections into a per-camera motion level. Detections are
// reference-counted rather than pulsed: Nest emits one message per detection
// and Protect wants one window per burst.
type Tracker struct {
	clock scheduler.Clock
	emit  func(Edge)

	// MaxDuration is the failsafe ceiling on a single motion window.
	MaxDuration time.Duration
	// SweepInterval is how often Run checks for expired holds. It bounds how
	// late an off edge can be, so it must stay well under DefaultLinger.
	SweepInterval time.Duration

	Logger *slog.Logger

	mu    sync.Mutex
	state map[string]*camState
}

type camState struct {
	cam    config.Camera
	linger time.Duration

	holds    int
	clearAt  time.Time
	on       bool
	raisedAt time.Time
}

// NewTracker builds a tracker for cameras. Events naming a device outside
// cameras are dropped, so the set here is also the filter.
func NewTracker(cameras []config.Camera, emit func(Edge), clock scheduler.Clock) *Tracker {
	state := make(map[string]*camState, len(cameras))
	for _, cam := range cameras {
		linger := cam.Linger
		if linger <= 0 {
			linger = DefaultLinger
		}
		state[cam.DeviceID] = &camState{cam: cam, linger: linger}
	}
	return &Tracker{
		clock:         clock,
		emit:          emit,
		MaxDuration:   DefaultMaxDuration,
		SweepInterval: defaultSweepInterval,
		state:         state,
	}
}

func (t *Tracker) log() *slog.Logger {
	if t.Logger == nil {
		return slog.Default()
	}
	return t.Logger
}

// Handle takes one detection. It satisfies Handler and never blocks on delivery,
// because it runs on the subscriber's loop and a stalled camera must not stop
// acknowledgement of messages for the others.
func (t *Tracker) Handle(e Event) {
	now := t.clock.Now()

	t.mu.Lock()
	st, ok := t.state[e.DeviceID]
	if !ok {
		t.mu.Unlock()
		return
	}
	st.holds++
	// The most recent hold always has the latest deadline, so tracking the
	// maximum is equivalent to tracking every hold individually.
	if deadline := now.Add(st.linger); deadline.After(st.clearAt) {
		st.clearAt = deadline
	}
	var edge *Edge
	if !st.on {
		st.on = true
		st.raisedAt = now
		edge = &Edge{Camera: st.cam, On: true, At: now,
			Kind: e.Kind, ThreadID: e.ThreadID, DetectedAt: e.At}
	}
	holds := st.holds
	cam := st.cam
	t.mu.Unlock()

	if edge != nil {
		t.log().Info("motion level up", "camera", cam.Name, "kind", e.Kind)
		t.emit(*edge)
		return
	}
	t.log().Debug("motion held", "camera", cam.Name, "kind", e.Kind, "holds", holds)
}

// Sweep lowers any level whose holds have expired, or whose window has hit the
// failsafe ceiling.
func (t *Tracker) Sweep(now time.Time) {
	var edges []Edge
	var failsafe []string

	t.mu.Lock()
	for _, st := range t.state {
		if !st.on {
			continue
		}
		expired := !now.Before(st.clearAt)
		wedged := now.Sub(st.raisedAt) >= t.MaxDuration
		if !expired && !wedged {
			continue
		}
		if wedged && !expired {
			failsafe = append(failsafe, st.cam.Name)
		}
		st.on = false
		st.holds = 0
		st.clearAt = time.Time{}
		edges = append(edges, Edge{Camera: st.cam, On: false, At: now})
	}
	t.mu.Unlock()

	for _, name := range failsafe {
		t.log().Warn("motion level forced down by failsafe", "camera", name, "after", t.MaxDuration)
	}
	for _, e := range edges {
		t.log().Info("motion level down", "camera", e.Camera.Name)
		t.emit(e)
	}
}

// Run sweeps until ctx is cancelled. It does not report an error: a tracker
// cannot fail, and streaming must survive anything that happens here.
func (t *Tracker) Run(ctx context.Context) {
	interval := t.SweepInterval
	if interval <= 0 {
		interval = defaultSweepInterval
	}
	for {
		if err := t.clock.Sleep(ctx, interval); err != nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		t.Sweep(t.clock.Now())
	}
}

// DeliverFunc delivers one edge. It is expected to be slow or to fail, so it is
// only ever called from the owning camera's dispatcher goroutine.
type DeliverFunc func(context.Context, Edge) error

// Dispatcher serialises delivery per camera and isolates cameras from each
// other. A wedged camera holds up only its own queue.
type Dispatcher struct {
	deliver DeliverFunc
	Logger  *slog.Logger

	stopCtx context.Context
	stop    context.CancelFunc
	wg      sync.WaitGroup

	mu     sync.Mutex
	queues map[string]chan Edge
}

func NewDispatcher(deliver DeliverFunc) *Dispatcher {
	ctx, cancel := context.WithCancel(context.Background())
	return &Dispatcher{
		deliver: deliver,
		stopCtx: ctx,
		stop:    cancel,
		queues:  map[string]chan Edge{},
	}
}

func (d *Dispatcher) log() *slog.Logger {
	if d.Logger == nil {
		return slog.Default()
	}
	return d.Logger
}

// Send queues an edge without blocking. A queue that is already full is a
// camera not keeping up, and the newer edge is the one worth keeping: it is the
// current state, whereas the queued one is already wrong.
func (d *Dispatcher) Send(e Edge) {
	q := d.queueFor(e.Camera.Name)
	select {
	case q <- e:
		return
	default:
	}
	select {
	case dropped := <-q:
		d.log().Warn("dropping superseded motion edge",
			"camera", e.Camera.Name, "state", dropped.On)
	default:
	}
	select {
	case q <- e:
	default:
		d.log().Warn("motion edge dropped; camera queue busy",
			"camera", e.Camera.Name, "state", e.On)
	}
}

func (d *Dispatcher) queueFor(name string) chan Edge {
	d.mu.Lock()
	defer d.mu.Unlock()
	if q, ok := d.queues[name]; ok {
		return q
	}
	q := make(chan Edge, 1)
	d.queues[name] = q
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for {
			select {
			case <-d.stopCtx.Done():
				return
			case e := <-q:
				if err := d.deliver(d.stopCtx, e); err != nil {
					// Dropped, never retried: the event is stale within seconds
					// and a retry loop against a dead camera buys nothing.
					d.log().Warn("motion delivery failed",
						"camera", e.Camera.Name, "state", e.On, "error", err)
				}
			}
		}
	}()
	return q
}

// Run blocks until ctx is cancelled, then stops every camera's delivery
// goroutine.
func (d *Dispatcher) Run(ctx context.Context) {
	<-ctx.Done()
	d.stop()
	d.wg.Wait()
}

// Trigger posts edges to a camera's ONVIF Events trigger endpoint, which is
// served by the patched onvif container on that camera's own IP.
type Trigger struct {
	http    *http.Client
	baseURL string
	Logger  *slog.Logger
}

type TriggerOption func(*Trigger)

// WithTriggerBaseURL overrides the per-camera address. For tests only: in
// production the address is the camera's identity.
func WithTriggerBaseURL(u string) TriggerOption { return func(t *Trigger) { t.baseURL = u } }

func WithTriggerHTTPClient(h *http.Client) TriggerOption {
	return func(t *Trigger) { t.http = h }
}

func NewTrigger(opts ...TriggerOption) *Trigger {
	t := &Trigger{http: &http.Client{Timeout: defaultTriggerTimeout}}
	for _, o := range opts {
		o(t)
	}
	return t
}

func (t *Trigger) Deliver(ctx context.Context, e Edge) error {
	state := "off"
	if e.On {
		state = "on"
	}
	base := t.baseURL
	if base == "" {
		base = "http://" + e.Camera.ONVIF.IP
	}
	reqURL := base + "/trigger/motion?" + url.Values{"state": {state}}.Encode()

	ctx, cancel := context.WithTimeout(ctx, defaultTriggerTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, nil)
	if err != nil {
		return fmt.Errorf("build trigger request: %w", err)
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return fmt.Errorf("post trigger: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("trigger returned HTTP %d", resp.StatusCode)
	}
	return nil
}
