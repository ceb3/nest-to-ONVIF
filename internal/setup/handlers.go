package setup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ceb3/nest-to-ONVIF/internal/sdm"
)

func (s *Server) handleAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if !s.requireReady(w) {
		return
	}
	s.mu.Lock()
	google := googleConfigFromDraft(s.draft)
	s.mu.Unlock()
	if strings.TrimSpace(google.ProjectID) == "" {
		writeError(w, http.StatusBadRequest, "save Google credentials first")
		return
	}

	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	state := hex.EncodeToString(buf)

	s.oauthMu.Lock()
	s.oauthState = state
	s.oauthWait = make(chan oauthResult, 1)
	wait := s.oauthWait
	s.oauthMu.Unlock()

	writeJSON(w, http.StatusOK, AuthStartResponse{URL: sdm.AuthCodeURL(google, state)})

	go func() {
		select {
		case res := <-wait:
			if res.err != nil {
				s.logger.Error("oauth failed", "err", res.err)
			}
		case <-time.After(6 * time.Minute):
		}
	}()
	_ = r
}

func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.oauthMu.Lock()
	state := s.oauthState
	wait := s.oauthWait
	s.oauthMu.Unlock()

	fail := func(msg string, err error) {
		if wait != nil {
			wait <- oauthResult{err: err}
		}
		http.Error(w, msg, http.StatusBadRequest)
	}

	if state == "" || wait == nil {
		fail("no authorisation in progress", fmt.Errorf("unexpected oauth callback"))
		return
	}
	if got := q.Get("state"); got != state {
		fail("state mismatch", fmt.Errorf("state mismatch"))
		return
	}
	if e := q.Get("error"); e != "" {
		fail(e, fmt.Errorf("authorisation denied: %s", e))
		return
	}
	code := q.Get("code")
	if code == "" {
		fail("missing code", fmt.Errorf("missing code"))
		return
	}

	s.mu.Lock()
	google := googleConfigFromDraft(s.draft)
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	tok, err := sdm.ExchangeCode(ctx, google, code)
	if err != nil {
		fail("token exchange failed", err)
		return
	}
	if err := sdm.NewFileTokenStore(s.tokenPath).Save(tok); err != nil {
		fail("save tokens failed", err)
		return
	}
	wait <- oauthResult{}
	fmt.Fprintln(w, "Authorisation complete. Return to the setup wizard.")
}

func (s *Server) handlePubSubKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if !s.requireReady(w) {
		return
	}
	const maxKey = 64 << 10
	body, err := io.ReadAll(io.LimitReader(r.Body, maxKey))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "empty upload")
		return
	}
	s.mu.Lock()
	s.draft.PubSubKey = body
	s.draft.HasPubSubKey = true
	s.mu.Unlock()
	if err := s.persistWizard(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if !s.requireReady(w) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.draft.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.draft.writeConfig(s.cfgPath); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.draft.view())
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if !s.requireReady(w) {
		return
	}
	s.mu.Lock()
	if err := s.draft.validate(); err != nil {
		s.mu.Unlock()
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.draft.writeConfig(s.cfgPath); err != nil {
		s.mu.Unlock()
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	hostIP := s.draft.Network.HostIP
	parent := s.draft.Network.ParentIface
	eventsOn := s.draft.Events.Onvif
	pubsubKey := append([]byte(nil), s.draft.PubSubKey...)
	s.mu.Unlock()

	if !s.authorized() {
		writeError(w, http.StatusUnauthorized, "authorize with Google first")
		return
	}
	if hostIP == "" {
		writeError(w, http.StatusBadRequest, "set deployment host IP on the Network step")
		return
	}
	if parent == "" {
		writeError(w, http.StatusBadRequest, "set macvlan parent interface on the Network step")
		return
	}

	log, err := Deploy(DeployOptions{
		RepoRoot:    s.repoRoot,
		DeployDir:   s.deployDir,
		ConfigPath:  s.cfgPath,
		TokenPath:   s.tokenPath,
		HostIP:      hostIP,
		ParentIface: parent,
		PubSubKey:   pubsubKey,
		EventsOnvif: eventsOn,
		LogLevel:    "info",
	})
	s.mu.Lock()
	s.lastDeploy = log
	s.mu.Unlock()

	if err != nil {
		writeJSON(w, http.StatusBadRequest, DeployResult{OK: false, Message: err.Error(), Log: log})
		return
	}
	envPath := filepath.Join(s.deployDir, "deploy.env")
	_ = os.WriteFile(envPath, []byte(fmt.Sprintf("HOST_IP=%q\nPARENT_IFACE=%q\n", hostIP, parent)), 0o644)
	writeJSON(w, http.StatusOK, DeployResult{OK: true, Message: "deployment complete", Log: log})
}
