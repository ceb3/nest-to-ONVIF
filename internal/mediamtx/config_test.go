package mediamtx_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/mustacheride/nest-to-ONVIF/internal/config"
	"github.com/mustacheride/nest-to-ONVIF/internal/mediamtx"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	var cfg config.Config
	require.NoError(t, yaml.Unmarshal([]byte(`
google:
  project_id: p
  client_id: c
  client_secret: s
media:
  rtsp_base_url: "rtsp://127.0.0.1:8554"
cameras:
  - device_id: d1
    name: "Front doorbell"
    audio: true
    onvif: { mac: "02:4E:53:54:00:01", ip: "10.0.0.1" }
  - device_id: d2
    name: "Side yard"
    event: { linger: 60s }
    onvif: { mac: "02:4E:53:54:00:02", ip: "10.0.0.2" }
`), &cfg))
	return cfg
}

// parsePaths decodes the generated document far enough to assert on structure
// rather than on formatting, so the tests do not pin the emitter's line breaks.
func parsePaths(t *testing.T, out []byte) map[string]map[string]any {
	t.Helper()
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	require.NoError(t, yaml.Unmarshal(out, &doc), "generated document must parse")
	return doc.Paths
}

func TestGenerateEmitsATranscodePathPerCamera(t *testing.T) {
	out, err := mediamtx.Generate(testConfig(t))
	require.NoError(t, err)

	paths := parsePaths(t, out)
	assert.Contains(t, paths, "cam-front-doorbell")
	assert.Contains(t, paths, "cam-side-yard")
	// The renditions are produced by the source path's hook, so they must not be
	// declared as paths of their own.
	assert.NotContains(t, paths, "cam-front-doorbell-hq")
	assert.NotContains(t, paths, "cam-front-doorbell-lq")
}

func TestGenerateProducesBothRenditionsAndASnapshotForEveryCamera(t *testing.T) {
	out, err := mediamtx.Generate(testConfig(t))
	require.NoError(t, err)

	for _, path := range []string{"cam-front-doorbell", "cam-side-yard"} {
		hook, ok := parsePaths(t, out)[path]["runOnAvailable"].(string)
		require.True(t, ok, "%s must carry a runOnAvailable hook", path)

		assert.Contains(t, hook, "rtsp://127.0.0.1:8554/"+path+"-hq")
		assert.Contains(t, hook, "rtsp://127.0.0.1:8554/"+path+"-lq")
		assert.Contains(t, hook, "/snapshots/"+path+".jpg")
		assert.Contains(t, hook, "-i rtsp://127.0.0.1:8554/"+path+" ",
			"the hook must read the camera's own source path")
	}
}

// Audio is optional per camera, and a camera configured without it publishes no
// audio track at all. One template serves both only if the audio mapping is
// optional, so a missing track cannot fail the whole ffmpeg process.
func TestGenerateMapsAudioOptionallySoSilentCamerasStillTranscode(t *testing.T) {
	out, err := mediamtx.Generate(testConfig(t))
	require.NoError(t, err)

	for _, path := range []string{"cam-front-doorbell", "cam-side-yard"} {
		hook := parsePaths(t, out)[path]["runOnAvailable"].(string)
		assert.Contains(t, hook, "-map 0:a?", "%s must tolerate a missing audio track", path)
		assert.NotContains(t, hook, "-map 0:a ")
	}
}

// The doorbell is portrait and the outdoor cameras are landscape. A fixed
// WxH would stretch one of the two, so the long edge is capped instead.
func TestGenerateScalesWithoutAssumingOrientation(t *testing.T) {
	out, err := mediamtx.Generate(testConfig(t))
	require.NoError(t, err)

	hook := parsePaths(t, out)["cam-front-doorbell"]["runOnAvailable"].(string)
	assert.NotContains(t, hook, "scale=480:640",
		"a fixed portrait size would distort the landscape cameras")
	assert.Contains(t, hook, "force_original_aspect_ratio=decrease",
		"scaling must preserve the source aspect ratio")
	assert.Contains(t, hook, "force_divisible_by=2",
		"H.264 requires even dimensions")
}

