package setup

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const bridgeUID = 10001

// composePlaceholderIP is the RFC 5737 address in deploy/docker-compose.yml.
// Only this literal is rewritten to the deployment host IP; 127.0.0.1 bindings
// are never touched.
const composePlaceholderIP = "203.0.113.1"

// composeLoopbackBindings are required for nest-bridge (RTSP publish to
// 127.0.0.1:8554) and the ONVIF snapshot proxy (127.0.0.1:8080).
var composeLoopbackBindings = []struct {
	hostPort string
	line     string
}{
	{"8554:8554", `      - "127.0.0.1:8554:8554"`},
	{"8888:8888", `      - "127.0.0.1:8888:8888"`},
	{"8080:80", `      - "127.0.0.1:8080:80"`},
}

// DeployOptions configures a deployment run.
type DeployOptions struct {
	RepoRoot    string
	DeployDir   string
	ConfigPath  string
	TokenPath   string
	HostIP      string
	ParentIface string
	PubSubKey   []byte
	EventsOnvif bool
	LogLevel    string
}

// Deploy runs the ONVIF layer deployment on Linux.
func Deploy(opts DeployOptions) (string, error) {
	if runtime.GOOS != "linux" {
		return "", fmt.Errorf("deployment requires Linux (macvlan and host networking)")
	}
	var log bytes.Buffer
	write := func(format string, args ...any) {
		fmt.Fprintf(&log, format+"\n", args...)
	}

	if err := generateConfigs(opts, &log); err != nil {
		return log.String(), err
	}
	if err := patchComposeHostIP(opts.DeployDir, opts.HostIP, &log); err != nil {
		return log.String(), err
	}
	if err := installCredentials(opts, &log); err != nil {
		return log.String(), err
	}
	if err := runMacvlan(opts, &log); err != nil {
		return log.String(), err
	}
	if err := dockerUp(opts, &log); err != nil {
		return log.String(), err
	}
	write("deployment complete")
	return log.String(), nil
}

func generateConfigs(opts DeployOptions, log io.Writer) error {
	onvifPath := filepath.Join(opts.DeployDir, "onvif.yml")
	mediamtxPath := filepath.Join(opts.DeployDir, "mediamtx.yml")
	bin, err := findBridgeBinary(opts.RepoRoot)
	if err != nil {
		return err
	}
	for _, spec := range []struct {
		cmd  string
		path string
	}{
		{"onvif-config", onvifPath},
		{"mediamtx-config", mediamtxPath},
	} {
		out, err := exec.Command(bin, "-config="+opts.ConfigPath, spec.cmd).Output()
		if err != nil {
			return fmt.Errorf("generate %s: %w", spec.cmd, err)
		}
		if err := os.WriteFile(spec.path, out, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(log, "wrote %s\n", spec.path)
	}
	return nil
}

func findBridgeBinary(repoRoot string) (string, error) {
	candidates := []string{
		filepath.Join(repoRoot, "bin", "nest-bridge"),
		filepath.Join(repoRoot, "nest-bridge"),
	}
	for _, path := range candidates {
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			return path, nil
		}
	}
	if path, err := exec.LookPath("nest-bridge"); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("nest-bridge binary not found; run make build")
}

