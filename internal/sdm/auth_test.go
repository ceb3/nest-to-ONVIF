package sdm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/ceb3/nest-to-ONVIF/internal/config"
)

func TestAuthCodeURLTargetsPartnerConnections(t *testing.T) {
	cfg := config.GoogleConfig{
		ProjectID:   "proj-uuid",
		ClientID:    "client-id",
		RedirectURI: "http://localhost:8190/oauth2callback",
	}
	raw := AuthCodeURL(cfg, "xyz")
	u, err := url.Parse(raw)
	require.NoError(t, err)

	// Device Access uses its own consent host, not accounts.google.com.
	assert.Equal(t, "nestservices.google.com", u.Host)
	assert.Equal(t, "/partnerconnections/proj-uuid/auth", u.Path)

	q := u.Query()
	assert.Equal(t, "client-id", q.Get("client_id"))
	assert.Equal(t, "code", q.Get("response_type"))
	assert.Equal(t, "https://www.googleapis.com/auth/sdm.service", q.Get("scope"))
	assert.Equal(t, "xyz", q.Get("state"))
	// Without these two, Google returns no refresh token.
	assert.Equal(t, "offline", q.Get("access_type"))
	assert.Equal(t, "consent", q.Get("prompt"))
}

func TestFileTokenStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	store := NewFileTokenStore(path)

	_, err := store.Load()
	require.Error(t, err, "loading before save should fail")

	want := &oauth2.Token{
		AccessToken:  "at",
		RefreshToken: "rt",
		Expiry:       time.Now().Add(time.Hour).Truncate(time.Second),
		TokenType:    "Bearer",
	}
	require.NoError(t, store.Save(want))

	got, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, want.AccessToken, got.AccessToken)
	assert.Equal(t, want.RefreshToken, got.RefreshToken)
	assert.True(t, want.Expiry.Equal(got.Expiry))
}

func TestFileTokenStoreIsNotWorldReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	store := NewFileTokenStore(path)
	require.NoError(t, store.Save(&oauth2.Token{AccessToken: "at", RefreshToken: "rt"}))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestNewTokenSourceRequiresStoredToken(t *testing.T) {
	cfg := config.GoogleConfig{ProjectID: "p", ClientID: "c", ClientSecret: "s"}
	store := NewFileTokenStore(filepath.Join(t.TempDir(), "absent.json"))

	_, err := NewTokenSource(context.Background(), cfg, store)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nest-bridge auth")
}

type staticTokenSource struct {
	tok *oauth2.Token
}

func (s *staticTokenSource) Token() (*oauth2.Token, error) {
	return s.tok, nil
}

type recordingTokenStore struct {
	mu        sync.Mutex
	failUntil int
	saves     int
	saved     *oauth2.Token
	loaded    *oauth2.Token
}

func (s *recordingTokenStore) Load() (*oauth2.Token, error) {
	if s.loaded != nil {
		return s.loaded, nil
	}
	return &oauth2.Token{AccessToken: "stored"}, nil
}

func (s *recordingTokenStore) Save(tok *oauth2.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	if s.saves <= s.failUntil {
		return fmt.Errorf("simulated save failure")
	}
	s.saved = tok
	return nil
}

func (s *recordingTokenStore) saveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestNewTokenSourceUsesThirtySecondRefreshTimeout(t *testing.T) {
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	var refreshDeadline time.Time
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var ok bool
		refreshDeadline, ok = req.Context().Deadline()
		require.True(t, ok, "token refresh request must carry the HTTP client timeout")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"access_token":"refreshed","token_type":"Bearer","expires_in":3600}`,
			)),
		}, nil
	})
	store := &recordingTokenStore{loaded: &oauth2.Token{
		AccessToken:  "expired",
		RefreshToken: "refresh",
		Expiry:       time.Now().Add(-time.Hour),
	}}

	src, err := NewTokenSource(context.Background(), config.GoogleConfig{
		ClientID:     "client",
		ClientSecret: "secret",
	}, store)
	require.NoError(t, err)
	_, err = src.Token()

	require.NoError(t, err)
	remaining := time.Until(refreshDeadline)
	assert.Greater(t, remaining, 29*time.Second)
	assert.LessOrEqual(t, remaining, 30*time.Second)
}

func TestPersistingTokenSourceRetriesSaveAfterFailure(t *testing.T) {
	refreshed := &oauth2.Token{AccessToken: "refreshed-at", RefreshToken: "refreshed-rt"}
	store := &recordingTokenStore{failUntil: 1}
	src := &persistingTokenSource{
		inner: &staticTokenSource{tok: refreshed},
		store: store,
	}

	_, err := src.Token()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "persist refreshed token")
	assert.Nil(t, store.saved)
	assert.Equal(t, 1, store.saveCount())

	got, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, refreshed.AccessToken, got.AccessToken)
	require.NotNil(t, store.saved)
	assert.Equal(t, refreshed.AccessToken, store.saved.AccessToken)
	assert.Equal(t, refreshed.RefreshToken, store.saved.RefreshToken)
	assert.Equal(t, 2, store.saveCount())

	_, err = src.Token()
	require.NoError(t, err)
	assert.Equal(t, 2, store.saveCount(), "unchanged token after successful save must not write again")
}

func TestPersistingTokenSourceSkipsSaveForUnchangedToken(t *testing.T) {
	tok := &oauth2.Token{AccessToken: "steady-at", RefreshToken: "steady-rt"}
	store := &recordingTokenStore{}
	src := &persistingTokenSource{
		inner: &staticTokenSource{tok: tok},
		store: store,
	}

	got, err := src.Token()
	require.NoError(t, err)
	assert.Equal(t, tok.AccessToken, got.AccessToken)
	assert.Equal(t, 1, store.saveCount())

	got, err = src.Token()
	require.NoError(t, err)
	assert.Equal(t, tok.AccessToken, got.AccessToken)
	assert.Equal(t, 1, store.saveCount(), "valid unchanged token must not trigger Save")
}
