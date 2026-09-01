package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ceb3/nest-to-ONVIF/internal/cli"
)

var version = "dev"

type cliArgs struct {
	cfgPath     string
	tokenPath   string
	streamFor   time.Duration
	setupListen string
	command     string
	cameraName  string
}

func parseCLIArgs(args []string) (cliArgs, error) {
	var parsed cliArgs
	flags := flag.NewFlagSet("nest-bridge", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&parsed.cfgPath, "config", "config.yaml", "path to configuration file")
	flags.StringVar(&parsed.tokenPath, "tokens", "tokens.json", "path to the OAuth token cache")
	flags.DurationVar(&parsed.streamFor, "for", time.Hour, "how long the stream command should run")
	flags.StringVar(&parsed.setupListen, "listen", "127.0.0.1:8190", "setup wizard listen address (loopback only)")
	if err := flags.Parse(args); err != nil {
		return cliArgs{}, err
	}
	parsed.command = flags.Arg(0)
	if parsed.command == "stream" {
		parsed.cameraName = flags.Arg(1)
		if flags.NArg() > 2 {
			for _, arg := range flags.Args()[2:] {
				if strings.HasPrefix(arg, "-") {
					return cliArgs{}, fmt.Errorf(
						"flags must precede the command; use: nest-bridge [-for=DURATION] stream <camera-name>")
				}
			}
		}
		if parsed.cameraName == "" || flags.NArg() != 2 {
			return cliArgs{}, fmt.Errorf("usage: nest-bridge [-for=DURATION] stream <camera-name>")
		}
	}
	return parsed, nil
}

func main() {
	args, parseErr := parseCLIArgs(os.Args[1:])
	if parseErr != nil {
		fmt.Fprintln(os.Stderr, "error:", parseErr)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch args.command {
	case "version":
		fmt.Println(version)
	case "auth":
		err = cli.RunAuth(ctx, args.cfgPath, args.tokenPath)
	case "devices":
		err = cli.RunDevices(ctx, args.cfgPath, args.tokenPath)
	case "devices-json":
		err = cli.RunDevicesJSON(ctx, args.cfgPath, args.tokenPath)
	case "stream":
		err = cli.RunStream(ctx, args.cfgPath, args.tokenPath, args.cameraName, args.streamFor)
	case "onvif-config":
		err = cli.RunONVIFConfig(os.Stdout, args.cfgPath)
	case "mediamtx-config":
		err = cli.RunMediaMTXConfig(os.Stdout, args.cfgPath)
	case "serve":
		err = cli.RunServe(ctx, args.cfgPath, args.tokenPath)
	case "setup":
		err = cli.RunSetup(ctx, args.setupListen, args.cfgPath, args.tokenPath)
	default:
		fmt.Fprintln(os.Stderr,
			"usage: nest-bridge [flags] "+
				"<version|auth|devices|devices-json|serve|setup|stream|onvif-config|mediamtx-config>")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
