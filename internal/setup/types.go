package setup

import "github.com/mustacheride/nest-to-ONVIF/internal/config"

// Status is returned by GET /api/status.
type Status struct {
	Linux          bool   `json:"linux"`
	Docker         bool   `json:"docker"`
	DockerMsg      string `json:"docker_msg,omitempty"`
	BridgeBuilt    bool   `json:"bridge_built"`
	Configured     bool   `json:"configured"`
	ConfigSaved    bool   `json:"config_saved"`
	Deployed       bool   `json:"deployed"`
	Authorized     bool   `json:"authorized"`
	CameraCount    int    `json:"camera_count"`
	HostIP         string `json:"host_ip"`
	DetectedHostIP string `json:"detected_host_ip"`
	ParentIface    string `json:"parent_iface"`
	LastDeploy     string `json:"last_deploy,omitempty"`
}

// GoogleInput is the credentials step payload.
type GoogleInput struct {
	ProjectID    string `json:"project_id"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RedirectURI  string `json:"redirect_uri"`
}

// NetworkInput holds deployment host networking choices.
type NetworkInput struct {
	HostIP      string `json:"host_ip"`
	ParentIface string `json:"parent_iface"`
}

// EventsInput holds optional Pub/Sub event forwarding settings.
type EventsInput struct {
	Onvif              bool   `json:"onvif"`
	PubSubSubscription string `json:"pubsub_subscription"`
}

// CameraInput is one camera the operator chose to deploy.
type CameraInput struct {
	DeviceID    string `json:"device_id"`
	Name        string `json:"name"`
	Selected    bool   `json:"selected"`
	Audio       bool   `json:"audio"`
	EventsOnvif bool   `json:"events_onvif"`
	Linger      string `json:"linger,omitempty"`
	MAC         string `json:"mac,omitempty"`
	IP          string `json:"ip,omitempty"`
	Type        string `json:"type,omitempty"`      // UI metadata; not written to config.yaml
	Protocols   string `json:"protocols,omitempty"` // UI metadata; not written to config.yaml
}

// CamerasInput is the camera selection step payload.
type CamerasInput struct {
	Cameras []CameraInput `json:"cameras"`
}

// DeviceInfo is a Nest device returned to the UI.
type DeviceInfo struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	Protocols []string `json:"protocols"`
	DeviceID  string   `json:"device_id"`
}

// ConfigView is a redacted config snapshot for the review step.
type ConfigView struct {
	Google  config.GoogleConfig `json:"google"`
	Media   config.MediaConfig  `json:"media"`
	Events  config.EventsConfig `json:"events"`
	Cameras []CameraView        `json:"cameras"`
}

// CameraView is one configured camera for review.
type CameraView struct {
	DeviceID    string             `json:"device_id"`
	Name        string             `json:"name"`
	Audio       bool               `json:"audio"`
	EventsOnvif bool               `json:"events_onvif"`
	Linger      string             `json:"linger,omitempty"`
	ONVIF       config.ONVIFConfig `json:"onvif"`
}

// AuthStartResponse is returned by POST /api/auth/start.
type AuthStartResponse struct {
	URL string `json:"url"`
}

// DeployResult is returned by POST /api/deploy.
type DeployResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Log     string `json:"log"`
}

// errorResponse is the standard API error body.
type errorResponse struct {
	Error string `json:"error"`
}
