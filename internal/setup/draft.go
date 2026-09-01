package setup

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mustacheride/nest-to-ONVIF/internal/config"
)

const macPrefix = "02:4E:53:54:00"

var placeholderRE = regexp.MustCompile(`(00000000|xxxxxxxx|your-gcp-project)`)

type draftCamera struct {
	DeviceID string `yaml:"device_id"`
	Name     string `yaml:"name"`
	Audio    bool   `yaml:"audio,omitempty"`
	Event    *struct {
		Linger string `yaml:"linger"`
	} `yaml:"event,omitempty"`
	ONVIF config.ONVIFConfig `yaml:"onvif"`
}

type draftFile struct {
	Google  config.GoogleConfig `yaml:"google"`
	Media   config.MediaConfig  `yaml:"media"`
	Events  config.EventsConfig `yaml:"events"`
	Cameras []draftCamera       `yaml:"cameras"`
}

// Draft holds wizard state before it is written to disk.
type Draft struct {
	Google          GoogleInput
	Media           config.MediaConfig
	Events          EventsInput
	Network         NetworkInput
	Cameras         []CameraInput
	PubSubKey       []byte
	HasPubSubKey    bool
}

func defaultDraft() Draft {
	return Draft{
		Media: config.MediaConfig{RTSPBaseURL: "rtsp://127.0.0.1:8554"},
		Network: NetworkInput{},
	}
}

// FindRepoRoot locates the repository root containing deploy/docker-compose.yml.
func FindRepoRoot() (string, error) {
	if root := os.Getenv("NEST_BRIDGE_ROOT"); root != "" {
		if _, err := os.Stat(filepath.Join(root, "deploy", "docker-compose.yml")); err == nil {
			return root, nil
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "deploy", "docker-compose.yml")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("could not find repository root (no deploy/docker-compose.yml)")
}

type wizardDraftFile struct {
	Google       GoogleInput        `yaml:"google"`
	Media        config.MediaConfig `yaml:"media"`
	Events       EventsInput        `yaml:"events"`
	Network      NetworkInput       `yaml:"network"`
	Cameras      []CameraInput      `yaml:"cameras"`
	HasPubSubKey bool               `yaml:"has_pubsub_key"`
}

func wizardDraftPath(repoRoot string) string {
	return filepath.Join(repoRoot, "setup-draft.yaml")
}

func setupPubsubKeyPath(repoRoot string) string {
	return filepath.Join(repoRoot, "setup-pubsub-sa.json")
}

func loadWizardDraft(repoRoot, cfgPath string) (Draft, error) {
	d, err := loadDraftFromFile(cfgPath)
	if err != nil {
		return defaultDraft(), err
	}

	draftPath := wizardDraftPath(repoRoot)
	raw, err := os.ReadFile(draftPath)
	if err != nil {
		if os.IsNotExist(err) {
			applyDeployEnv(&d, filepath.Join(repoRoot, "deploy"))
			loadExistingPubSubKey(repoRoot, &d)
			return d, nil
		}
		return d, err
	}
	var w wizardDraftFile
	if err := yaml.Unmarshal(raw, &w); err != nil {
		return d, fmt.Errorf("parse setup draft: %w", err)
	}
	mergeWizardDraft(&d, w)
	if w.HasPubSubKey {
		if b, err := os.ReadFile(setupPubsubKeyPath(repoRoot)); err == nil {
			d.PubSubKey = b
			d.HasPubSubKey = true
		} else {
			d.HasPubSubKey = false
		}
	}
	applyDeployEnv(&d, filepath.Join(repoRoot, "deploy"))
	loadExistingPubSubKey(repoRoot, &d)
	d.Events.Onvif = anyCameraEventsOnvif(d.Cameras)
	return d, nil
}

func mergeWizardDraft(d *Draft, w wizardDraftFile) {
	if strings.TrimSpace(w.Google.ProjectID) != "" {
		d.Google = w.Google
	}
	if w.Media.RTSPBaseURL != "" {
		d.Media = w.Media
	}
	if strings.TrimSpace(w.Events.PubSubSubscription) != "" || w.Events.Onvif {
		d.Events = w.Events
	}
	if strings.TrimSpace(w.Network.HostIP) != "" || strings.TrimSpace(w.Network.ParentIface) != "" {
		if strings.TrimSpace(w.Network.HostIP) != "" {
			d.Network.HostIP = w.Network.HostIP
		}
		if strings.TrimSpace(w.Network.ParentIface) != "" {
			d.Network.ParentIface = w.Network.ParentIface
		}
	}
	if len(w.Cameras) > 0 {
		d.Cameras = w.Cameras
	}
}

func applyDeployEnv(d *Draft, deployDir string) {
	env := loadNetworkFromDeployEnv(deployDir)
	if strings.TrimSpace(d.Network.HostIP) == "" && env.HostIP != "" {
		d.Network.HostIP = env.HostIP
	}
	if strings.TrimSpace(d.Network.ParentIface) == "" && env.ParentIface != "" {
		d.Network.ParentIface = env.ParentIface
	}
}

func loadNetworkFromDeployEnv(deployDir string) NetworkInput {
	raw, err := os.ReadFile(filepath.Join(deployDir, "deploy.env"))
	if err != nil {
		return NetworkInput{}
	}
	var n NetworkInput
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.Trim(value, `"'`)
		switch strings.TrimSpace(key) {
		case "HOST_IP":
			n.HostIP = value
		case "PARENT_IFACE":
			n.ParentIface = value
		}
	}
	return n
}

