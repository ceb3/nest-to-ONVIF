package setup

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

const bridgeUID = 10001

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
	re := regexp.MustCompile(`"(\d{1,3}(?:\.\d{1,3}){3}):(8554|8080|8888)`)
	matches := re.FindAllStringSubmatch(string(raw), -1)
	if len(matches) == 0 {
		return fmt.Errorf("no host IP bindings found in docker-compose.yml")
	}
	text := string(raw)
	seen := map[string]struct{}{}
	for _, m := range matches {
		old := m[1]
		if old == hostIP || old == "127.0.0.1" {
			continue
		}
		if _, ok := seen[old]; ok {
			continue
		}
		seen[old] = struct{}{}
		text = strings.ReplaceAll(text, `"`+old+`:`, `"`+hostIP+`:`)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(log, "docker-compose.yml host bindings -> %s\n", hostIP)
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
