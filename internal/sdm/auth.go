package sdm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/oauth2"

	"github.com/mustacheride/nest-to-ONVIF/internal/config"
)

// Scope is the only OAuth scope the SDM API accepts.
const Scope = "https://www.googleapis.com/auth/sdm.service"

const tokenEndpoint = "https://oauth2.googleapis.com/token"

// AuthCodeURL builds the Device Access consent URL. This deliberately does not use
// oauth2.Config.AuthCodeURL: Device Access authorises through its own
// partnerconnections host rather than accounts.google.com, and sending users to the
// standard endpoint yields a token that the SDM API rejects.
func AuthCodeURL(cfg config.GoogleConfig, state string) string {
	q := url.Values{}
	q.Set("client_id", cfg.ClientID)
	q.Set("redirect_uri", cfg.RedirectURI)
	q.Set("response_type", "code")
	q.Set("scope", Scope)
	q.Set("state", state)
	// access_type=offline and prompt=consent are both required; without them Google
	// omits the refresh token and the bridge cannot run unattended.
	q.Set("access_type", "offline")
	q.Set("prompt", "consent")

	return fmt.Sprintf("https://nestservices.google.com/partnerconnections/%s/auth?%s",
		cfg.ProjectID, q.Encode())
}

func oauthConfig(cfg config.GoogleConfig) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  cfg.RedirectURI,
		Scopes:       []string{Scope},
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://nestservices.google.com/partnerconnections/auth",
			TokenURL: tokenEndpoint,
		},
	}
}

func ExchangeCode(ctx context.Context, cfg config.GoogleConfig, code string) (*oauth2.Token, error) {
	tok, err := oauthConfig(cfg).Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("exchange authorization code: %w", err)
	}
	if tok.RefreshToken == "" {
		return nil, fmt.Errorf("no refresh token returned; re-authorise with prompt=consent")
	}
	return tok, nil
}

type TokenStore interface {
	Load() (*oauth2.Token, error)
	Save(*oauth2.Token) error
}

type fileTokenStore struct {
	path string
	mu   sync.Mutex
}

func NewFileTokenStore(path string) TokenStore { return &fileTokenStore{path: path} }

func (f *fileTokenStore) Load() (*oauth2.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	raw, err := os.ReadFile(f.path)
	if err != nil {
		return nil, fmt.Errorf("read token file: %w", err)
	}
	var tok oauth2.Token
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, fmt.Errorf("parse token file: %w", err)
	}
	return &tok, nil
}

func (f *fileTokenStore) Save(tok *oauth2.Token) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	raw, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return fmt.Errorf("encode token: %w", err)
	}
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create token directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tokens-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp token file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("set temp token file permissions: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp token file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp token file: %w", err)
	}
	if err := os.Rename(tmpPath, f.path); err != nil {
		cleanup()
		return fmt.Errorf("write token file: %w", err)
	}
	return nil
}

// persistingTokenSource writes refreshed tokens back to the store so a restart does
// not require re-authorisation. Save is serialised so concurrent callers may share one
// token source safely.
type persistingTokenSource struct {
	inner oauth2.TokenSource
	store TokenStore
	mu    sync.Mutex
	last  string
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.inner.Token()
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if tok.AccessToken != p.last {
		if err := p.store.Save(tok); err != nil {
			// Return the refresh error without the token value; the in-memory
			// oauth2 source still holds the new access token for retry.
			return nil, fmt.Errorf("persist refreshed token: %w", err)
		}
		p.last = tok.AccessToken
	}
	return tok, nil
}

func NewTokenSource(ctx context.Context, cfg config.GoogleConfig, store TokenStore) (oauth2.TokenSource, error) {
	tok, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("no stored credentials; run 'nest-bridge auth' first: %w", err)
	}
	oauthCtx := context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: requestTimeout})
	inner := oauthConfig(cfg).TokenSource(oauthCtx, tok)
	return &persistingTokenSource{inner: inner, store: store}, nil
}
