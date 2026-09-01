// Obfuscate nest-bridge config files for docs screenshots or safe sharing.
//
// Usage:
//
//	go run ./scripts/obfuscate_config.go -config config.yaml -tokens tokens.json
//	go run ./scripts/obfuscate_config.go -config config.yaml -in-place -backup
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mustacheride/nest-to-ONVIF/internal/config"
)

const (
	placeholderProjectID  = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	placeholderClientID   = "000000000000-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx.apps.googleusercontent.com"
	placeholderSecret     = "GOCSPX-xxxxxxxxxxxxxxxxxxxxxxxx"
	placeholderPubSub     = "projects/your-gcp-project/subscriptions/nest-events"
	placeholderEnterprise = "00000000-0000-0000-0000-000000000000"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "config.yaml path")
	tokensPath := flag.String("tokens", "", "optional tokens.json path")
	draftOut := flag.String("draft", "", "write obfuscated setup-draft.yaml here")
	inPlace := flag.Bool("in-place", false, "overwrite input files")
	backup := flag.Bool("backup", true, "when -in-place, write .bak copies first")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	obf := obfuscateConfig(cfg)
	if *draftOut != "" {
		if err := writeDraft(*draftOut, obf); err != nil {
			fmt.Fprintf(os.Stderr, "write draft: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", *draftOut)
	}

	if *inPlace {
		if *backup {
			if err := backupFile(*cfgPath); err != nil {
				fmt.Fprintf(os.Stderr, "backup config: %v\n", err)
				os.Exit(1)
			}
		}
		if err := writeConfigYAML(*cfgPath, obf); err != nil {
			fmt.Fprintf(os.Stderr, "write config: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "obfuscated %s\n", *cfgPath)
	}

	if *tokensPath != "" {
		if *inPlace && *backup {
			if err := backupFile(*tokensPath); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "backup tokens: %v\n", err)
				os.Exit(1)
			}
		}
		if err := writeTokens(*tokensPath); err != nil {
			fmt.Fprintf(os.Stderr, "write tokens: %v\n", err)
			os.Exit(1)
		}
		if *inPlace {
			fmt.Fprintf(os.Stderr, "obfuscated %s\n", *tokensPath)
		}
	}

	if !*inPlace && *draftOut == "" {
		if err := writeConfigYAML("-", obf); err != nil {
			fmt.Fprintf(os.Stderr, "write config: %v\n", err)
			os.Exit(1)
		}
	}
}

func obfuscateConfig(cfg *config.Config) *config.Config {
	out := *cfg
	out.Google.ProjectID = placeholderProjectID
	out.Google.ClientID = placeholderClientID
	out.Google.ClientSecret = placeholderSecret
	out.Google.RedirectURI = normalizeRedirect(out.Google.RedirectURI)
	if strings.TrimSpace(out.Google.PubSubSubscription) != "" {
		out.Google.PubSubSubscription = placeholderPubSub
	}
	if strings.TrimSpace(out.Google.ServiceAccountKey) != "" {
		out.Google.ServiceAccountKey = "pubsub-sa.json"
	}

	out.Cameras = make([]config.Camera, len(cfg.Cameras))
	for i, cam := range cfg.Cameras {
		cam.DeviceID = placeholderDeviceID(i + 1)
		out.Cameras[i] = cam
	}
	return &out
}

func placeholderDeviceID(n int) string {
	return fmt.Sprintf("enterprises/%s/devices/%024d", placeholderEnterprise, n)
}

func normalizeRedirect(uri string) string {
	u := strings.TrimSpace(uri)
	if u == "" || strings.Contains(u, "localhost") {
		return "http://127.0.0.1:8190/oauth2callback"
	}
	if strings.Contains(u, "127.0.0.1:8190") {
		return u
	}
	return "http://127.0.0.1:8190/oauth2callback"
}

func backupFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path+".bak", raw, 0o600)
}

func writeConfigYAML(path string, cfg *config.Config) error {
	doc := map[string]any{
		"google": map[string]any{
			"project_id":          cfg.Google.ProjectID,
			"client_id":           cfg.Google.ClientID,
			"client_secret":       cfg.Google.ClientSecret,
			"redirect_uri":        cfg.Google.RedirectURI,
			"pubsub_subscription": cfg.Google.PubSubSubscription,
			"service_account_key": cfg.Google.ServiceAccountKey,
		},
		"media": map[string]any{
			"rtsp_base_url": cfg.Media.RTSPBaseURL,
		},
		"events": map[string]any{
			"onvif": cfg.Events.Onvif,
		},
	}
	if listen := cfg.ViewerListen(); listen != "" && listen != "0.0.0.0:8090" {
		doc["viewer"] = map[string]any{"listen": cfg.Viewer.Listen}
	}
	cams := make([]map[string]any, 0, len(cfg.Cameras))
	for _, cam := range cfg.Cameras {
		entry := map[string]any{
			"device_id": cam.DeviceID,
			"name":      cam.Name,
			"onvif": map[string]string{
				"mac": cam.ONVIF.MAC,
				"ip":  cam.ONVIF.IP,
			},
		}
		if cam.Audio {
			entry["audio"] = true
		}
		if cam.EventsEnabled {
			entry["event"] = map[string]string{"linger": "60s"}
		}
		cams = append(cams, entry)
	}
	doc["cameras"] = cams

	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return err
	}
	_ = enc.Close()
	out := buf.String()

	if path == "-" {
		fmt.Print(out)
		return nil
	}
	return os.WriteFile(path, []byte(out), 0o600)
}

type draftCamera struct {
	DeviceID    string `yaml:"device_id"`
	Name        string `yaml:"name"`
	Selected    bool   `yaml:"selected"`
	Audio       bool   `yaml:"audio"`
	EventsOnvif bool   `yaml:"events_onvif"`
	MAC         string `yaml:"mac"`
	IP          string `yaml:"ip"`
}

type draftFile struct {
	Google  map[string]string `yaml:"google"`
	Events  map[string]any    `yaml:"events"`
	Network map[string]string `yaml:"network"`
	Cameras []draftCamera     `yaml:"cameras"`
}

func writeDraft(path string, cfg *config.Config) error {
	obf := obfuscateConfig(cfg)
	d := draftFile{
		Google: map[string]string{
			"project_id":          obf.Google.ProjectID,
			"client_id":           obf.Google.ClientID,
			"client_secret":       obf.Google.ClientSecret,
			"redirect_uri":        obf.Google.RedirectURI,
			"pubsub_subscription": obf.Google.PubSubSubscription,
		},
		Events: map[string]any{
			"onvif":               obf.Events.Onvif,
			"pubsub_subscription": obf.Google.PubSubSubscription,
		},
		Network: map[string]string{
			"host_ip":      "192.168.1.15",
			"parent_iface": "en0",
		},
	}
	for _, cam := range obf.Cameras {
		d.Cameras = append(d.Cameras, draftCamera{
			DeviceID:    cam.DeviceID,
			Name:        cam.Name,
			Selected:    true,
			Audio:       cam.Audio,
			EventsOnvif: cam.EventsEnabled,
			MAC:         cam.ONVIF.MAC,
			IP:          cam.ONVIF.IP,
		})
	}
	raw, err := yaml.Marshal(d)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func writeTokens(path string) error {
	tok := map[string]string{
		"access_token":  "ya29.obfuscated-access-token",
		"token_type":    "Bearer",
		"refresh_token": "1//0obfuscated-refresh-token",
		"expiry":        "2000-01-01T00:00:00Z",
	}
	raw, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, raw, 0o600)
}
