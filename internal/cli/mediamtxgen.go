package cli

import (
	"io"

	"github.com/mustacheride/nest-to-ONVIF/internal/config"
	"github.com/mustacheride/nest-to-ONVIF/internal/mediamtx"
)

// RunMediaMTXConfig writes the MediaMTX server configuration, derived from the
// bridge's own config, to w.
func RunMediaMTXConfig(w io.Writer, cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	out, err := mediamtx.Generate(*cfg)
	if err != nil {
		return err
	}
	_, err = w.Write(out)
	return err
}
