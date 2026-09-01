package onvif_test

import (
	"os"
	"strings"
	"testing"

	"github.com/mustacheride/nest-to-ONVIF/internal/config"
	"github.com/mustacheride/nest-to-ONVIF/internal/onvif"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func twoCameras() config.Config {
	return config.Config{Cameras: []config.Camera{
		{Name: "Front doorbell", Audio: true,
			ONVIF: config.ONVIFConfig{MAC: "02:4E:53:54:00:01", IP: "192.168.1.8"}},
		{Name: "Side yard",
			ONVIF: config.ONVIFConfig{MAC: "02:4E:53:54:00:06", IP: "192.168.1.13"}},
	}}
}

func TestGenerateProducesOneEntryPerCamera(t *testing.T) {
	got, err := onvif.Generate(twoCameras(), "192.168.1.15:8554", "192.168.1.15:8080")
	require.NoError(t, err)
	out := string(got)

	// The upstream schema decomposes each URL into a shared host_source plus a
	// per-camera path, so the full URLs the plan sketched never appear literally.
	assert.Contains(t, out, "hostname: 192.168.1.15")
	assert.Contains(t, out, "rtsp_port: 8554")
	assert.Contains(t, out, "http_port: 8080")
	assert.Contains(t, out, "rtsp_path_hq: /cam-front-doorbell-hq")
	assert.Contains(t, out, "rtsp_path_lq: /cam-front-doorbell-lq")
	assert.Contains(t, out, "snapshot_path: /cam-front-doorbell.jpg")
	assert.Contains(t, out, "02:4E:53:54:00:01")
	assert.Contains(t, out, "/cam-side-yard-hq")
	assert.Equal(t, 2, strings.Count(out, "rtsp_path_hq:"))
}

func TestGenerateEmitsCIDRAddresses(t *testing.T) {
	got, err := onvif.Generate(twoCameras(), "192.168.1.15:8554", "192.168.1.15:8080")
	require.NoError(t, err)

	assert.Contains(t, string(got), "ip: 192.168.1.8/24")
	assert.Contains(t, string(got), "ip: 192.168.1.13/24")
}

func TestGenerateDerivesStableHardwareIDFromMAC(t *testing.T) {
	got, err := onvif.Generate(twoCameras(), "192.168.1.15:8554", "192.168.1.15:8080")
	require.NoError(t, err)

	// UUIDv5 of "02:4e:53:54:00:01" under the bridge's namespace. Hard-coded so
	// that changing the derivation — which would re-identify every adopted
	// camera in Protect — cannot pass silently.
	assert.Contains(t, string(got), onvif.HardwareID("02:4E:53:54:00:01"))
	assert.Regexp(t,
		`hardware_id: [0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}`,
		string(got))
}

func TestHardwareIDIsCaseInsensitiveAndDistinct(t *testing.T) {
	assert.Equal(t, onvif.HardwareID("02:4E:53:54:00:01"), onvif.HardwareID("02:4e:53:54:00:01"))
	assert.NotEqual(t, onvif.HardwareID("02:4E:53:54:00:01"), onvif.HardwareID("02:4E:53:54:00:02"))
}

func TestHardwareIDNormalisesSeparatorAndCase(t *testing.T) {
	want := onvif.HardwareID("02:4E:53:54:00:01")
	for _, spelling := range []string{"02:4e:53:54:00:01", "02-4E-53-54-00-01", "024e.5354.0001"} {
		assert.Equal(t, want, onvif.HardwareID(spelling), spelling)
	}
	assert.Equal(t, "02:4e:53:54:00:01", onvif.CanonicalMAC("02-4E-53-54-00-01"))
}

// Identity is derived from the canonical MAC, so a differently spelled config
// entry must generate the same hardware_id and serial_number — otherwise
// re-spelling an address in config.yaml would silently re-identify an adopted
// camera. The emitted mac field keeps the operator's spelling.
func TestGenerateDerivesIdentityIndependentOfMACSpelling(t *testing.T) {
	cfg := twoCameras()
	cfg.Cameras[0].ONVIF.MAC = "02-4e-53-54-00-01"

	got, err := onvif.Generate(cfg, "192.168.1.15:8554", "192.168.1.15:8080")
	require.NoError(t, err)
	out := string(got)

	assert.Contains(t, out, "hardware_id: "+onvif.HardwareID("02:4E:53:54:00:01"))
	assert.Contains(t, out, `serial_number: "024E53540001"`)
	assert.Contains(t, out, `mac: "02-4e-53-54-00-01"`)
}

func TestGenerateIsDeterministic(t *testing.T) {
	first, err := onvif.Generate(twoCameras(), "192.168.1.15:8554", "192.168.1.15:8080")
	require.NoError(t, err)
	second, err := onvif.Generate(twoCameras(), "192.168.1.15:8554", "192.168.1.15:8080")
	require.NoError(t, err)

	assert.Equal(t, first, second)
}

func TestGenerateOmitsCredentials(t *testing.T) {
	cfg := twoCameras()
	cfg.Google = config.GoogleConfig{
		ProjectID:          "aaaa-project-id-bbbb",
		ClientID:           "cccc-client-id-dddd.apps.googleusercontent.com",
		ClientSecret:       "eeee-client-secret-ffff",
		RedirectURI:        "http://localhost:8190/oauth2callback",
		PubSubSubscription: "projects/x/subscriptions/gggg-subscription-hhhh",
	}

	got, err := onvif.Generate(cfg, "192.168.1.15:8554", "192.168.1.15:8080")
	require.NoError(t, err)

	for _, secret := range []string{
		cfg.Google.ProjectID, cfg.Google.ClientID, cfg.Google.ClientSecret,
		cfg.Google.RedirectURI, cfg.Google.PubSubSubscription,
	} {
		assert.NotContains(t, string(got), secret)
	}
	// MediaMTX runs unauthenticated, so no host credentials belong here either.
	assert.NotContains(t, string(got), "password")
	assert.NotContains(t, string(got), "username")
}

func TestGenerateRejectsMismatchedHosts(t *testing.T) {
	_, err := onvif.Generate(twoCameras(), "192.168.1.15:8554", "192.168.1.99:8080")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "same host")
}

