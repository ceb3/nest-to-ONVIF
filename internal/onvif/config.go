// Package onvif generates the configuration consumed by the
// emberstonel/onvif-virtual-camera container from the bridge's own camera
// config, so the two cannot drift.
package onvif

import (
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mustacheride/nest-to-ONVIF/internal/config"
)

// Resolutions produced by the MediaMTX transcode. Declared here rather than
// probed because probing only succeeds while the bridge is publishing, and the
// ONVIF container starts before any stream exists.
const (
	hqWidth  = 960
	hqHeight = 1280
	lqWidth  = 480
	lqHeight = 640
)

// The camera addresses live on a /24 alongside the deployment host.
const cameraPrefixLen = 24

// hostSourceName is the single upstream source: MediaMTX on the deployment host.
const hostSourceName = "mediamtx"

const (
	manufacturer = "nest-bridge"
	model        = "Nest Virtual Camera"
)

// hardwareIDNamespace seeds the UUIDv5 derivation. It is the standard RFC 4122
// URL namespace, and it must never change: Protect keys a camera's identity off
// the values derived under it, so a new namespace would re-present every adopted
// camera as new hardware.
var hardwareIDNamespace = [16]byte{
	0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1,
	0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8,
}

// nominalFramerate is what we advertise to Protect. Nest varies the delivered rate
// with available light: a 30-second frame count in daylight measured 27 fps, while an
// earlier night-time capture recorded 15. Advertising the nominal ceiling rather than
// either observation is the honest choice, since the rate is not fixed and Protect
// tolerates a stream arriving below its advertised maximum.
const nominalFramerate = 30

// Audio advertised to Protect, matching the AAC transcode in internal/mediamtx:
// 64 kbps on the HQ rendition and 32 kbps on the LQ one.
const (
	// audioEncoding is an ONVIF Media1 AudioEncoderConfiguration enum value. The
	// enum admits only G711, G726 and AAC.
	audioEncoding = "AAC"
	// audioSampleRate is in Hz, the unit the transcode and ffprobe both use. ONVIF
	// states AudioEncoderConfiguration/SampleRate in kHz, so the container divides.
	audioSampleRate = 48000
	audioChannels   = 2
	hqAudioBitrate  = 64
	lqAudioBitrate  = 32
)

type runtimeSettings struct {
	EnableDebugLogs     bool `yaml:"enable_debug_logs"`
	ProbeStreams        bool `yaml:"probe_streams"`
	ProbeTimeoutMS      int  `yaml:"probe_timeout_ms"`
	IPMonitorIntervalMS int  `yaml:"ip_monitor_interval_ms"`
}

type hostSource struct {
	Name     string `yaml:"name"`
	Hostname string `yaml:"hostname"`
	RTSPPort int    `yaml:"rtsp_port"`
	HTTPPort int    `yaml:"http_port"`
}

type streamSettings struct {
	Encoding  string `yaml:"encoding"`
	Width     int    `yaml:"width"`
	Height    int    `yaml:"height"`
	Framerate int    `yaml:"framerate"`
	Bitrate   int    `yaml:"bitrate"`
	Quality   int    `yaml:"quality"`
}

type audioSettings struct {
	Encoding   string `yaml:"encoding"`
	SampleRate int    `yaml:"sample_rate"`
	Channels   int    `yaml:"channels"`
	Bitrate    int    `yaml:"bitrate"`
}

// quoted forces a value to be emitted as a double-quoted scalar.
//
// The consumer of this file is a Node service using js-yaml, which implements
// YAML 1.1, while yaml.v3 emits under YAML 1.2 rules. The two disagree about
// unquoted scalars: a serial such as 024E53540001 is a plain string to yaml.v3
// but scientific notation to js-yaml, which resolves it to Infinity and rejects
// the config. Quoting removes the ambiguity for both readers.
//
// Applied to serials and MACs. Only the serials need it: yaml.v3 already quotes
// a scalar such as 02:00:00:00:00:01 of its own accord, having noticed the YAML
// 1.1 sexagesimal reading, so quoting MACs changes nothing that yaml.v3 would
// otherwise emit. It is kept as belt and braces, making the intent explicit at
// the field rather than resting on an encoder detail.
type quoted string

func (q quoted) MarshalYAML() (any, error) {
	return &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!str",
		Value: string(q),
		Style: yaml.DoubleQuotedStyle,
	}, nil
}

type virtualCamera struct {
	Name         string         `yaml:"name"`
	Manufacturer string         `yaml:"manufacturer"`
	Model        string         `yaml:"model"`
	MAC          quoted         `yaml:"mac"`
	IP           string         `yaml:"ip"`
	HardwareID   string         `yaml:"hardware_id"`
	SerialNumber quoted         `yaml:"serial_number"`
	HostSource   string         `yaml:"host_source"`
	RTSPPathHQ   string         `yaml:"rtsp_path_hq"`
	RTSPPathLQ   string         `yaml:"rtsp_path_lq"`
	SnapshotPath string         `yaml:"snapshot_path"`
	StreamHQ     streamSettings `yaml:"stream_hq"`
	StreamLQ     streamSettings `yaml:"stream_lq"`
	AudioHQ      *audioSettings `yaml:"audio_hq,omitempty"`
	AudioLQ      *audioSettings `yaml:"audio_lq,omitempty"`
}

