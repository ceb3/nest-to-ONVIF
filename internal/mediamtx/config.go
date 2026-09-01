// Package mediamtx generates the MediaMTX server configuration from the
// bridge's own config.
//
// Each camera publishes one raw path. Protect needs three further artefacts per
// camera: a high-quality rendition with AAC audio, since Protect does not accept
// the Opus that WebRTC delivers; a low-quality rendition, since Protect opens
// both and holds them open; and a periodic JPEG, which MediaMTX does not
// produce natively. All three come from a single ffmpeg process per camera,
// started when the raw path becomes available, so the source is decoded once.
//
// Generating this file rather than hand-writing it keeps the path names in step
// with config.yaml, which is the same reason the ONVIF config is generated: the
// path names, the ONVIF stream URIs, and the publisher's own targets all derive
// from the camera name, and a mismatch surfaces only as a 404 at adoption time.
package mediamtx

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mustacheride/nest-to-ONVIF/internal/config"
)

// snapshotDir is where the snapshot writer drops JPEGs. It is a volume shared
// with the file server, so it must match the mount on both the `mediamtx` and
// `snapshots` services in deploy/docker-compose.yml.
const snapshotDir = "/snapshots"

// longEdge caps the low-quality rendition and the snapshot. Cameras differ in
// orientation — the doorbell is portrait, the outdoor cameras landscape — so
// the cap is applied to whichever edge is longer and the other is derived from
// the source aspect ratio. `-2` keeps the derived edge even, which H.264
// requires.
const longEdge = 640

// scaleFilter fits the image inside a longEdge square, preserving the source
// aspect ratio, so it caps the long edge whichever way round the camera is.
//
// It must contain no commas. MediaMTX splits the hook into arguments itself
// rather than running it through a shell, and it strips backslashes while
// doing so, so neither quoting nor escaping survives: a comma inside the
// expression reaches ffmpeg as a filter separator and the filter is not found.
// force_original_aspect_ratio expresses the same intent comma-free, and
// force_divisible_by keeps both dimensions even, as H.264 requires.
var scaleFilter = fmt.Sprintf(
	"scale=%d:%d:force_original_aspect_ratio=decrease:force_divisible_by=2",
	longEdge, longEdge)

// pathConfig is one entry under `paths`.
type pathConfig struct {
	RunOnAvailable        *yaml.Node `yaml:"runOnAvailable"`
	RunOnAvailableRestart bool       `yaml:"runOnAvailableRestart"`
}

// folded emits s in YAML's folded style, which wraps a long scalar across
// lines and folds those breaks back into spaces when parsed. The hook is a
// single command line; this keeps it legible in the file without changing it.
func folded(s string) *yaml.Node {
	return &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!str",
		Value: s,
		Style: yaml.FoldedStyle,
	}
}

// document is the generated file. Field order is emission order.
type document struct {
	LogLevel       string   `yaml:"logLevel"`
	RTSP           bool     `yaml:"rtsp"`
	RTSPAddress    string   `yaml:"rtspAddress"`
	RTSPTransports []string `yaml:"rtspTransports"`
	HLS            bool     `yaml:"hls"`
	HLSAddress     string   `yaml:"hlsAddress,omitempty"`
	// MPEG-TS HLS plays in every browser via hls.js; the default low-latency
	// fMP4 variant breaks Safari's native player and needs LL-HLS support.
	HLSVariant      string    `yaml:"hlsVariant,omitempty"`
	HLSAllowOrigins []string  `yaml:"hlsAllowOrigins,omitempty"`
	WebRTC          bool      `yaml:"webrtc"`
	RTMP            bool      `yaml:"rtmp"`
	SRT             bool      `yaml:"srt"`
	Paths           yaml.Node `yaml:"paths"`
}