func patchComposeHostIP(deployDir, hostIP string, log io.Writer) error {
	path := filepath.Join(deployDir, "docker-compose.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(raw)
	if !strings.Contains(text, `"`+composePlaceholderIP+`:`) &&
		!strings.Contains(text, `"`+hostIP+`:8554`) {
		return fmt.Errorf("no host IP bindings found in docker-compose.yml")
	}
	text = strings.ReplaceAll(text, `"`+composePlaceholderIP+`:`, `"`+hostIP+`:`)
	text = ensureComposeLoopbackBindings(text, hostIP)
	if err := validateComposeLoopbackBindings(text); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(log, "docker-compose.yml host bindings -> %s\n", hostIP)
	return nil
}

// ensureComposeLoopbackBindings repairs compose files where a prior deploy
// rewrote 127.0.0.1 to the host IP, leaving nest-bridge unable to reach
// MediaMTX on loopback.
func ensureComposeLoopbackBindings(text, hostIP string) string {
	for _, binding := range composeLoopbackBindings {
		loopback := `"127.0.0.1:` + binding.hostPort + `"`
		if strings.Contains(text, loopback) {
			continue
		}
		hostBinding := `"` + hostIP + `:` + binding.hostPort + `"`
		if strings.Count(text, hostBinding) >= 2 {
			// A broken file often has the host IP twice where host + loopback
			// should be; convert the second occurrence back to loopback.
			first := strings.Index(text, hostBinding)
			rest := text[first+len(hostBinding):]
			second := strings.Index(rest, hostBinding)
			if second >= 0 {
				pos := first + len(hostBinding) + second
				text = text[:pos] + loopback + text[pos+len(hostBinding):]
				continue
			}
		}
		if strings.Contains(text, hostBinding) {
			text = strings.Replace(text, hostBinding+"\n", hostBinding+"\n"+binding.line+"\n", 1)
		}
	}
	return text
}

func validateComposeLoopbackBindings(text string) error {
	for _, binding := range composeLoopbackBindings {
		hostSuffix := `:` + binding.hostPort + `"`
		if !strings.Contains(text, hostSuffix) {
			continue
		}
		loopback := `"127.0.0.1:` + binding.hostPort + `"`
		if !strings.Contains(text, loopback) {
			return fmt.Errorf("docker-compose.yml missing required loopback binding %s", loopback)
		}
	}
	return nil
}

func installCredentials(opts DeployOptions, log io.Writer) error {
	configDir := filepath.Join(opts.DeployDir, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}
	for _, pair := range []struct {
		src string
		dst string
	}{
		{opts.ConfigPath, "config.yaml"},
		{opts.TokenPath, "tokens.json"},
	} {
		data, err := os.ReadFile(pair.src)
		if err != nil {
			return fmt.Errorf("read %s: %w", pair.src, err)
		}
		path := filepath.Join(configDir, pair.dst)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return err
		}
		if err := os.Chown(path, bridgeUID, bridgeUID); err != nil && os.Geteuid() == 0 {
			return fmt.Errorf("chown %s: %w", path, err)
		}
	}
	pubsubPath := filepath.Join(configDir, "pubsub-sa.json")
	var pubsubBody []byte
	if opts.EventsOnvif && len(opts.PubSubKey) > 0 {
		pubsubBody = opts.PubSubKey
	} else {
		pubsubBody = []byte("{}\n")
	}
	if err := os.WriteFile(pubsubPath, pubsubBody, 0o400); err != nil {
		return err
	}
	if err := os.Chown(pubsubPath, bridgeUID, bridgeUID); err != nil && os.Geteuid() == 0 {
		return fmt.Errorf("chown %s: %w", pubsubPath, err)
	}
	fmt.Fprintf(log, "installed credentials in %s\n", configDir)
	return nil
}

func runMacvlan(opts DeployOptions, log io.Writer) error {
	script := filepath.Join(opts.DeployDir, "macvlan-setup.sh")
	env := []string{
		"PARENT=" + opts.ParentIface,
		"ONVIF_CONFIG=" + filepath.Join(opts.DeployDir, "onvif.yml"),
	}
	var cmd *exec.Cmd
	if os.Geteuid() == 0 {
		cmd = exec.Command("bash", script)
		cmd.Env = append(os.Environ(), env...)
	} else {
		args := append([]string{"env"}, env...)
		args = append(args, "bash", script)
		cmd = exec.Command("sudo", args...)
	}
	cmd.Dir = opts.DeployDir
	out, err := cmd.CombinedOutput()
	fmt.Fprint(log, string(out))
	if err != nil {
		return fmt.Errorf("macvlan setup: %w", err)
	}
	return nil
}

func dockerUp(opts DeployOptions, log io.Writer) error {
	compose := filepath.Join(opts.DeployDir, "docker-compose.yml")
	env := os.Environ()
	if opts.LogLevel != "" {
		env = append(env, "NEST_BRIDGE_LOG_LEVEL="+opts.LogLevel)
	}
	for _, args := range [][]string{
		{"docker", "compose", "-f", compose, "build", "bridge", "onvif"},
		{"docker", "compose", "-f", compose, "up", "-d"},
		{"docker", "compose", "-f", compose, "ps"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = opts.DeployDir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		fmt.Fprint(log, string(out))
		if err != nil {
			return fmt.Errorf("%s: %w", strings.Join(args, " "), err)
		}
	}
	return nil
}

func checkDocker() (bool, string) {
	if _, err := exec.LookPath("docker"); err != nil {
		return false, "docker not found in PATH"
	}
	out, err := exec.Command("docker", "compose", "version").CombinedOutput()
	if err != nil {
		return false, strings.TrimSpace(string(out))
	}
	return true, ""
}
