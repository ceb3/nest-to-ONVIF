package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/ceb3/nest-to-ONVIF/internal/config"
	"github.com/ceb3/nest-to-ONVIF/internal/sdm"
)

type authResult struct {
	code string
	err  error
}

func parseRedirectURI(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse redirect_uri: %w", err)
	}
	if u.Scheme != "http" {
		return nil, fmt.Errorf("redirect_uri must use http")
	}
	host := u.Hostname()
	if !isLoopbackHost(host) {
		return nil, fmt.Errorf("redirect_uri must use a loopback address, got %q", host)
	}
	if u.Port() == "" {
		return nil, fmt.Errorf("redirect_uri must include an explicit port")
	}
	if u.Path == "" {
		return nil, fmt.Errorf("redirect_uri must include a callback path")
	}
	return u, nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func authCallbackHandler(state string, results chan<- authResult) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if got := q.Get("state"); got != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			results <- authResult{err: fmt.Errorf("state mismatch: possible CSRF")}
			return
		}
		if e := q.Get("error"); e != "" {
			http.Error(w, e, http.StatusBadRequest)
			results <- authResult{err: fmt.Errorf("authorisation denied: %s", e)}
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			results <- authResult{err: fmt.Errorf("no authorisation code in callback")}
			return
		}
		fmt.Fprintln(w, "Authorisation complete. You can close this tab.")
		results <- authResult{code: code}
	}
}

func RunAuth(ctx context.Context, cfgPath, tokenPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	redirect, err := parseRedirectURI(cfg.Google.RedirectURI)
	if err != nil {
		return err
	}

	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Errorf("generate state: %w", err)
	}
	state := hex.EncodeToString(buf)

	results := make(chan authResult, 1)

	mux := http.NewServeMux()
	mux.HandleFunc(redirect.Path, authCallbackHandler(state, results))

	ln, err := net.Listen("tcp", redirect.Host)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", redirect.Host, err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	fmt.Println("Open this URL in a browser and authorise access:")
	fmt.Println()
	fmt.Println("  " + sdm.AuthCodeURL(cfg.Google, state))
	fmt.Println()
	fmt.Println("Waiting for the callback...")

	var res authResult
	select {
	case res = <-results:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Minute):
		return fmt.Errorf("timed out waiting for authorisation")
	}
	if res.err != nil {
		return res.err
	}

	tok, err := sdm.ExchangeCode(ctx, cfg.Google, res.code)
	if err != nil {
		return err
	}
	if err := sdm.NewFileTokenStore(tokenPath).Save(tok); err != nil {
		return err
	}

	fmt.Printf("Credentials saved to %s\n", tokenPath)
	return nil
}
