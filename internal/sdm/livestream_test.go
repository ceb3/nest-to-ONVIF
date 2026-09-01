package sdm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateWebRTCStreamSendsOffer(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/enterprises/proj/devices/cam1:executeCommand", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &got))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":{
			"answerSdp":"v=0 answer",
			"mediaSessionId":"session-123",
			"expiresAt":"2026-01-01T00:05:00.000Z"}}`))
	}))
	defer srv.Close()

	c := NewClient("proj", staticSource(), WithBaseURL(srv.URL))
	s, err := c.GenerateWebRTCStream(context.Background(), "enterprises/proj/devices/cam1", "v=0 offer\n")
	require.NoError(t, err)

	assert.Equal(t, "sdm.devices.commands.CameraLiveStream.GenerateWebRtcStream", got["command"])
	params := got["params"].(map[string]any)
	assert.Equal(t, "v=0 offer\n", params["offerSdp"])
	assert.Equal(t, "v=0 answer", s.AnswerSDP)
	assert.Equal(t, "session-123", s.MediaSessionID)
	assert.Equal(t, time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC), s.ExpiresAt)
}

func TestExtendWebRTCStreamReturnsNewExpiry(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &got))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":{
			"mediaSessionId":"session-123",
			"expiresAt":"2026-01-01T00:10:00.000Z"}}`))
	}))
	defer srv.Close()

	c := NewClient("proj", staticSource(), WithBaseURL(srv.URL))
	s, err := c.ExtendWebRTCStream(context.Background(), "dev", "session-123")
	require.NoError(t, err)

	assert.Equal(t, "sdm.devices.commands.CameraLiveStream.ExtendWebRtcStream", got["command"])
	params := got["params"].(map[string]any)
	assert.Equal(t, "session-123", params["mediaSessionId"])
	assert.Equal(t, "session-123", s.MediaSessionID)
	assert.Equal(t, time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC), s.ExpiresAt)
}

func TestExtendWebRTCStreamKeepsExistingSessionIDWhenResponseOmitsIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":{"expiresAt":"2026-01-01T00:10:00Z"}}`))
	}))
	defer srv.Close()

	c := NewClient("proj", staticSource(), WithBaseURL(srv.URL))
	s, err := c.ExtendWebRTCStream(context.Background(), "dev", "session-123")
	require.NoError(t, err)
	assert.Equal(t, "session-123", s.MediaSessionID)
}

func TestExtendSurfacesUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"status":"FAILED_PRECONDITION","message":"Command is not supported for doorbell."}}`))
	}))
	defer srv.Close()

	c := NewClient("proj", staticSource(), WithBaseURL(srv.URL))
	_, err := c.ExtendWebRTCStream(context.Background(), "dev", "session-123")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrExtendUnsupported)

	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.HTTPStatusCode)
	assert.Equal(t, "FAILED_PRECONDITION", apiErr.Status)
}

func TestExtendDoesNotMisclassifyAmbiguousFailedPrecondition(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"status":"FAILED_PRECONDITION","message":"The media session has expired."}}`))
	}))
	defer srv.Close()

	c := NewClient("proj", staticSource(), WithBaseURL(srv.URL))
	_, err := c.ExtendWebRTCStream(context.Background(), "dev", "session-123")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrExtendUnsupported)

	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "FAILED_PRECONDITION", apiErr.Status)
	assert.Equal(t, "The media session has expired.", apiErr.Message)
}

func TestStreamCommandsRejectMalformedExpiryWithoutReturningZeroTime(t *testing.T) {
	const offerSecret = "offer-ice-secret"
	const answerSecret = "answer-ice-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":{
			"answerSdp":"answer-ice-secret",
			"mediaSessionId":"session-123",
			"expiresAt":"not-a-time"}}`))
	}))
	defer srv.Close()

	c := NewClient("proj", staticSource(), WithBaseURL(srv.URL))
	s, err := c.GenerateWebRTCStream(context.Background(), "dev", offerSecret)
	require.Error(t, err)
	assert.Nil(t, s)
	assert.Contains(t, err.Error(), "parse expiresAt")
	assert.NotContains(t, err.Error(), offerSecret)
	assert.NotContains(t, err.Error(), answerSecret)
}

func TestGenerateErrorOmitsServerMessageAndSDP(t *testing.T) {
	const offerSecret = "offer-ice-secret"
	const answerSecret = "answer-ice-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"offer-ice-secret answer-ice-secret"}}`))
	}))
	defer srv.Close()

	c := NewClient("proj", staticSource(), WithBaseURL(srv.URL))
	_, err := c.GenerateWebRTCStream(context.Background(), "dev", offerSecret)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), offerSecret)
	assert.NotContains(t, err.Error(), answerSecret)

	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "offer-ice-secret answer-ice-secret", apiErr.Message)
}

func TestStopWebRTCStream(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &got))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClient("proj", staticSource(), WithBaseURL(srv.URL))
	require.NoError(t, c.StopWebRTCStream(context.Background(), "dev", "session-123"))
	assert.Equal(t, "sdm.devices.commands.CameraLiveStream.StopWebRtcStream", got["command"])
	params := got["params"].(map[string]any)
	assert.Equal(t, "session-123", params["mediaSessionId"])
}

func TestStopWebRTCStreamTreatsMissingSessionAsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":404,"status":"NOT_FOUND","message":"The media session no longer exists."}}`))
	}))
	defer srv.Close()

	c := NewClient("proj", staticSource(), WithBaseURL(srv.URL))
	require.NoError(t, c.StopWebRTCStream(context.Background(), "dev", "session-123"))
}

func TestStopWebRTCStreamReturnsOtherFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"status":"PERMISSION_DENIED","message":"Denied."}}`))
	}))
	defer srv.Close()

	c := NewClient("proj", staticSource(), WithBaseURL(srv.URL))
	err := c.StopWebRTCStream(context.Background(), "dev", "session-123")
	require.Error(t, err)

	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "PERMISSION_DENIED", apiErr.Status)
}

func TestCommandRateLimitBehaviorIsPreserved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Rate limited."}}`))
	}))
	defer srv.Close()

	c := NewClient("proj", staticSource(), WithBaseURL(srv.URL))
	_, err := c.ExtendWebRTCStream(context.Background(), "dev", "session-123")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRateLimited)
	assert.NotErrorIs(t, err, ErrExtendUnsupported)

	var rateErr *RateLimitError
	require.ErrorAs(t, err, &rateErr)
	assert.True(t, rateErr.HasRetryAfter)
	assert.Equal(t, 30*time.Second, rateErr.RetryAfter)

	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusTooManyRequests, apiErr.HTTPStatusCode)
	assert.Equal(t, "RESOURCE_EXHAUSTED", apiErr.Status)
	assert.Equal(t, "Rate limited.", apiErr.Message)
	assert.NotContains(t, strings.ToLower(err.Error()), "rate limited.")
}
