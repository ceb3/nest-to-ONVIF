package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRedirectURI(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantHost string
		wantPath string
		wantErr  string
	}{
		{
			name:     "default localhost",
			raw:      "http://localhost:8190/oauth2callback",
			wantHost: "localhost:8190",
			wantPath: "/oauth2callback",
		},
		{
			name:     "loopback IP",
			raw:      "http://127.0.0.1:8190/oauth2callback",
			wantHost: "127.0.0.1:8190",
			wantPath: "/oauth2callback",
		},
		{
			name:    "not loopback",
			raw:     "http://example.com:8190/oauth2callback",
			wantErr: "loopback",
		},
		{
			name:    "https scheme",
			raw:     "https://localhost:8190/oauth2callback",
			wantErr: "http",
		},
		{
			name:    "missing port",
			raw:     "http://localhost/oauth2callback",
			wantErr: "port",
		},
		{
			name:    "missing path",
			raw:     "http://localhost:8190",
			wantErr: "path",
		},
		{
			name:    "invalid URL",
			raw:     "://bad",
			wantErr: "parse redirect_uri",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRedirectURI(tt.raw)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantHost, got.Host)
			assert.Equal(t, tt.wantPath, got.Path)
		})
	}
}

func TestAuthCallbackHandler(t *testing.T) {
	const state = "expected-state"

	waitResult := func(t *testing.T, results <-chan authResult) authResult {
		t.Helper()
		select {
		case res := <-results:
			return res
		default:
			t.Fatal("expected a result on the channel")
			return authResult{}
		}
	}

	t.Run("success delivers code", func(t *testing.T) {
		results := make(chan authResult, 1)
		handler := authCallbackHandler(state, results)

		req := httptest.NewRequest(http.MethodGet, "/oauth2callback?state="+state+"&code=secret-code", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), "close this tab")

		res := waitResult(t, results)
		require.NoError(t, res.err)
		assert.Equal(t, "secret-code", res.code)
	})

	t.Run("state mismatch rejected", func(t *testing.T) {
		results := make(chan authResult, 1)
		handler := authCallbackHandler(state, results)

		req := httptest.NewRequest(http.MethodGet, "/oauth2callback?state=wrong&code=secret-code", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, "state mismatch\n", rec.Body.String())

		res := waitResult(t, results)
		require.Error(t, res.err)
		assert.Contains(t, res.err.Error(), "state mismatch")
		assert.NotContains(t, res.err.Error(), "secret-code")
	})

	t.Run("missing state rejected", func(t *testing.T) {
		results := make(chan authResult, 1)
		handler := authCallbackHandler(state, results)

		req := httptest.NewRequest(http.MethodGet, "/oauth2callback?code=secret-code", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		res := waitResult(t, results)
		require.Error(t, res.err)
		assert.Contains(t, res.err.Error(), "state mismatch")
		assert.NotContains(t, res.err.Error(), "secret-code")
	})

	t.Run("access denied", func(t *testing.T) {
		results := make(chan authResult, 1)
		handler := authCallbackHandler(state, results)

		req := httptest.NewRequest(http.MethodGet, "/oauth2callback?state="+state+"&error=access_denied", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, "access_denied\n", rec.Body.String())

		res := waitResult(t, results)
		require.Error(t, res.err)
		assert.Contains(t, res.err.Error(), "authorisation denied")
		assert.Contains(t, res.err.Error(), "access_denied")
	})

	t.Run("missing code", func(t *testing.T) {
		results := make(chan authResult, 1)
		handler := authCallbackHandler(state, results)

		req := httptest.NewRequest(http.MethodGet, "/oauth2callback?state="+state, nil)
		rec := httptest.NewRecorder()
		handler(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		res := waitResult(t, results)
		require.Error(t, res.err)
		assert.Contains(t, res.err.Error(), "no authorisation code")
	})
}

func TestAuthCallbackHandlerDoesNotEchoRequestURL(t *testing.T) {
	results := make(chan authResult, 1)
	handler := authCallbackHandler("state", results)

	req := httptest.NewRequest(http.MethodGet, "/oauth2callback?state=state&code=leaked-code", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	assert.NotContains(t, string(body), "leaked-code")
	assert.NotContains(t, rec.Result().Header.Get("Location"), "leaked-code")

	res := <-results
	require.NoError(t, res.err)
	assert.Equal(t, "leaked-code", res.code)
}

func TestAuthCallbackHandlerQueryEncoding(t *testing.T) {
	results := make(chan authResult, 1)
	handler := authCallbackHandler("s", results)

	q := url.Values{}
	q.Set("state", "s")
	q.Set("code", "abc123")
	req := httptest.NewRequest(http.MethodGet, "/oauth2callback?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	res := <-results
	require.NoError(t, res.err)
	assert.Equal(t, "abc123", res.code)
	assert.False(t, strings.Contains(rec.Body.String(), "abc123"))
}
