package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ceb3/nest-to-ONVIF/internal/config"
	"github.com/ceb3/nest-to-ONVIF/internal/sdm"
)

// Server is the setup wizard HTTP server.
type Server struct {
	listen    string
	cfgPath   string
	tokenPath string
	repoRoot  string
	deployDir string
	logger    *slog.Logger

	mu         sync.Mutex
	draft      Draft
	lastDeploy string

	oauthState string
	oauthWait  chan oauthResult
	oauthMu    sync.Mutex
}

type oauthResult struct {
	err error
}

// NewServer builds a setup server for the given paths.
func NewServer(listen, cfgPath, tokenPath, repoRoot string) (*Server, error) {
	deployDir := filepath.Join(repoRoot, "deploy")
	draft, err := loadWizardDraft(repoRoot, cfgPath)
	if err != nil {
		return nil, err
	}
	if draft.Google.RedirectURI == "" {
		draft.Google.RedirectURI = redirectURIFromListen(listen)
	}
	draft.Google.RedirectURI = normalizeRedirectURI(draft.Google.RedirectURI)
	if draft.Network.HostIP == "" {
		draft.Network.HostIP = detectHostIP()
	}
	if draft.Network.ParentIface == "" {
		draft.Network.ParentIface = detectParentIface()
	}
	return &Server{
		listen:    listen,
		cfgPath:   cfgPath,
		tokenPath: tokenPath,
		repoRoot:  repoRoot,
		deployDir: deployDir,
		logger:    slog.Default(),
		draft:     draft,
	}, nil
}

// Run starts the HTTP server until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2callback", s.handleOAuthCallback)
	mux.HandleFunc("/api/discovery", s.handleDiscovery)
	mux.HandleFunc("/api/interfaces", s.handleInterfaces)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/google", s.handleGoogle)
	mux.HandleFunc("/api/network", s.handleNetwork)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/cameras", s.handleCameras)
	mux.HandleFunc("/api/devices", s.handleDevices)
	mux.HandleFunc("/api/auth/start", s.handleAuthStart)
	mux.HandleFunc("/api/pubsub-key", s.handlePubSubKey)
	mux.HandleFunc("/api/save", s.handleSave)
	mux.HandleFunc("/api/deploy", s.handleDeploy)
	mux.Handle("/", http.FileServer(http.FS(staticFS())))

	ln, err := net.Listen("tcp", s.listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.listen, err)
	}
	host, port, _ := net.SplitHostPort(s.listen)
	if host == "" {
		host = "localhost"
	}
	fmt.Printf("Setup wizard: http://%s:%s\n", host, port)
	fmt.Println("Press Ctrl+C to stop.")

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	return srv.Serve(ln)
}

func (s *Server) authorized() bool {
	if _, err := os.Stat(s.tokenPath); err != nil {
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

func readJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	dockerOK, dockerMsg := checkDocker()
	_, bridgeErr := findBridgeBinary(s.repoRoot)
	detectedIP := detectHostIP()
	detectedIface := detectParentIface()
	s.mu.Lock()
	defer s.mu.Unlock()
	hostIP := s.draft.Network.HostIP
	if hostIP == "" {
		hostIP = detectedIP
	}
	parentIface := s.draft.Network.ParentIface
	if parentIface == "" {
		parentIface = detectedIface
	}
	_, configSaved := os.Stat(s.cfgPath)
	_, deployed := os.Stat(filepath.Join(s.deployDir, "config", "config.yaml"))
	writeJSON(w, http.StatusOK, Status{
		Linux:          runtime.GOOS == "linux",
		Docker:         dockerOK,
		DockerMsg:      dockerMsg,
		BridgeBuilt:    bridgeErr == nil,
		Configured:     s.draft.Google.ProjectID != "",
		ConfigSaved:    configSaved == nil,
		Deployed:       deployed == nil,
		Authorized:     s.authorized(),
		CameraCount:    len(s.draft.selectedCameras()),
		HostIP:         hostIP,
		DetectedHostIP: detectedIP,
		ParentIface:    parentIface,
		LastDeploy:     s.lastDeploy,
	})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"google":         s.draft.Google,
		"network":        s.draft.Network,
		"events":         s.draft.Events,
		"cameras":        s.draft.Cameras,
		"media":          s.draft.Media,
		"has_pubsub_key": s.draft.HasPubSubKey,
		"pubsub_ready":   strings.TrimSpace(s.draft.Events.PubSubSubscription) != "" && s.draft.HasPubSubKey,
	})
}

func (s *Server) persistWizard() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return saveWizardDraft(s.repoRoot, &s.draft)
}

func (s *Server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	writeJSON(w, http.StatusOK, s.discovery())
}

func (s *Server) handleInterfaces(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	ifaces, err := listHostInterfaces()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ifaces)
}

func (s *Server) handleGoogle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if !s.requireReady(w) {
		return
	}
	var in GoogleInput
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(in.RedirectURI) == "" {
		in.RedirectURI = redirectURIFromListen(s.listen)
	}
	in.RedirectURI = normalizeRedirectURI(in.RedirectURI)
	s.mu.Lock()
	s.draft.Google = in
	s.mu.Unlock()
	if err := s.persistWizard(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleNetwork(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if !s.requireReady(w) {
		return
	}
	var in NetworkInput
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(in.HostIP) == "" {
		in.HostIP = detectHostIP()
	}
	if strings.TrimSpace(in.ParentIface) == "" {
		in.ParentIface = detectParentIface()
	}
	s.mu.Lock()
	s.draft.Network = in
	s.mu.Unlock()
	if err := s.persistWizard(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if !s.requireReady(w) {
		return
	}
	var in EventsInput
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mu.Lock()
	s.draft.Events = in
	s.mu.Unlock()
	if err := s.persistWizard(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleCameras(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if !s.requireReady(w) {
		return
	}
	var in CamerasInput
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	assigned, err := finalizeCameras(in.Cameras)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mu.Lock()
	s.draft.Cameras = assigned
	s.draft.Events.Onvif = anyCameraEventsOnvif(assigned)
	s.mu.Unlock()
	if err := s.persistWizard(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, CamerasInput{Cameras: assigned})
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if !s.requireReady(w) {
		return
	}
	s.mu.Lock()
	google := googleConfigFromDraft(s.draft)
	s.mu.Unlock()
	if strings.TrimSpace(google.ProjectID) == "" || strings.TrimSpace(google.ClientID) == "" {
		writeError(w, http.StatusBadRequest, "save Google credentials first")
		return
	}
	if !s.authorized() {
		writeError(w, http.StatusUnauthorized, "authorize with Google first")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	ts, err := sdm.NewTokenSource(ctx, google, sdm.NewFileTokenStore(s.tokenPath))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	devices, err := sdm.NewClient(google.ProjectID, ts).ListDevices(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	out := make([]DeviceInfo, 0, len(devices))
	for _, d := range devices {
		out = append(out, DeviceInfo{
			Name:      d.DisplayName(),
			Type:      strings.TrimPrefix(d.Type, "sdm.devices.types."),
			Protocols: d.SupportedProtocols(),
			DeviceID:  d.Name,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func googleConfigFromDraft(d Draft) config.GoogleConfig {
	return config.GoogleConfig{
		ProjectID:    strings.TrimSpace(d.Google.ProjectID),
		ClientID:     strings.TrimSpace(d.Google.ClientID),
		ClientSecret: strings.TrimSpace(d.Google.ClientSecret),
		RedirectURI:  strings.TrimSpace(d.Google.RedirectURI),
	}
}