// MediaMTX splits the hook into arguments itself rather than handing it to a
// shell, and strips backslashes while doing so. Neither quoting nor escaping
// survives, so the scale expression must contain no comma at all: one would
// reach ffmpeg as a filter separator and the filter would not be found.
func TestGenerateScaleExpressionSurvivesArgumentSplitting(t *testing.T) {
	out, err := mediamtx.Generate(testConfig(t))
	require.NoError(t, err)

	hook := parsePaths(t, out)["cam-front-doorbell"]["runOnAvailable"].(string)

	assert.NotContains(t, hook, "'", "no shell runs this, so quotes would be literal")
	assert.NotContains(t, hook, `"`)
	assert.NotContains(t, hook, `\`, "backslashes are stripped before ffmpeg sees them")

	// Every -vf argument must be a single filter chain whose only commas
	// separate whole filters.
	fields := strings.Fields(hook)
	for i, f := range fields {
		if f != "-vf" {
			continue
		}
		require.Less(t, i+1, len(fields), "-vf must have a value")
		for _, filter := range strings.Split(fields[i+1], ",") {
			name, _, _ := strings.Cut(filter, "=")
			assert.Contains(t, []string{"scale", "fps"}, name,
				"unexpected filter %q in %q", name, fields[i+1])
		}
	}
}

// The renditions publish to paths that are not declared individually, and
// MediaMTX refuses to publish to a path with no entry.
func TestGenerateAdmitsTheRenditionPathsWithACatchAll(t *testing.T) {
	out, err := mediamtx.Generate(testConfig(t))
	require.NoError(t, err)

	var doc struct {
		Paths yaml.Node `yaml:"paths"`
	}
	require.NoError(t, yaml.Unmarshal(out, &doc))

	var keys []string
	for i := 0; i < len(doc.Paths.Content); i += 2 {
		keys = append(keys, doc.Paths.Content[i].Value)
	}
	require.NotEmpty(t, keys)
	assert.Equal(t, "all_others", keys[len(keys)-1],
		"the catch-all must come last so it cannot shadow a camera's own path")
}

// The hook is written across several lines for readability, but it is executed
// as a command line. Folding must collapse those breaks, or ffmpeg is handed
// arguments containing newlines.
func TestGenerateFoldsTheHookOntoASingleCommandLine(t *testing.T) {
	out, err := mediamtx.Generate(testConfig(t))
	require.NoError(t, err)

	hook := parsePaths(t, out)["cam-front-doorbell"]["runOnAvailable"].(string)

	assert.NotContains(t, strings.TrimRight(hook, "\n"), "\n",
		"the parsed hook must be one line")
	assert.NotContains(t, hook, "  ", "folding must not leave doubled spaces")
	assert.Contains(t, hook, "-c:a aac -b:a 64k -f rtsp",
		"arguments either side of a line break must stay space-separated")
}

func TestGenerateRestartsTheHookSoARepublishIsPickedUp(t *testing.T) {
	out, err := mediamtx.Generate(testConfig(t))
	require.NoError(t, err)

	for _, path := range []string{"cam-front-doorbell", "cam-side-yard"} {
		assert.Equal(t, true, parsePaths(t, out)[path]["runOnAvailableRestart"],
			"%s must restart its hook", path)
	}
}

func TestGenerateKeepsServerSettingsAlongsideThePaths(t *testing.T) {
	out, err := mediamtx.Generate(testConfig(t))
	require.NoError(t, err)

	var doc struct {
		RTSPAddress     string   `yaml:"rtspAddress"`
		RTSPTransports  []string `yaml:"rtspTransports"`
		HLS             bool     `yaml:"hls"`
		HLSAddress      string   `yaml:"hlsAddress"`
		HLSVariant      string   `yaml:"hlsVariant"`
		HLSAllowOrigins []string `yaml:"hlsAllowOrigins"`
		WebRTC          bool     `yaml:"webrtc"`
	}
	require.NoError(t, yaml.Unmarshal(out, &doc))

	assert.Equal(t, ":8554", doc.RTSPAddress)
	assert.Equal(t, []string{"tcp"}, doc.RTSPTransports)
	assert.True(t, doc.HLS)
	assert.Equal(t, ":8888", doc.HLSAddress)
	assert.Equal(t, "mpegts", doc.HLSVariant)
	assert.Equal(t, []string{"*"}, doc.HLSAllowOrigins)
	assert.False(t, doc.WebRTC)
}

func TestGenerateIsDeterministic(t *testing.T) {
	first, err := mediamtx.Generate(testConfig(t))
	require.NoError(t, err)
	second, err := mediamtx.Generate(testConfig(t))
	require.NoError(t, err)

	assert.Equal(t, string(first), string(second))
}

func TestGenerateMatchesTheCommittedDeploymentFile(t *testing.T) {
	cfg, err := config.Load("../../config.example.yaml")
	require.NoError(t, err)

	out, err := mediamtx.Generate(*cfg)
	require.NoError(t, err)

	want, err := os.ReadFile("testdata/expected.yml")
	require.NoError(t, err)

	assert.Equal(t, strings.TrimSpace(string(want)), strings.TrimSpace(string(out)),
		"regenerate with: ./bin/nest-bridge -config=config.example.yaml mediamtx-config > internal/mediamtx/testdata/expected.yml")
}
