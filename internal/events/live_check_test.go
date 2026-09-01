package events

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/mustacheride/nest-to-ONVIF/internal/config"
	"github.com/stretchr/testify/require"
)

// TestLiveTriggerDelivery drives the real Trigger and Dispatcher against a real
// camera. Skipped unless NEST_LIVE_CAMERA_IP names one, because it needs the
// deployment on the LAN.
func TestLiveTriggerDelivery(t *testing.T) {
	ip := os.Getenv("NEST_LIVE_CAMERA_IP")
	if ip == "" {
		t.Skip("set NEST_LIVE_CAMERA_IP to run")
	}
	cam := config.Camera{DeviceID: "dev", Name: "Live", Linger: time.Second,
		ONVIF: config.ONVIFConfig{IP: ip}}

	trigger := NewTrigger()
	trigger.Logger = testLogger()
	d := NewDispatcher(trigger.Deliver)
	d.Logger = testLogger()
	ctx, cancel := context.WithCancel(context.Background())
	go d.Run(ctx)

	clock := newFakeClock()
	tr := NewTracker([]config.Camera{cam}, d.Send, clock)
	tr.Logger = testLogger()

	require.NoError(t, trigger.Deliver(ctx, Edge{Camera: cam, On: false}))

	tr.Handle(Event{DeviceID: "dev", Kind: KindMotion, At: clock.Now()})
	time.Sleep(500 * time.Millisecond)
	// The endpoint reports whether a trigger changed state, so a probe that
	// reports no change proves the tracker's own POST already landed.
	require.False(t, probeChanged(t, ip, "on"), "on edge was not delivered")

	clock.advance(2 * time.Second)
	tr.Sweep(clock.Now())
	time.Sleep(500 * time.Millisecond)
	require.False(t, probeChanged(t, ip, "off"), "off edge was not delivered")

	cancel()
}

// TestLivePubSubPull proves the service-account key and subscription work
// together. It asserts nothing about content: whether anything is queued depends
// on whether a camera has recently seen motion.
func TestLivePubSubPull(t *testing.T) {
	key := os.Getenv("NEST_LIVE_SA_KEY")
	sub := os.Getenv("NEST_LIVE_SUBSCRIPTION")
	if key == "" || sub == "" {
		t.Skip("set NEST_LIVE_SA_KEY and NEST_LIVE_SUBSCRIPTION to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ts, err := TokenSourceFromKeyFile(ctx, key)
	require.NoError(t, err)

	s := NewSubscriber(sub, nil, ts, func(Event) {})
	s.Logger = testLogger()
	msgs, err := s.pull(ctx)
	require.NoError(t, err)
	t.Logf("pull returned %d message(s)", len(msgs))
}

func probeChanged(t *testing.T, ip, state string) bool {
	t.Helper()
	resp, err := http.Post("http://"+ip+"/trigger/motion?state="+state, "", nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Motion  bool `json:"motion"`
		Changed bool `json:"changed"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	t.Logf("probe state=%s -> motion=%v changed=%v", state, out.Motion, out.Changed)
	return out.Changed
}