type document struct {
	Runtime        runtimeSettings `yaml:"runtime"`
	HostSources    []hostSource    `yaml:"host_sources"`
	VirtualCameras []virtualCamera `yaml:"virtual_cameras"`
}

// CanonicalMAC returns mac in the canonical lower-case colon-separated form.
// Identity is derived from this rather than from the spelling in config.yaml, so
// that two spellings of one address cannot produce two Protect identities. An
// unparseable value is lower-cased and otherwise left alone; config validation
// rejects it before generation is reached.
func CanonicalMAC(mac string) string {
	if hw, err := net.ParseMAC(mac); err == nil {
		return hw.String()
	}
	return strings.ToLower(mac)
}

// HardwareID derives a stable UUIDv5 from a MAC address. Two calls with the
// same MAC always agree, including across differences in letter case and
// separator.
func HardwareID(mac string) string {
	h := sha1.New()
	h.Write(hardwareIDNamespace[:])
	h.Write([]byte(CanonicalMAC(mac)))
	sum := h.Sum(nil)

	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant

	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		binary.BigEndian.Uint32(u[0:4]),
		binary.BigEndian.Uint16(u[4:6]),
		binary.BigEndian.Uint16(u[6:8]),
		binary.BigEndian.Uint16(u[8:10]),
		u[10:16])
}

// Generate renders the ONVIF container's YAML for every camera in cfg. rtspHost
// and snapshotHost are host:port pairs for MediaMTX's RTSP server and the
// snapshot file server respectively; both must name the same host, because the
// upstream schema binds a camera to a single host source.
func Generate(cfg config.Config, rtspHost, snapshotHost string) ([]byte, error) {
	if len(cfg.Cameras) == 0 {
		return nil, fmt.Errorf("no cameras configured")
	}

	rtspName, rtspPort, err := splitHostPort("rtsp host", rtspHost)
	if err != nil {
		return nil, err
	}
	snapName, snapPort, err := splitHostPort("snapshot host", snapshotHost)
	if err != nil {
		return nil, err
	}
	if rtspName != snapName {
		return nil, fmt.Errorf(
			"rtsp host %q and snapshot host %q must name the same host", rtspName, snapName)
	}

	doc := document{
		Runtime: runtimeSettings{
			ProbeStreams:        false,
			ProbeTimeoutMS:      15000,
			IPMonitorIntervalMS: 5000,
		},
		HostSources: []hostSource{{
			Name:     hostSourceName,
			Hostname: rtspName,
			RTSPPort: rtspPort,
			HTTPPort: snapPort,
		}},
	}

	for _, cam := range cfg.Cameras {
		path := cam.PathName()
		vc := virtualCamera{
			Name:         cam.Name,
			Manufacturer: manufacturer,
			Model:        model,
			MAC:          quoted(cam.ONVIF.MAC),
			IP:           fmt.Sprintf("%s/%d", cam.ONVIF.IP, cameraPrefixLen),
			HardwareID:   HardwareID(cam.ONVIF.MAC),
			SerialNumber: quoted(serialNumber(cam.ONVIF.MAC)),
			HostSource:   hostSourceName,
			RTSPPathHQ:   "/" + path + "-hq",
			RTSPPathLQ:   "/" + path + "-lq",
			SnapshotPath: "/" + path + ".jpg",
			StreamHQ: streamSettings{
				Encoding: "H264", Width: hqWidth, Height: hqHeight,
				Framerate: nominalFramerate, Bitrate: 2048, Quality: 5,
			},
			StreamLQ: streamSettings{
				Encoding: "H264", Width: lqWidth, Height: lqHeight,
				Framerate: nominalFramerate, Bitrate: 512, Quality: 3,
			},
		}

		// Only cameras whose Nest source actually carries audio advertise any.
		// Advertising it on a camera that never sends an audio track leaves
		// Protect waiting for a stream that never arrives, which is a worse
		// failure than silence.
		if cam.Audio {
			vc.AudioHQ = &audioSettings{
				Encoding: audioEncoding, SampleRate: audioSampleRate,
				Channels: audioChannels, Bitrate: hqAudioBitrate,
			}
			vc.AudioLQ = &audioSettings{
				Encoding: audioEncoding, SampleRate: audioSampleRate,
				Channels: audioChannels, Bitrate: lqAudioBitrate,
			}
		}

		doc.VirtualCameras = append(doc.VirtualCameras, vc)
	}

	var sb strings.Builder
	enc := yaml.NewEncoder(&sb)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encode onvif config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode onvif config: %w", err)
	}
	return []byte(sb.String()), nil
}

// MACKey returns the separator-free upper-case form of mac. This is the
// spelling an adopting client reports a device's address in, so it is also the only
// safe key for matching a configured camera to an adopted one.
func MACKey(mac string) string {
	return strings.ToUpper(strings.ReplaceAll(CanonicalMAC(mac), ":", ""))
}

func serialNumber(mac string) string {
	return MACKey(mac)
}

func splitHostPort(label, hostPort string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		return "", 0, fmt.Errorf("%s %q: expected host:port", label, hostPort)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("%s %q: invalid port %q", label, hostPort, portStr)
	}
	if host == "" {
		return "", 0, fmt.Errorf("%s %q: missing host", label, hostPort)
	}
	return host, port, nil
}