// Generate renders the MediaMTX configuration for every camera in cfg.
func Generate(cfg config.Config) ([]byte, error) {
	if len(cfg.Cameras) == 0 {
		return nil, fmt.Errorf("no cameras configured")
	}

	// The hooks address MediaMTX over the loopback inside its own container,
	// never the host address, so a change to the published bind cannot break
	// the transcode.
	const local = "rtsp://127.0.0.1:8554"

	// Path order follows config.yaml. yaml.v3 sorts Go map keys, so the mapping
	// is built as a node to keep the generated file diffable against the config.
	paths := yaml.Node{Kind: yaml.MappingNode}
	seen := make(map[string]string, len(cfg.Cameras))
	for _, cam := range cfg.Cameras {
		path := cam.PathName()
		if prior, dup := seen[path]; dup {
			return nil, fmt.Errorf(
				"cameras %q and %q both resolve to path %q", prior, cam.Name, path)
		}
		seen[path] = cam.Name

		var body yaml.Node
		if err := body.Encode(pathConfig{
			RunOnAvailable: folded(hook(local, path)),
			// Without this the hook runs once and a republish after a session
			// drop would leave Protect reading a path nothing feeds.
			RunOnAvailableRestart: true,
		}); err != nil {
			return nil, fmt.Errorf("encoding path %q: %w", path, err)
		}
		paths.Content = append(paths.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: path}, &body)
	}

	// The renditions are published to paths that are not declared above, and
	// MediaMTX refuses to publish to a path it has no entry for. This catch-all
	// admits them. Without it every -hq and -lq publish is closed with
	// "path is not configured" and Protect sees nothing.
	paths.Content = append(paths.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "all_others"},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"})

	doc := document{
		LogLevel:    "info",
		RTSP:        true,
		RTSPAddress: ":8554",
		// Protect and the renditions both use TCP; UDP would add a failure mode
		// with no benefit here.
		RTSPTransports:  []string{"tcp"},
		HLS:             true,
		HLSAddress:      ":8888",
		HLSVariant:      "mpegts",
		HLSAllowOrigins: []string{"*"},
		Paths:           paths,
	}

	var buf bytes.Buffer
	buf.WriteString("# Generated by: nest-bridge mediamtx-config\n")
	buf.WriteString("# Edit config.yaml and regenerate; changes here are overwritten.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encoding MediaMTX config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encoding MediaMTX config: %w", err)
	}
	return buf.Bytes(), nil
}

// hook builds the single ffmpeg invocation that produces both renditions and
// the snapshot for one camera.
func hook(local, path string) string {
	// Audio is mapped with `?` throughout: cameras without audio publish no
	// audio track, and a non-optional mapping would abort the whole process,
	// taking video with it.
	parts := []string{
		"ffmpeg -nostdin -i " + local + "/" + path,

		// HQ: video is copied, so the only cost is the audio transcode. Protect
		// rejects Opus, which is what WebRTC delivers.
		"-map 0:v -map 0:a? -c:v copy -c:a aac -b:a 64k",
		"-f rtsp " + local + "/" + path + "-hq",

		// LQ: re-encoded small. zerolatency keeps the added delay low, since
		// Protect holds this rendition open continuously.
		"-map 0:v -map 0:a? -c:v libx264 -preset veryfast -tune zerolatency",
		"-vf " + scaleFilter + " -b:v 512k -g 30 -c:a aac -b:a 32k",
		"-f rtsp " + local + "/" + path + "-lq",

		// Snapshot: one frame every two seconds over the same decode.
		// atomic_writing keeps the file server from ever serving a half-written
		// JPEG.
		"-map 0:v -vf fps=1/2," + scaleFilter + " -update 1 -atomic_writing 1 -y",
		snapshotDir + "/" + path + ".jpg",
	}
	// Joined with spaces, not newlines: this is one command line. The folded
	// emitter wraps it across lines in the file for readability, and folding
	// turns those breaks back into the spaces they replaced.
	return strings.Join(parts, " ")
}
