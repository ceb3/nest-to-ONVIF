// Package events turns Google Smart Device Management device events into motion
// on an ONVIF client timeline.
//
// Google publishes an event per detection to a Pub/Sub subscription in the
// user's own Cloud project. This package pulls them, filters them to the
// configured cameras, and hands them to a Tracker that maintains a per-camera
// motion level. Each change in that level is delivered as an HTTP POST to the
// ONVIF Events trigger on the camera's own address.
//
// Streaming is the bridge's primary function and events are an enhancement, so
// nothing here reports a fatal error: a subscription that cannot be reached
// costs timeline markers, not video.
package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Scope is the only scope the bridge needs against Pub/Sub. cloud-platform
// would also work and is what most examples use, but it grants the whole
// project to read one subscription.
const Scope = "https://www.googleapis.com/auth/pubsub"

const defaultPubSubBaseURL = "https://pubsub.googleapis.com/v1"

const (
	defaultMaxMessages = 10
	// The REST pull long-polls, so the idle delay only paces the loop when the
	// server returns immediately with nothing.
	defaultIdleDelay  = 2 * time.Second
	defaultErrorDelay = 10 * time.Second
	pullTimeout       = 90 * time.Second
)

// Kind is the detection a device reported. Protect files all of them as motion;
// the distinction survives only in this process's logs.
type Kind string

const (
	KindMotion Kind = "motion"
	KindPerson Kind = "person"
	KindChime  Kind = "chime"
)

// kinds maps SDM trait event names to a Kind. Anything absent is dropped
// silently: Google adds event types over time and treating a new one as an
// error would wedge the subscriber behind a message it can never accept.
var kinds = map[string]Kind{
	"sdm.devices.events.CameraMotion.Motion": KindMotion,
	"sdm.devices.events.CameraPerson.Person": KindPerson,
	"sdm.devices.events.DoorbellChime.Chime": KindChime,
}

// Event is one decoded detection.
type Event struct {
	DeviceID string
	Kind     Kind
	At       time.Time
	ThreadID string
}

// Handler receives decoded events. It must not block: it runs on the pull loop.
type Handler func(Event)

type sdmEnvelope struct {
	Timestamp      time.Time `json:"timestamp"`
	EventThreadID  string    `json:"eventThreadId"`
	ResourceUpdate struct {
		Name   string                     `json:"name"`
		Events map[string]json.RawMessage `json:"events"`
	} `json:"resourceUpdate"`
}

func decode(raw []byte) ([]Event, error) {
	var env sdmEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// Deliberately not wrapping the payload into the error: it carries the
		// device resource path.
		return nil, fmt.Errorf("decode sdm envelope: invalid JSON")
	}
	if env.ResourceUpdate.Name == "" {
		return nil, nil
	}
	var out []Event
	for name := range env.ResourceUpdate.Events {
		kind, ok := kinds[name]
		if !ok {
			continue
		}
		out = append(out, Event{
			DeviceID: env.ResourceUpdate.Name,
			Kind:     kind,
			At:       env.Timestamp,
			ThreadID: env.EventThreadID,
		})
	}
	return out, nil
}

// shortDeviceID trims a device resource path to a recognisable tail. The full
// path carries the enterprise UUID, which does not belong in a log that may be
// pasted into an issue.
func shortDeviceID(device string) string {
	id := device[strings.LastIndex(device, "/")+1:]
	if len(id) > 12 {
		return "…" + id[len(id)-12:]
	}
	return id
}

// summarize reports the device and the raw event names in an envelope, for
// debug logging only. Every message is worth reporting, including the trait
// updates that carry no detection: without them, a device that publishes
// updates but never detections cannot be distinguished from one that publishes
// nothing, and those have different causes.
//
// Malformed input yields empty values rather than an error. The caller is a log
// line; it has already handled decode's error.
func summarize(raw []byte) (device string, names []string) {
	var env sdmEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", nil
	}
	for name := range env.ResourceUpdate.Events {
		names = append(names, name)
	}
	sort.Strings(names)
	return env.ResourceUpdate.Name, names
}

// Subscriber pulls SDM events from a Pub/Sub subscription.
//
// This uses the REST pull endpoint rather than cloud.google.com/go/pubsub. The
// library's advantages are streaming pull and lease management, and neither
// applies: messages are acknowledged the moment they reach the tracker, so
// there are no leases to extend, and the whole event stream is a handful of
// messages a minute. Against that, the library pulls in gRPC and its own
// transport stack, and its stub-free testing story is worse than an httptest
// server answering two documented endpoints.
type Subscriber struct {
	subscription string
	devices      map[string]struct{}
	handler      Handler

	http    *http.Client
	baseURL string

	MaxMessages int
	// IdleDelay paces the loop when a pull returns nothing.
	IdleDelay time.Duration
	// ErrorDelay paces the loop after a failed pull, so a sustained Pub/Sub
	// outage does not turn into a hot retry loop.
	ErrorDelay time.Duration

	Logger *slog.Logger
}

type SubscriberOption func(*Subscriber)

func WithBaseURL(u string) SubscriberOption { return func(s *Subscriber) { s.baseURL = u } }

