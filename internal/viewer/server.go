package viewer

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ceb3/nest-to-ONVIF/internal/config"
	"github.com/ceb3/nest-to-ONVIF/internal/events"
)

// CameraView is one tile in the viewer grid.
type CameraView struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	IP       string `json:"ip"`
	Audio    bool   `json:"audio"`
	Events   bool   `json:"events"`
	HLSLQ    string `json:"hls_lq"`
	HLSHQ    string `json:"hls_hq"`
	Snapshot string `json:"snapshot"`
}

// Server serves the LAN viewer page and APIs.
type Server struct {
	cfg      *config.Config
	eventsOn bool
	bus      *EventBus
}

func NewServer(cfg *config.Config, eventsOn bool, bus *EventBus) *Server {
	return &Server{cfg: cfg, eventsOn: eventsOn, bus: bus}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/cameras", s.handleCameras)
	mux.HandleFunc("/api/events", s.handleEvents)
	return mux
}

func (s *Server) ListenAndServe(ctx context.Context, listen string) error {
	mux := s.Handler()

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("viewer listen on %s: %w", listen, err)
	}
	host, port, _ := net.SplitHostPort(listen)
	if host == "" || host == "0.0.0.0" {
		host = "<lan-ip>"
	}
	fmt.Printf("Viewer: http://%s:%s\n", host, port)

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	return srv.Serve(ln)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *Server) handleCameras(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	host := requestHost(r)
	out := make([]CameraView, 0, len(s.cfg.Cameras))
	for _, cam := range s.cfg.Cameras {
		path := cam.PathName()
		out = append(out, CameraView{
			Name:     cam.Name,
			Path:     path,
			IP:       cam.ONVIF.IP,
			Audio:    cam.Audio,
			Events:   cam.EventsEnabled,
			HLSLQ:    fmt.Sprintf("http://%s:%s/%s-lq/index.m3u8", host, config.HLSPort(), path),
			HLSHQ:    fmt.Sprintf("http://%s:%s/%s-hq/index.m3u8", host, config.HLSPort(), path),
			Snapshot: fmt.Sprintf("http://%s:%s/%s.jpg", host, config.SnapshotPort(), path),
		})
	}
	writeJSON(w, map[string]any{
		"cameras":   out,
		"events_on": s.eventsOn,
		"page_size": 6,
	})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if !s.eventsOn || s.bus == nil {
		http.Error(w, "events not enabled", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, unsubscribe := s.bus.Subscribe()
	defer unsubscribe()

	ctx := r.Context()
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			payload, _ := json.Marshal(edgeToSSE(e))
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		}
	}
}

type sseEvent struct {
	Camera string `json:"camera"`
	On     bool   `json:"on"`
	Kind   string `json:"kind,omitempty"`
	At     string `json:"at"`
}

func edgeToSSE(e events.Edge) sseEvent {
	kind := ""
	if e.On {
		kind = string(e.Kind)
	}
	return sseEvent{
		Camera: e.Camera.Name,
		On:     e.On,
		Kind:   kind,
		At:     e.At.UTC().Format(time.RFC3339),
	}
}

func requestHost(r *http.Request) string {
	host := r.Host
	if i := strings.LastIndex(host, ":"); i > 0 {
		if strings.Count(host, ":") == 1 || strings.HasPrefix(host, "[") {
			host = host[:i]
		}
	}
	host = strings.Trim(host, "[]")
	if host == "" || host == "localhost" {
		return "127.0.0.1"
	}
	return host
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
