package cli

import (
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mustacheride/nest-to-ONVIF/internal/config"
)

// ONVIF forwarding is opt-in. The default config must not start Pub/Sub.
func TestEventsDisabledByDefault(t *testing.T) {
	cfg := &config.Config{}
	assert.False(t, cfg.Events.Onvif)
}

// When events.onvif is false, startEvents is a no-op regardless of whether
// Pub/Sub credentials are present.
func TestStartEventsNoOpWhenOnvifDisabled(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{
		Events: config.EventsConfig{Onvif: false},
		Google: config.GoogleConfig{
			PubSubSubscription: "projects/p/subscriptions/sdm-events",
			ServiceAccountKey:  "pubsub-sa.json",
		},
	}
	rt := startEvents(t.Context(), cfg, nil, log, nil)
	assert.NotNil(t, rt.bus)
	rt.stop()
}