func loadExistingPubSubKey(repoRoot string, d *Draft) {
	if d.HasPubSubKey && len(d.PubSubKey) > 0 {
		return
	}
	for _, path := range []string{
		setupPubsubKeyPath(repoRoot),
		filepath.Join(repoRoot, "deploy", "config", "pubsub-sa.json"),
	} {
		b, err := os.ReadFile(path)
		if err != nil || len(b) < 3 {
			continue
		}
		trimmed := strings.TrimSpace(string(b))
		if trimmed == "" || trimmed == "{}" {
			continue
		}
		d.PubSubKey = b
		d.HasPubSubKey = true
		return
	}
}

func saveWizardDraft(repoRoot string, d *Draft) error {
	d.Events.Onvif = anyCameraEventsOnvif(d.Cameras)
	body, err := yaml.Marshal(wizardDraftFile{
		Google:       d.Google,
		Media:        d.Media,
		Events:       d.Events,
		Network:      d.Network,
		Cameras:      d.Cameras,
		HasPubSubKey: d.HasPubSubKey,
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(wizardDraftPath(repoRoot), body, 0o600); err != nil {
		return err
	}
	pubsubPath := setupPubsubKeyPath(repoRoot)
	if len(d.PubSubKey) > 0 {
		return os.WriteFile(pubsubPath, d.PubSubKey, 0o400)
	}
	if d.HasPubSubKey {
		return nil
	}
	_ = os.Remove(pubsubPath)
	return nil
}

func loadDraftFromFile(path string) (Draft, error) {
	d := defaultDraft()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return d, nil
		}
		return d, err
	}
	var file draftFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return d, fmt.Errorf("parse config: %w", err)
	}
	d.Google = GoogleInput{
		ProjectID:    file.Google.ProjectID,
		ClientID:     file.Google.ClientID,
		ClientSecret: file.Google.ClientSecret,
		RedirectURI:  file.Google.RedirectURI,
	}
	if file.Media.RTSPBaseURL != "" {
		d.Media = file.Media
	}
	d.Events = EventsInput{
		Onvif:              file.Events.Onvif,
		PubSubSubscription: file.Google.PubSubSubscription,
	}
	for _, cam := range file.Cameras {
		linger := ""
		eventsOnvif := cam.Event != nil
		if cam.Event != nil {
			linger = cam.Event.Linger
		}
		d.Cameras = append(d.Cameras, CameraInput{
			DeviceID:    cam.DeviceID,
			Name:        cam.Name,
			Selected:    true,
			Audio:       cam.Audio,
			EventsOnvif: eventsOnvif,
			Linger:      linger,
			MAC:         cam.ONVIF.MAC,
			IP:          cam.ONVIF.IP,
		})
	}
	return d, nil
}

