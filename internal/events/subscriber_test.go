package events

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testDeviceOne = "enterprises/REDACTED-ENTERPRISE/devices/REDACTED-DEVICE-1"
	testDeviceTwo = "enterprises/REDACTED-ENTERPRISE/devices/REDACTED-DEVICE-2"
)

func payload(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return raw
}

func TestDecodeMotionEvent(t *testing.T) {
	got, err := decode(payload(t, "motion.json"))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, testDeviceOne, got[0].DeviceID)
	assert.Equal(t, KindMotion, got[0].Kind)
	assert.Equal(t, "2026-08-30T20:34:01.123Z", got[0].At.UTC().Format(time.RFC3339Nano))
}

func TestDecodePersonEvent(t *testing.T) {
	got, err := decode(payload(t, "person.json"))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, testDeviceTwo, got[0].DeviceID)
	assert.Equal(t, KindPerson, got[0].Kind)
}

// The chime fixture also carries a ClipPreview event, which is exactly the
// mixed-envelope shape a doorbell press produces.
func TestDecodeChimeEventIgnoresCompanionEvents(t *testing.T) {
	got, err := decode(payload(t, "chime.json"))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, KindChime, got[0].Kind)
}

func TestDecodeDropsUnknownAndTraitUpdates(t *testing.T) {
	for _, name := range []string{"unknown.json", "trait_update.json"} {
		got, err := decode(payload(t, name))
		assert.NoError(t, err, name)
		assert.Empty(t, got, name)
	}
}

func TestDecodeMalformedJSONErrors(t *testing.T) {
	_, err := decode(payload(t, "malformed.json"))
	require.Error(t, err)
}

// A trait update carries no detection, so it produces no Event and nothing is
// logged about it above debug. That makes "this device sends updates but never
// detections" look identical to "this device sends nothing at all", which are
// different faults with different fixes. summarize exists to tell them apart.
func TestSummarizeReportsTheDeviceAndRawEventNames(t *testing.T) {
	device, names := summarize(payload(t, "trait_update.json"))
	assert.Equal(t, testDeviceOne, device)
	assert.Empty(t, names, "a trait update carries no events")

	device, names = summarize(payload(t, "motion.json"))
	assert.Equal(t, testDeviceOne, device)
	assert.Equal(t, []string{"sdm.devices.events.CameraMotion.Motion"}, names)
}

// Names are sorted so that a multi-event envelope logs reproducibly rather than
// in Go's randomised map order.
func TestSummarizeSortsNames(t *testing.T) {
	_, names := summarize(payload(t, "chime.json"))
	assert.Equal(t, sort.StringsAreSorted(names), true, "got %v", names)
	assert.Greater(t, len(names), 1, "the chime fixture carries a companion event")
}

func TestSummarizeToleratesMalformedJSON(t *testing.T) {
	device, names := summarize(payload(t, "malformed.json"))
	assert.Empty(t, device)
	assert.Empty(t, names)
}

// pubsubStub answers :pull from a queue of raw SDM envelopes and records acks.
type pubsubStub struct {
	mu       sync.Mutex
	queue    [][]byte
	acked    []string
	pulls    int
	pulled   chan struct{}
	failNext bool
}

func newPubSubStub(envelopes ...[]byte) *pubsubStub {
	return &pubsubStub{queue: envelopes, pulled: make(chan struct{}, 64)}
}

func (s *pubsubStub) server(t *testing.T, subscription string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/"+subscription+":pull", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.pulls++
		if s.failNext {
			s.failNext = false
			s.mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var out struct {
			ReceivedMessages []receivedMessage `json:"receivedMessages"`
		}
		for i, raw := range s.queue {
			out.ReceivedMessages = append(out.ReceivedMessages, receivedMessage{
				AckID:   "ack-" + string(rune('a'+i)),
				Message: pubsubMessage{Data: raw},
			})
		}
		s.queue = nil
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(out))
		s.pulled <- struct{}{}
	})
	mux.HandleFunc("/v1/"+subscription+":acknowledge", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			AckIDs []string `json:"ackIds"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		s.mu.Lock()
		s.acked = append(s.acked, in.AckIDs...)
		s.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (s *pubsubStub) ackIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.acked...)
}

// collector gathers handled events without racing the subscriber goroutine.
type collector struct {
	mu   sync.Mutex
	seen []Event
	got  chan struct{}
}

func newCollector() *collector { return &collector{got: make(chan struct{}, 64)} }

func (c *collector) handle(e Event) {
	c.mu.Lock()
	c.seen = append(c.seen, e)
	c.mu.Unlock()
	c.got <- struct{}{}
}

func (c *collector) events() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.seen...)
}

func runSubscriber(t *testing.T, stub *pubsubStub, devices []string, handler Handler) *Subscriber {
	t.Helper()
	const sub = "projects/test-project/subscriptions/sdm-events"
	srv := stub.server(t, sub)

	s := NewSubscriber(sub, devices, nil, handler,
		WithBaseURL(srv.URL+"/v1"),
		WithHTTPClient(srv.Client()))
	s.IdleDelay = time.Millisecond
	s.Logger = testLogger()
	return s
}

func TestSubscriberDeliversConfiguredDeviceAndAcks(t *testing.T) {
	stub := newPubSubStub(payload(t, "motion.json"))
	col := newCollector()
	s := runSubscriber(t, stub, []string{testDeviceOne}, col.handle)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()

	select {
	case <-col.got:
	case <-time.After(2 * time.Second):
		t.Fatal("no event handled")
	}
	assert.Eventually(t, func() bool { return len(stub.ackIDs()) == 1 }, 2*time.Second, time.Millisecond)
	cancel()
	<-done

	got := col.events()
	require.Len(t, got, 1)
	assert.Equal(t, KindMotion, got[0].Kind)
	assert.Equal(t, []string{"ack-a"}, stub.ackIDs())
}

// A message for a device that is not in config.yaml, an unknown event type, and
// a malformed envelope must all be acknowledged rather than left to redeliver.
func TestSubscriberAcksDroppedMessages(t *testing.T) {
	stub := newPubSubStub(
		payload(t, "unconfigured_device.json"),
		payload(t, "unknown.json"),
		payload(t, "malformed.json"),
	)
	col := newCollector()
	s := runSubscriber(t, stub, []string{testDeviceOne}, col.handle)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()

	select {
	case <-stub.pulled:
	case <-time.After(2 * time.Second):
		t.Fatal("no pull observed")
	}
	assert.Eventually(t, func() bool { return len(stub.ackIDs()) == 3 }, 2*time.Second, time.Millisecond)
	cancel()
	<-done

	assert.Empty(t, col.events())
}

// A pull failure must not end the subscriber: Pub/Sub outages are transient and
// the loop is the only thing keeping events flowing.
func TestSubscriberSurvivesPullFailure(t *testing.T) {
	stub := newPubSubStub(payload(t, "motion.json"))
	stub.failNext = true
	col := newCollector()
	s := runSubscriber(t, stub, []string{testDeviceOne}, col.handle)
	s.ErrorDelay = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()

	select {
	case <-col.got:
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not recover from pull failure")
	}
	cancel()
	<-done
}

func TestSubscriberStopsOnContextCancel(t *testing.T) {
	stub := newPubSubStub()
	s := runSubscriber(t, stub, []string{testDeviceOne}, func(Event) {})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return on cancellation")
	}
}
