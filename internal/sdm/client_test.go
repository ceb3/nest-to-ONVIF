package sdm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

func staticSource() oauth2.TokenSource {
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test-token", TokenType: "Bearer"})
}

const devicesBody = `{
  "devices": [
    {
      "name": "enterprises/p/devices/cam1",
      "type": "sdm.devices.types.CAMERA",
      "traits": {
        "sdm.devices.traits.Info": {"customName": "Driveway"},
        "sdm.devices.traits.CameraLiveStream": {"supportedProtocols": ["WEB_RTC"]}
      }
    },
    {
      "name": "enterprises/p/devices/door1",
      "type": "sdm.devices.types.DOORBELL",
      "traits": {
        "sdm.devices.traits.Info": {"customName": "Front Door"},
        "sdm.devices.traits.CameraLiveStream": {"supportedProtocols": ["WEB_RTC"]}
      }
    }
  ]
}`

func TestListDevicesParsesTraits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/enterprises/proj/devices", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(devicesBody))
	}))
	defer srv.Close()

	c := NewClient("proj", staticSource(), WithBaseURL(srv.URL))
	devices, err := c.ListDevices(context.Background())
	require.NoError(t, err)
	require.Len(t, devices, 2)

	assert.Equal(t, "Driveway", devices[0].DisplayName())
	assert.Equal(t, []string{"WEB_RTC"}, devices[0].SupportedProtocols())
	assert.Equal(t, "Front Door", devices[1].DisplayName())
}

func TestListDevicesSurfacesRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Rate limited."}}`))
	}))
	defer srv.Close()

	c := NewClient("proj", staticSource(), WithBaseURL(srv.URL))
	_, err := c.ListDevices(context.Background())
	require.Error(t, err)
	// The scheduler keys its backoff off this sentinel, so it must survive wrapping.
	assert.True(t, errors.Is(err, ErrRateLimited), "expected ErrRateLimited, got %v", err)
}

func TestDisplayNameFallsBackToDeviceID(t *testing.T) {
	d := Device{Name: "enterprises/p/devices/abc123"}
	assert.Equal(t, "abc123", d.DisplayName())
}

func TestListDevicesPaginatesAllPages(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "/enterprises/proj/devices", r.URL.Path)
		switch calls {
		case 1:
			assert.Empty(t, r.URL.Query().Get("pageToken"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
  "devices": [{"name": "enterprises/p/devices/cam1", "type": "sdm.devices.types.CAMERA", "traits": {}}],
  "nextPageToken": "page2"
}`))
		case 2:
			assert.Equal(t, "page2", r.URL.Query().Get("pageToken"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
  "devices": [{"name": "enterprises/p/devices/cam2", "type": "sdm.devices.types.CAMERA", "traits": {}}]
}`))
		default:
			t.Fatalf("unexpected request #%d", calls)
		}
	}))
	defer srv.Close()

	c := NewClient("proj", staticSource(), WithBaseURL(srv.URL))
	devices, err := c.ListDevices(context.Background())
	require.NoError(t, err)
	require.Len(t, devices, 2)
	assert.Equal(t, "enterprises/p/devices/cam1", devices[0].Name)
	assert.Equal(t, "enterprises/p/devices/cam2", devices[1].Name)
	assert.Equal(t, 2, calls)
}

func TestListDevicesPaginationExceedsMaxPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"devices":[],"nextPageToken":"forever"}`))
	}))
	defer srv.Close()

	c := NewClient("proj", staticSource(), WithBaseURL(srv.URL))
	_, err := c.ListDevices(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), fmt.Sprintf("exceeded %d pages", maxListDevicePages))
}

func TestListDevicesRateLimitWithRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Rate limited."}}`))
	}))
	defer srv.Close()

	c := NewClient("proj", staticSource(), WithBaseURL(srv.URL))
	_, err := c.ListDevices(context.Background())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRateLimited))

	var rle *RateLimitError
	require.True(t, errors.As(err, &rle))
	assert.True(t, rle.HasRetryAfter)
	assert.Equal(t, 30*time.Second, rle.RetryAfter)
}

func TestListDevicesRateLimitWithoutRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Rate limited."}}`))
	}))
	defer srv.Close()

	c := NewClient("proj", staticSource(), WithBaseURL(srv.URL))
	_, err := c.ListDevices(context.Background())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRateLimited))

	var rle *RateLimitError
	require.True(t, errors.As(err, &rle))
	assert.False(t, rle.HasRetryAfter)
}

func TestListDevicesErrorOmitsCredentials(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		body       string
	}{
		{"forbidden", http.StatusForbidden, `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"Forbidden."}}`},
		{"server error", http.StatusInternalServerError, `{"error":{"code":500,"status":"INTERNAL","message":"Server error."}}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.statusCode)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := NewClient("proj", staticSource(), WithBaseURL(srv.URL))
			_, err := c.ListDevices(context.Background())
			require.Error(t, err)
			assert.False(t, errors.Is(err, ErrRateLimited))
			msg := err.Error()
			assert.NotContains(t, strings.ToLower(msg), "bearer")
			assert.NotContains(t, msg, "test-token")
			assert.NotContains(t, strings.ToLower(msg), "authorization")
		})
	}
}

func TestSupportedProtocolsMissingTraitReturnsNil(t *testing.T) {
	d := Device{
		Name:   "enterprises/p/devices/cam1",
		Type:   "sdm.devices.types.CAMERA",
		Traits: map[string]json.RawMessage{},
	}
	assert.Nil(t, d.SupportedProtocols())
}
