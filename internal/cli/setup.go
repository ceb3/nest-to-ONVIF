package cli

import (
	"context"
	"fmt"
	"net"

	"github.com/mustacheride/nest-to-ONVIF/internal/setup"
)

// RunSetup starts the browser-based deployment wizard.
func RunSetup(ctx context.Context, listen, cfgPath, tokenPath string) error {
	if err := validateSetupListen(listen); err != nil {
		return err
	}
	repoRoot, err := setup.FindRepoRoot()
	if err != nil {
		return err
	}
	srv, err := setup.NewServer(listen, cfgPath, tokenPath, repoRoot)
	if err != nil {
		return err
	}
	return srv.Run(ctx)
}

func validateSetupListen(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("invalid --listen address %q: %w", listen, err)
	}
	if host == "" || host == "0.0.0.0" {
		return fmt.Errorf("--listen must bind to a loopback address (e.g. 127.0.0.1:8190)")
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("--listen must be loopback-only for credential safety, got %q", host)
	}
	return nil
}
