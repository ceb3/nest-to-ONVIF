package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/ceb3/nest-to-ONVIF/internal/config"
	"github.com/ceb3/nest-to-ONVIF/internal/sdm"
)

// DeviceInfo is the JSON shape emitted by devices-json for deploy tooling.
type DeviceInfo struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	Protocols []string `json:"protocols"`
	DeviceID  string   `json:"device_id"`
}

func RunDevicesJSON(ctx context.Context, cfgPath, tokenPath string) error {
	devices, err := listDevices(ctx, cfgPath, tokenPath)
	if err != nil {
		return err
	}
	out := make([]DeviceInfo, 0, len(devices))
	for _, d := range devices {
		out = append(out, DeviceInfo{
			Name:      d.DisplayName(),
			Type:      strings.TrimPrefix(d.Type, "sdm.devices.types."),
			Protocols: d.SupportedProtocols(),
			DeviceID:  d.Name,
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func RunDevices(ctx context.Context, cfgPath, tokenPath string) error {
	devices, err := listDevices(ctx, cfgPath, tokenPath)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTYPE\tPROTOCOLS\tDEVICE ID")
	for _, d := range devices {
		protocols := strings.Join(d.SupportedProtocols(), ",")
		if protocols == "" {
			protocols = "-"
		}
		shortType := strings.TrimPrefix(d.Type, "sdm.devices.types.")
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", d.DisplayName(), shortType, protocols, d.Name)
	}
	return w.Flush()
}

func listDevices(ctx context.Context, cfgPath, tokenPath string) ([]sdm.Device, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	ts, err := sdm.NewTokenSource(ctx, cfg.Google, sdm.NewFileTokenStore(tokenPath))
	if err != nil {
		return nil, err
	}

	devices, err := sdm.NewClient(cfg.Google.ProjectID, ts).ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	if len(devices) == 0 {
		return nil, fmt.Errorf("no devices returned; check the Device Access project has been granted access to your cameras")
	}
	return devices, nil
}