func (d *Draft) toFile() draftFile {
	file := draftFile{
		Google: config.GoogleConfig{
			ProjectID:          strings.TrimSpace(d.Google.ProjectID),
			ClientID:           strings.TrimSpace(d.Google.ClientID),
			ClientSecret:       strings.TrimSpace(d.Google.ClientSecret),
			RedirectURI:        strings.TrimSpace(d.Google.RedirectURI),
			PubSubSubscription: strings.TrimSpace(d.Events.PubSubSubscription),
		},
		Media:  d.Media,
		Events: config.EventsConfig{Onvif: d.anyCameraEventsOnvif()},
	}
	if d.anyCameraEventsOnvif() {
		file.Google.ServiceAccountKey = "pubsub-sa.json"
	}
	for _, cam := range d.selectedCameras() {
		entry := draftCamera{
			DeviceID: cam.DeviceID,
			Name:     cam.Name,
			Audio:    cam.Audio,
			ONVIF:    config.ONVIFConfig{MAC: cam.MAC, IP: cam.IP},
		}
		if cam.EventsOnvif {
			linger := cam.Linger
			if linger == "" {
				linger = "60s"
			}
			entry.Event = &struct {
				Linger string `yaml:"linger"`
			}{Linger: linger}
		}
		file.Cameras = append(file.Cameras, entry)
	}
	return file
}

func (d *Draft) anyCameraEventsOnvif() bool {
	return anyCameraEventsOnvif(d.selectedCameras())
}

func anyCameraEventsOnvif(cameras []CameraInput) bool {
	for _, cam := range cameras {
		if cam.Selected && cam.EventsOnvif {
			return true
		}
	}
	return false
}

func (d *Draft) selectedCameras() []CameraInput {
	var out []CameraInput
	for _, cam := range d.Cameras {
		if cam.Selected {
			out = append(out, cam)
		}
	}
	return out
}

func (d *Draft) validate() error {
	g := d.Google
	if strings.TrimSpace(g.ProjectID) == "" {
		return fmt.Errorf("google.project_id is required")
	}
	if placeholderRE.MatchString(g.ProjectID) || placeholderRE.MatchString(g.ClientID) {
		return fmt.Errorf("replace placeholder Google credentials")
	}
	if strings.TrimSpace(g.ClientID) == "" || strings.TrimSpace(g.ClientSecret) == "" {
		return fmt.Errorf("google client_id and client_secret are required")
	}
	if strings.TrimSpace(g.RedirectURI) == "" {
		return fmt.Errorf("google.redirect_uri is required")
	}
	selected := d.selectedCameras()
	if len(selected) == 0 {
		return fmt.Errorf("select at least one camera")
	}
	macs := map[string]string{}
	ips := map[string]string{}
	for _, cam := range selected {
		if cam.DeviceID == "" || cam.Name == "" {
			return fmt.Errorf("every camera needs a device_id and name")
		}
		if cam.MAC == "" || cam.IP == "" {
			return fmt.Errorf("camera %q needs ONVIF mac and ip", cam.Name)
		}
		parsed, err := net.ParseMAC(cam.MAC)
		if err != nil {
			return fmt.Errorf("camera %q: invalid mac %q", cam.Name, cam.MAC)
		}
		macKey := parsed.String()
		if prev, dup := macs[macKey]; dup {
			return fmt.Errorf("duplicate mac %s on %q and %q", cam.MAC, prev, cam.Name)
		}
		if net.ParseIP(cam.IP) == nil {
			return fmt.Errorf("camera %q: invalid ip %q", cam.Name, cam.IP)
		}
		if prev, dup := ips[cam.IP]; dup {
			return fmt.Errorf("duplicate ip %s on %q and %q", cam.IP, prev, cam.Name)
		}
		macs[macKey] = cam.Name
		ips[cam.IP] = cam.Name
	}
	if d.anyCameraEventsOnvif() {
		if strings.TrimSpace(d.Events.PubSubSubscription) == "" {
			return fmt.Errorf("pubsub_subscription is required when any camera has ONVIF motion events enabled")
		}
		if !d.HasPubSubKey {
			return fmt.Errorf("upload a Pub/Sub service-account key when any camera has ONVIF motion events enabled")
		}
	}
	return nil
}

