package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

const valid = `
google:
  project_id: "11111111-2222-3333-4444-555555555555"
  client_id: "abc.apps.googleusercontent.com"
  client_secret: "secret"
cameras:
  - device_id: "enterprises/p/devices/a"
    name: "Front Door"
    audio: true
    onvif: { mac: "02:4E:53:54:00:01", ip: "192.168.1.8" }
  - device_id: "enterprises/p/devices/b"
    name: "Driveway"
    event: { linger: 90s }
    onvif: { mac: "02:4E:53:54:00:02", ip: "192.168.1.9" }
`

func TestLoadValid(t *testing.T) {
	c, err := Load(write(t, valid))
	require.NoError(t, err)
	require.Len(t, c.Cameras, 2)
	assert.True(t, c.Cameras[0].Audio)
	assert.False(t, c.Cameras[0].EventsEnabled)
	assert.Equal(t, time.Duration(0), c.Cameras[0].Linger)
	assert.True(t, c.Cameras[1].EventsEnabled)
	assert.Equal(t, 90*time.Second, c.Cameras[1].Linger)
}

func TestDefaultsApplied(t *testing.T) {
	c, err := Load(write(t, `
google:
  project_id: "p"
  client_id: "c"
  client_secret: "s"
cameras:
  - device_id: "d"
    name: "Cam"
    onvif: { mac: "02:4E:53:54:00:01", ip: "192.168.1.8" }
`))
	require.NoError(t, err)
	assert.False(t, c.Cameras[0].Audio)
	assert.False(t, c.Cameras[0].EventsEnabled)
	assert.Equal(t, time.Duration(0), c.Cameras[0].Linger)
	assert.Equal(t, "http://127.0.0.1:8190/oauth2callback", c.Google.RedirectURI)
}

func TestRejectsDuplicateMAC(t *testing.T) {
	_, err := Load(write(t, `
google: { project_id: p, client_id: c, client_secret: s }
cameras:
  - device_id: a
    name: A
    onvif: { mac: "02:4E:53:54:00:01", ip: "192.168.1.8" }
  - device_id: b
    name: B
    onvif: { mac: "02:4E:53:54:00:01", ip: "192.168.1.9" }
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate MAC")
}

// The ONVIF identity is derived from the canonical MAC, so two spellings of one
// address are one device to Protect. Validation must reject the pair rather than
// let both cameras through to collide after adoption.
func TestRejectsDuplicateMACDifferingOnlyInSpelling(t *testing.T) {
	for _, spelling := range []string{"02:4e:53:54:00:01", "02-4E-53-54-00-01"} {
		_, err := Load(write(t, `
google: { project_id: p, client_id: c, client_secret: s }
cameras:
  - device_id: a
    name: A
    onvif: { mac: "02:4E:53:54:00:01", ip: "192.168.1.8" }
  - device_id: b
    name: B
    onvif: { mac: "`+spelling+`", ip: "192.168.1.9" }
`))
		require.Error(t, err, spelling)
		assert.Contains(t, err.Error(), "duplicate MAC", spelling)
	}
}

func TestRejectsDuplicateIP(t *testing.T) {
	_, err := Load(write(t, `
google: { project_id: p, client_id: c, client_secret: s }
cameras:
  - device_id: a
    name: A
    onvif: { mac: "02:4E:53:54:00:01", ip: "192.168.1.8" }
  - device_id: b
    name: B
    onvif: { mac: "02:4E:53:54:00:02", ip: "192.168.1.8" }
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate IP")
}

func TestRejectsMissingCredentials(t *testing.T) {
	_, err := Load(write(t, `
google: { project_id: "", client_id: c, client_secret: s }
cameras: []
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "project_id")
}

func TestRejectsMalformedLinger(t *testing.T) {
	_, err := Load(write(t, `
google: { project_id: p, client_id: c, client_secret: s }
cameras:
  - device_id: a
    name: "Front Door"
    event: { linger: not-a-duration }
    onvif: { mac: "02:4E:53:54:00:01", ip: "192.168.1.8" }
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Front Door")
	assert.Contains(t, err.Error(), "not-a-duration")
}

func TestCameraPathNameSlugifiesName(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"Front doorbell", "cam-front-doorbell"},
		{"Side yard", "cam-side-yard"},
		{"Chicken Roost", "cam-chicken-roost"},
		{"A  B", "cam-a-b"},
		{"Drive/way", "cam-drive-way"},
		{"  Padded  ", "cam-padded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cam := Camera{Name: tc.name}
			assert.Equal(t, tc.want, cam.PathName())
		})
	}
}

func TestCameraPublishURLJoinsBaseAndPath(t *testing.T) {
	cam := Camera{Name: "Front doorbell"}
	assert.Equal(t,
		"rtsp://127.0.0.1:8554/cam-front-doorbell",
		cam.PublishURL("rtsp://127.0.0.1:8554"))
	assert.Equal(t,
		"rtsp://127.0.0.1:8554/cam-front-doorbell",
		cam.PublishURL("rtsp://127.0.0.1:8554/"))
}

func TestLoadRejectsCamerasWithCollidingPathNames(t *testing.T) {
	path := write(t, `
google:
  project_id: "p"
  client_id: "c"
  client_secret: "s"
  redirect_uri: "http://localhost:8190/oauth2callback"
media:
  rtsp_base_url: "rtsp://127.0.0.1:8554"
cameras:
  - device_id: "enterprises/p/devices/1"
    name: "Side yard"
    onvif: { mac: "02:4E:53:54:00:01", ip: "192.168.1.8" }
  - device_id: "enterprises/p/devices/2"
    name: "side  YARD"
    onvif: { mac: "02:4E:53:54:00:02", ip: "192.168.1.9" }
`)
	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cam-side-yard")
}

func TestLoadDefaultsRTSPBaseURL(t *testing.T) {
	cfg, err := Load(write(t, valid))
	require.NoError(t, err)
	assert.Equal(t, "rtsp://127.0.0.1:8554", cfg.Media.RTSPBaseURL)
}

func TestEventsOnvifDisabledByDefault(t *testing.T) {
	cfg, err := Load(write(t, valid))
	require.NoError(t, err)
	assert.False(t, cfg.Events.Onvif)
}