func WithHTTPClient(h *http.Client) SubscriberOption {
	return func(s *Subscriber) { s.http = h }
}

// NewSubscriber builds a subscriber for subscription, dropping events for any
// device outside devices.
func NewSubscriber(subscription string, devices []string, ts oauth2.TokenSource, handler Handler, opts ...SubscriberOption) *Subscriber {
	set := make(map[string]struct{}, len(devices))
	for _, d := range devices {
		set[d] = struct{}{}
	}
	s := &Subscriber{
		subscription: subscription,
		devices:      set,
		handler:      handler,
		baseURL:      defaultPubSubBaseURL,
		http:         &http.Client{Timeout: pullTimeout},
		MaxMessages:  defaultMaxMessages,
		IdleDelay:    defaultIdleDelay,
		ErrorDelay:   defaultErrorDelay,
	}
	if ts != nil {
		s.http = &http.Client{Timeout: pullTimeout, Transport: &oauth2.Transport{Source: ts}}
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// TokenSourceFromKeyFile reads a service-account key and returns a token source
// scoped to Pub/Sub.
func TokenSourceFromKeyFile(ctx context.Context, path string) (oauth2.TokenSource, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read service account key: %w", err)
	}
	creds, err := google.CredentialsFromJSON(ctx, raw, Scope)
	if err != nil {
		// google's error can quote the key material, so it is not wrapped.
		return nil, fmt.Errorf("parse service account key: invalid JSON key file")
	}
	return creds.TokenSource, nil
}

func (s *Subscriber) log() *slog.Logger {
	if s.Logger == nil {
		return slog.Default()
	}
	return s.Logger
}

type pubsubMessage struct {
	Data []byte `json:"data"`
}

type receivedMessage struct {
	AckID   string        `json:"ackId"`
	Message pubsubMessage `json:"message"`
}

// Run pulls until ctx is cancelled. It never returns an error, by design: the
// caller runs cameras alongside it and a broken subscription must not be
// allowed to look like a reason to stop them.
func (s *Subscriber) Run(ctx context.Context) {
	s.log().Info("subscribing to device events", "devices", len(s.devices))

	for ctx.Err() == nil {
		msgs, err := s.pull(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			s.log().Warn("pubsub pull failed", "error", err, "retry_in", s.ErrorDelay)
			if sleepCtx(ctx, s.ErrorDelay) != nil {
				break
			}
			continue
		}
		if len(msgs) == 0 {
			if sleepCtx(ctx, s.IdleDelay) != nil {
				break
			}
			continue
		}

		ackIDs := make([]string, 0, len(msgs))
		for _, m := range msgs {
			// Every message is acknowledged, including ones that are dropped.
			// Redelivery would only bring back an event that is already stale,
			// and a payload this process cannot use will never become usable.
			ackIDs = append(ackIDs, m.AckID)
			s.dispatch(m.Message.Data)
		}
		if err := s.acknowledge(ctx, ackIDs); err != nil && ctx.Err() == nil {
			s.log().Warn("pubsub acknowledge failed", "error", err, "count", len(ackIDs))
		}
	}

	s.log().Info("device event subscriber stopped")
}

func (s *Subscriber) dispatch(raw []byte) {
	if s.log().Enabled(context.Background(), slog.LevelDebug) {
		device, names := summarize(raw)
		_, configured := s.devices[device]
		s.log().Debug("pubsub message",
			"device", shortDeviceID(device), "configured", configured, "events", names)
	}
	evts, err := decode(raw)
	if err != nil {
		s.log().Warn("dropping undecodable device event", "error", err)
		return
	}
	for _, e := range evts {
		if _, ok := s.devices[e.DeviceID]; !ok {
			// Expected: the subscription carries every device in the Device
			// Access project, including the ones deliberately out of scope.
			s.log().Debug("dropping event for unconfigured device", "kind", e.Kind)
			continue
		}
		s.handler(e)
	}
}

func (s *Subscriber) pull(ctx context.Context) ([]receivedMessage, error) {
	body, err := json.Marshal(map[string]any{"maxMessages": s.MaxMessages})
	if err != nil {
		return nil, err
	}
	var out struct {
		ReceivedMessages []receivedMessage `json:"receivedMessages"`
	}
	if err := s.post(ctx, ":pull", body, &out); err != nil {
		return nil, err
	}
	return out.ReceivedMessages, nil
}

func (s *Subscriber) acknowledge(ctx context.Context, ackIDs []string) error {
	body, err := json.Marshal(map[string]any{"ackIds": ackIDs})
	if err != nil {
		return err
	}
	return s.post(ctx, ":acknowledge", body, nil)
}

func (s *Subscriber) post(ctx context.Context, action string, body []byte, out any) error {
	reqURL := s.baseURL + "/" + s.subscription + action
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("call pubsub: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Google's message quotes the resource name but not credentials; the
		// status alone is enough to tell a 403 on IAM from a 404 on the name.
		var ae struct {
			Error struct {
				Status string `json:"status"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &ae)
		if ae.Error.Status != "" {
			return fmt.Errorf("pubsub: HTTP %d (%s)", resp.StatusCode, ae.Error.Status)
		}
		return fmt.Errorf("pubsub: HTTP %d", resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