func (d *Draft) writeConfig(path string) error {
	if err := d.validate(); err != nil {
		return err
	}
	body, err := yaml.Marshal(d.toFile())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

func (d *Draft) view() ConfigView {
	file := d.toFile()
	out := ConfigView{
		Google: file.Google,
		Media:  file.Media,
		Events: file.Events,
	}
	for _, cam := range file.Cameras {
		linger := ""
		if cam.Event != nil {
			linger = cam.Event.Linger
		}
		out.Cameras = append(out.Cameras, CameraView{
			DeviceID:    cam.DeviceID,
			Name:        cam.Name,
			Audio:       cam.Audio,
			EventsOnvif: cam.Event != nil,
			Linger:      linger,
			ONVIF:       cam.ONVIF,
		})
	}
	out.Google.ClientSecret = redact(out.Google.ClientSecret)
	return out
}

func redact(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "••••"
	}
	return s[:4] + "••••" + s[len(s)-4:]
}

func formatMAC(index int) string {
	return fmt.Sprintf("%s:%02X", macPrefix, index)
}

func finalizeCameras(cameras []CameraInput) ([]CameraInput, error) {
	usedMAC := map[string]struct{}{}
	usedIP := map[string]struct{}{}
	for _, cam := range cameras {
		if !cam.Selected {
			continue
		}
		if cam.IP == "" {
			return nil, fmt.Errorf("camera %q needs an ONVIF IP address", cam.Name)
		}
		if net.ParseIP(cam.IP) == nil || net.ParseIP(cam.IP).To4() == nil {
			return nil, fmt.Errorf("camera %q: invalid IPv4 address %q", cam.Name, cam.IP)
		}
		if _, dup := usedIP[cam.IP]; dup {
			return nil, fmt.Errorf("duplicate IP %s", cam.IP)
		}
		usedIP[cam.IP] = struct{}{}
		if cam.MAC != "" {
			parsed, err := net.ParseMAC(cam.MAC)
			if err != nil {
				return nil, fmt.Errorf("camera %q: invalid mac %q", cam.Name, cam.MAC)
			}
			macKey := strings.ToLower(parsed.String())
			if _, dup := usedMAC[macKey]; dup {
				return nil, fmt.Errorf("duplicate MAC %s", cam.MAC)
			}
			usedMAC[macKey] = struct{}{}
		}
	}

	macIdx := 1
	for i := range cameras {
		if !cameras[i].Selected || cameras[i].MAC != "" {
			continue
		}
		for {
			candidate := formatMAC(macIdx)
			macIdx++
			if _, ok := usedMAC[strings.ToLower(candidate)]; !ok {
				cameras[i].MAC = candidate
				usedMAC[strings.ToLower(candidate)] = struct{}{}
				break
			}
			if macIdx > 255 {
				return nil, fmt.Errorf("no free virtual MAC addresses left")
			}
		}
	}
	return cameras, nil
}

func normalizeRedirectURI(uri string) string {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return "http://127.0.0.1:8190/oauth2callback"
	}
	if strings.Contains(uri, "localhost") {
		return "http://127.0.0.1:8190/oauth2callback"
	}
	return uri
}

func redirectURIFromListen(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://127.0.0.1:8190/oauth2callback"
	}
	if host == "" || host == "0.0.0.0" || host == "localhost" {
		host = "127.0.0.1"
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%s/oauth2callback", host, port)
}
