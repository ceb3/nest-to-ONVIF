package cli

import (
	"errors"
	"fmt"
	"io"
	"net/url"

	"github.com/ceb3/nest-to-ONVIF/internal/config"
	"github.com/ceb3/nest-to-ONVIF/internal/onvif"
)

// defaultSnapshotPort is the port the snapshot file server listens on, served
// from the same host as MediaMTX. It must match the host side of the port
// mapping on the `snapshots` service in deploy/docker-compose.yml.
const defaultSnapshotPort = "8080"

// RunONVIFConfig writes the ONVIF container's configuration, derived from the
// bridge's own config, to w.
func RunONVIFConfig(w io.Writer, cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	rtspHost, err := rtspHostPort(cfg.Media.RTSPBaseURL)
	if err != nil {
		return err
	}
	snapshotHost, err := replacePort(rtspHost, defaultSnapshotPort)
	if err != nil {
		return err
	}

	out, err := onvif.Generate(*cfg, rtspHost, snapshotHost)
	if err != nil {
		return err
	}
	_, err = w.Write(out)
	return err
}

// rtspHostPort extracts host:port from the configured RTSP base URL, supplying
// the RTSP default port when the URL omits one.
func rtspHostPort(base string) (string, error) {
	// Errors quote the URL with its userinfo redacted: an RTSP base URL may carry
	// credentials, and this output goes to logs and terminals.
	u, err := url.Parse(base)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			// url.Error embeds the offending URL verbatim; report only the cause.
			return "", fmt.Errorf("media.rtsp_base_url is not a valid URL: %w", uerr.Err)
		}
		return "", fmt.Errorf("media.rtsp_base_url is not a valid URL: %w", err)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("media.rtsp_base_url %q: missing host", u.Redacted())
	}
	port := u.Port()
	if port == "" {
		port = "554"
	}
	return u.Hostname() + ":" + port, nil
}

func replacePort(hostPort, port string) (string, error) {
	u, err := url.Parse("//" + hostPort)
	if err != nil {
		return "", fmt.Errorf("host %q: %w", hostPort, err)
	}
	return u.Hostname() + ":" + port, nil
}
