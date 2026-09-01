// Package config loads and validates the bridge's YAML configuration.
package config

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const defaultRedirectURI = "http://127.0.0.1:8190/oauth2callback"
const defaultLinger = 60 * time.Second
const defaultRTSPBaseURL = "rtsp://127.0.0.1:8554"

var nonSlugChars = regexp.MustCompile(`[^a-z0-9]+`)

type GoogleConfig struct {
	ProjectID          string `yaml:"project_id"`
	ClientID           string `yaml:"client_id"`
	ClientSecret       string `yaml:"client_secret"`
	RedirectURI        string `yaml:"redirect_uri"`
	PubSubSubscription string `yaml:"pubsub_subscription"`
	// ServiceAccountKey is the path to the Pub/Sub service-account key. It is a
	// path rather than the key itself because Google's download filename embeds
	// the project and key ID, so it matches no predictable pattern and must not
	// appear in a committed file.
	ServiceAccountKey string `yaml:"service_account_key"`
}

type ONVIFConfig struct {
	MAC string `yaml:"mac"`
	IP  string `yaml:"ip"`
}

type MediaConfig struct {
	RTSPBaseURL string `yaml:"rtsp_base_url"`
}

// ViewerConfig controls the optional LAN live-view page served alongside
// nest-bridge serve. Stream URLs are derived from the browser's host plus the
// standard MediaMTX HLS and snapshot ports.
type ViewerConfig struct {
	Listen string `yaml:"listen"`
}

const (
	defaultViewerListen = "0.0.0.0:8090"
	defaultHLSPort      = "8888"
	defaultSnapshotPort = "8080"
)

// ViewerListen returns the viewer HTTP listen address, or "" when disabled.
func (c *Config) ViewerListen() string {
	switch strings.TrimSpace(c.Viewer.Listen) {
	case "", "off", "false", "disable", "disabled":
		if c.Viewer.Listen == "" {
			return defaultViewerListen
		}
		return ""
	default:
		return strings.TrimSpace(c.Viewer.Listen)
	}
}

// HLSPort is the MediaMTX HLS port exposed on the deployment host.
func HLSPort() string { return defaultHLSPort }

// SnapshotPort is the nginx snapshot port on the deployment host.
func SnapshotPort() string { return defaultSnapshotPort }

type Camera struct {
	DeviceID      string        `yaml:"device_id"`
	Name          string        `yaml:"name"`
	Audio         bool          `yaml:"audio"`
	EventsEnabled bool          `yaml:"-"`
	Linger        time.Duration `yaml:"-"`
	ONVIF         ONVIFConfig   `yaml:"onvif"`
}

func (c *Camera) UnmarshalYAML(value *yaml.Node) error {
	type cameraYAML struct {
		DeviceID string `yaml:"device_id"`
		Name     string `yaml:"name"`
		Audio    bool   `yaml:"audio"`
		Event    *struct {
			Linger string `yaml:"linger"`
		} `yaml:"event"`
		ONVIF ONVIFConfig `yaml:"onvif"`
	}
	var raw cameraYAML
	if err := value.Decode(&raw); err != nil {
		return err
	}
	c.DeviceID = raw.DeviceID
	c.Name = raw.Name
	c.Audio = raw.Audio
	c.ONVIF = raw.ONVIF
	if raw.Event != nil {
		c.EventsEnabled = true
		if raw.Event.Linger != "" {
			d, err := time.ParseDuration(raw.Event.Linger)
			if err != nil {
				return fmt.Errorf("camera %q: invalid linger duration %q", raw.Name, raw.Event.Linger)
			}
			c.Linger = d
		}
	}
	return nil
}

// PathName returns the MediaMTX path for this camera. Camera names are
// human-chosen and may contain spaces, capitals, and punctuation; RTSP paths
// may not.
func (c Camera) PathName() string {
	slug := nonSlugChars.ReplaceAllString(strings.ToLower(c.Name), "-")
	return "cam-" + strings.Trim(slug, "-")
}

func (c Camera) PublishURL(base string) string {
	return strings.TrimSuffix(base, "/") + "/" + c.PathName()
}

// EventsConfig controls whether Nest detections pulled from Pub/Sub are
// forwarded to the virtual cameras' ONVIF motion endpoints. It is off by
// default because many Protect consoles — including 7.2.105 on a UDR7 —
// ignore third-party ONVIF motion while still detecting motion from the RTSP
// stream themselves.
type EventsConfig struct {
	Onvif bool `yaml:"onvif"`
}

type Config struct {
	Google  GoogleConfig `yaml:"google"`
	Media   MediaConfig  `yaml:"media"`
	Viewer  ViewerConfig `yaml:"viewer"`
	Events  EventsConfig `yaml:"events"`
	Cameras []Camera     `yaml:"cameras"`
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.applyDefaults(); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() error {
	if c.Google.RedirectURI == "" {
		c.Google.RedirectURI = defaultRedirectURI
	}
	if c.Media.RTSPBaseURL == "" {
		c.Media.RTSPBaseURL = defaultRTSPBaseURL
	}
	for i := range c.Cameras {
		cam := &c.Cameras[i]
		if cam.EventsEnabled && cam.Linger == 0 {
			cam.Linger = defaultLinger
		}
	}
	return nil
}

func (c *Config) validate() error {
	if c.Google.ProjectID == "" {
		return fmt.Errorf("google.project_id is required")
	}
	if c.Google.ClientID == "" {
		return fmt.Errorf("google.client_id is required")
	}
	if c.Google.ClientSecret == "" {
		return fmt.Errorf("google.client_secret is required")
	}

	macs := map[string]string{}
	ips := map[string]string{}
	paths := map[string]string{}
	for _, cam := range c.Cameras {
		if cam.DeviceID == "" {
			return fmt.Errorf("camera %q: device_id is required", cam.Name)
		}
		// Compare MACs in their canonical form. Protect derives one identity from
		// 02:4e:...:01 and 02:4E:...:01 alike, so a case difference between two
		// entries must be caught here rather than colliding after adoption.
		parsedMAC, err := net.ParseMAC(cam.ONVIF.MAC)
		if err != nil {
			return fmt.Errorf("camera %q: invalid MAC %q", cam.Name, cam.ONVIF.MAC)
		}
		macKey := parsedMAC.String()
		if net.ParseIP(cam.ONVIF.IP) == nil {
			return fmt.Errorf("camera %q: invalid IP %q", cam.Name, cam.ONVIF.IP)
		}
		if prev, dup := macs[macKey]; dup {
			return fmt.Errorf("duplicate MAC %s shared by %q and %q", cam.ONVIF.MAC, prev, cam.Name)
		}
		if prev, dup := ips[cam.ONVIF.IP]; dup {
			return fmt.Errorf("duplicate IP %s shared by %q and %q", cam.ONVIF.IP, prev, cam.Name)
		}
		path := cam.PathName()
		if prev, dup := paths[path]; dup {
			return fmt.Errorf("duplicate path %s shared by %q and %q", path, prev, cam.Name)
		}
		macs[macKey] = cam.Name
		ips[cam.ONVIF.IP] = cam.Name
		paths[path] = cam.Name
	}
	return nil
}
