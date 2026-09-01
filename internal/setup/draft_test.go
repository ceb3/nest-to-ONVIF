package setup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFinalizeCamerasAutoFillsMAC(t *testing.T) {
	cams := []CameraInput{
		{DeviceID: "a", Name: "Front", Selected: true, IP: "192.168.1.8"},
		{DeviceID: "b", Name: "Back", Selected: true, IP: "192.168.1.9"},
	}
	out, err := finalizeCameras(cams)
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Equal(t, "02:4E:53:54:00:01", out[0].MAC)
	assert.Equal(t, "02:4E:53:54:00:02", out[1].MAC)
}

func TestFinalizeCamerasRequiresIP(t *testing.T) {
	_, err := finalizeCameras([]CameraInput{
		{DeviceID: "a", Name: "Front", Selected: true},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ONVIF IP")
}

func TestDraftValidateRejectsDuplicateIP(t *testing.T) {
	d := Draft{
		Google: GoogleInput{
			ProjectID:    "11111111-2222-3333-4444-555555555555",
			ClientID:     "abc.apps.googleusercontent.com",
			ClientSecret: "secret",
			RedirectURI:  "http://localhost:8190/oauth2callback",
		},
		Cameras: []CameraInput{
			{DeviceID: "a", Name: "A", Selected: true, MAC: "02:4E:53:54:00:01", IP: "192.168.1.8"},
			{DeviceID: "b", Name: "B", Selected: true, MAC: "02:4E:53:54:00:02", IP: "192.168.1.8"},
		},
	}
	err := d.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate ip")
}

func TestDraftWritesPerCameraEvents(t *testing.T) {
	d := Draft{
		Google: GoogleInput{
			ProjectID:    "11111111-2222-3333-4444-555555555555",
			ClientID:     "abc.apps.googleusercontent.com",
			ClientSecret: "secret",
			RedirectURI:  "http://localhost:8190/oauth2callback",
		},
		Events: EventsInput{
			PubSubSubscription: "projects/p/subscriptions/sdm-events",
		},
		HasPubSubKey: true,
		Cameras: []CameraInput{
			{DeviceID: "a", Name: "Front", Selected: true, MAC: "02:4E:53:54:00:01", IP: "192.168.1.8", EventsOnvif: true},
			{DeviceID: "b", Name: "Back", Selected: true, MAC: "02:4E:53:54:00:02", IP: "192.168.1.9"},
		},
	}
	file := d.toFile()
	require.True(t, file.Events.Onvif)
	require.Len(t, file.Cameras, 2)
	require.NotNil(t, file.Cameras[0].Event)
	assert.Equal(t, "60s", file.Cameras[0].Event.Linger)
	assert.Nil(t, file.Cameras[1].Event)
}

func TestNormalizeRedirectURI(t *testing.T) {
	assert.Equal(t, "http://127.0.0.1:8190/oauth2callback", normalizeRedirectURI(""))
	assert.Equal(t, "http://127.0.0.1:8190/oauth2callback", normalizeRedirectURI("http://localhost:8190/oauth2callback"))
	assert.Equal(t, "http://127.0.0.1:8190/oauth2callback", normalizeRedirectURI("http://127.0.0.1:8190/oauth2callback"))
}

func TestSaveAndLoadWizardDraft(t *testing.T) {
	dir := t.TempDir()
	d := Draft{
		Google: GoogleInput{
			ProjectID:    "11111111-2222-3333-4444-555555555555",
			ClientID:     "abc.apps.googleusercontent.com",
			ClientSecret: "secret",
			RedirectURI:  "http://localhost:8190/oauth2callback",
		},
		Events:  EventsInput{PubSubSubscription: "projects/p/subscriptions/sdm-events"},
		Network: NetworkInput{HostIP: "192.168.1.15", ParentIface: "eth0"},
		Cameras: []CameraInput{
			{DeviceID: "a", Name: "Front", Selected: true, MAC: "02:4E:53:54:00:01", IP: "192.168.1.8"},
		},
		PubSubKey:    []byte(`{"type":"service_account"}`),
		HasPubSubKey: true,
	}
	require.NoError(t, saveWizardDraft(dir, &d))
	got, err := loadWizardDraft(dir, filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	assert.Equal(t, d.Google.ProjectID, got.Google.ProjectID)
	assert.Equal(t, d.Network.HostIP, got.Network.HostIP)
	require.Len(t, got.Cameras, 1)
	assert.True(t, got.HasPubSubKey)
}

func TestLoadWizardDraftFromConfigAndDeployEnv(t *testing.T) {
	dir := t.TempDir()
	deployDir := filepath.Join(dir, "deploy")
	require.NoError(t, os.MkdirAll(deployDir, 0o755))
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
google:
  project_id: "11111111-2222-3333-4444-555555555555"
  client_id: "abc.apps.googleusercontent.com"
  client_secret: "secret"
  redirect_uri: "http://127.0.0.1:8190/oauth2callback"
cameras:
  - device_id: "a"
    name: "Front"
    onvif: { mac: "02:4E:53:54:00:01", ip: "192.168.1.8" }
`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(deployDir, "deploy.env"), []byte(`HOST_IP="192.168.1.15"
PARENT_IFACE="eth0"
`), 0o644))

	got, err := loadWizardDraft(dir, cfgPath)
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.15", got.Network.HostIP)
	assert.Equal(t, "eth0", got.Network.ParentIface)
	require.Len(t, got.Cameras, 1)
	assert.True(t, got.Cameras[0].Selected)
}

func TestMergeWizardDraftKeepsConfigWhenDraftCamerasEmpty(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
google:
  project_id: "11111111-2222-3333-4444-555555555555"
  client_id: "abc.apps.googleusercontent.com"
  client_secret: "secret"
  redirect_uri: "http://127.0.0.1:8190/oauth2callback"
cameras:
  - device_id: "a"
    name: "Front"
    onvif: { mac: "02:4E:53:54:00:01", ip: "192.168.1.8" }
`), 0o600))
	require.NoError(t, saveWizardDraft(dir, &Draft{
		Google:  GoogleInput{ProjectID: "draft-project"},
		Network: NetworkInput{HostIP: "192.168.1.20"},
	}))

	got, err := loadWizardDraft(dir, cfgPath)
	require.NoError(t, err)
	assert.Equal(t, "draft-project", got.Google.ProjectID)
	assert.Equal(t, "192.168.1.20", got.Network.HostIP)
	require.Len(t, got.Cameras, 1)
	assert.Equal(t, "Front", got.Cameras[0].Name)
}

func TestRedirectURIFromListen(t *testing.T) {
	assert.Equal(t, "http://127.0.0.1:8190/oauth2callback", redirectURIFromListen("127.0.0.1:8190"))
	assert.Equal(t, "http://127.0.0.1:8190/oauth2callback", redirectURIFromListen("0.0.0.0:8190"))
	assert.Equal(t, "http://127.0.0.1:8190/oauth2callback", redirectURIFromListen("localhost:8190"))
}