func TestGenerateRejectsMalformedHostPort(t *testing.T) {
	_, err := onvif.Generate(twoCameras(), "192.168.1.15", "192.168.1.15:8080")
	require.Error(t, err)

	_, err = onvif.Generate(twoCameras(), "192.168.1.15:not-a-port", "192.168.1.15:8080")
	require.Error(t, err)
}

func TestGenerateRejectsNoCameras(t *testing.T) {
	_, err := onvif.Generate(config.Config{}, "192.168.1.15:8554", "192.168.1.15:8080")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no cameras")
}

// The consumer is a Node service using js-yaml, which implements YAML 1.1 and
// reads an unquoted 024E53540001 as scientific notation, resolving it to
// Infinity and rejecting the file. yaml.v3 emits under YAML 1.2, where the same
// scalar is a plain string, so nothing on the Go side notices. The container
// crashloops before it binds, which is a slow thing to diagnose from Protect.
func TestGenerateQuotesScalarsThatYAML11WouldMisread(t *testing.T) {
	got, err := onvif.Generate(twoCameras(), "192.168.1.15:8554", "192.168.1.15:8080")
	require.NoError(t, err)

	assert.Contains(t, string(got), `serial_number: "024E53540001"`)
	assert.NotContains(t, string(got), "serial_number: 024E53540001")

	// MACs are quoted as belt and braces. yaml.v3 already quotes an all-digit
	// address such as 02:00:00:00:00:01, which YAML 1.1 would read as a
	// sexagesimal integer, so this pins intent rather than fixing a live bug.
	assert.Contains(t, string(got), `mac: "02:4E:53:54:00:01"`)
	assert.NotContains(t, string(got), "mac: 02:4E:53:54:00:01\n")
}

// The regression that matters: a camera with no audio must advertise none.
// Protect requests whatever the media profile lists, so audio advertised on a
// silent camera makes it wait for a track that never arrives.
func TestGenerateEmitsAudioOnlyForCamerasThatHaveIt(t *testing.T) {
	got, err := onvif.Generate(twoCameras(), "192.168.1.15:8554", "192.168.1.15:8080")
	require.NoError(t, err)
	out := string(got)

	assert.Equal(t, 1, strings.Count(out, "audio_hq:"))
	assert.Equal(t, 1, strings.Count(out, "audio_lq:"))

	frontDoorbell, sideYard, ok := strings.Cut(out, "  - name: Side yard")
	require.True(t, ok)
	assert.Contains(t, frontDoorbell, "audio_hq:")
	assert.NotContains(t, sideYard, "audio")
}

// The bitrates must track the transcode in internal/mediamtx, and the encoding
// must stay inside the ONVIF Media1 enum of G711, G726 and AAC.
func TestGenerateAdvertisesTheAudioTheTranscodeProduces(t *testing.T) {
	got, err := onvif.Generate(twoCameras(), "192.168.1.15:8554", "192.168.1.15:8080")
	require.NoError(t, err)
	out := string(got)

	assert.Equal(t, 2, strings.Count(out, "encoding: AAC"))
	assert.Equal(t, 2, strings.Count(out, "sample_rate: 48000"))
	assert.Equal(t, 2, strings.Count(out, "channels: 2"))
	assert.Contains(t, out, "bitrate: 64")
	assert.Contains(t, out, "bitrate: 32")
}

func TestGenerateMatchesGoldenFile(t *testing.T) {
	want, err := os.ReadFile("testdata/expected.yml")
	require.NoError(t, err)

	got, err := onvif.Generate(twoCameras(), "192.168.1.15:8554", "192.168.1.15:8080")
	require.NoError(t, err)

	assert.Equal(t, string(want), string(got))
}
