package setup

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// RequirementStatus is the outcome of one system check.
type RequirementStatus string

const (
	RequirementPass RequirementStatus = "pass"
	RequirementFail RequirementStatus = "fail"
	RequirementWarn RequirementStatus = "warn"
)

// Requirement is one row on the system discovery page.
type Requirement struct {
	ID       string            `json:"id"`
	Label    string            `json:"label"`
	Status   RequirementStatus `json:"status"`
	Detail   string            `json:"detail"`
	Required bool              `json:"required"`
}

// SystemDiscovery is returned by GET /api/discovery.
type SystemDiscovery struct {
	Hostname string        `json:"hostname"`
	OS       string        `json:"os"`
	Arch     string        `json:"arch"`
	Ready    bool          `json:"ready"`
	Checks   []Requirement `json:"checks"`
}

func runSystemDiscovery(repoRoot, deployDir string) SystemDiscovery {
	hostname, _ := os.Hostname()
	checks := []Requirement{
		checkLinux(),
		checkMacvlanPlatform(),
		checkRepoLayout(repoRoot, deployDir),
		checkBridgeBinary(repoRoot),
		checkDockerRequirement(),
		checkDockerComposeRequirement(),
		checkHostNetworking(),
		checkIPCommand(),
		checkBash(),
		checkPrivilegedHelper(),
	}
	ready := true
	for _, c := range checks {
		if c.Required && c.Status != RequirementPass {
			ready = false
		}
	}
	return SystemDiscovery{
		Hostname: hostname,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Ready:    ready,
		Checks:   checks,
	}
}

func checkLinux() Requirement {
	id := "linux"
	if runtime.GOOS == "linux" {
		return Requirement{
			ID: id, Label: "Linux host",
			Status: RequirementPass, Detail: "Running on Linux.",
			Required: true,
		}
	}
	return Requirement{
		ID: id, Label: "Linux host",
		Status:   RequirementFail,
		Detail:   fmt.Sprintf("This machine is %s/%s. The ONVIF layer needs Linux for macvlan interfaces and host-networked Docker containers. Run nest-bridge setup on your deployment VM (use SSH port forwarding to reach the wizard).", runtime.GOOS, runtime.GOARCH),
		Required: true,
	}
}

func checkMacvlanPlatform() Requirement {
	id := "macvlan"
	switch runtime.GOOS {
	case "linux":
		return Requirement{
			ID: id, Label: "macvlan networking",
			Status:   RequirementPass,
			Detail:   "Linux supports per-camera macvlan interfaces for distinct ONVIF identities.",
			Required: true,
		}
	case "darwin":
		return Requirement{
			ID: id, Label: "macvlan networking",
			Status:   RequirementFail,
			Detail:   "macOS cannot provide per-camera MAC addresses (Docker Desktop uses a VM; macvlan is not available on the LAN).",
			Required: true,
		}
	case "windows":
		return Requirement{
			ID: id, Label: "macvlan networking",
			Status:   RequirementFail,
			Detail:   "Windows cannot provide per-camera MAC addresses for ONVIF adoption.",
			Required: true,
		}
	default:
		return Requirement{
			ID: id, Label: "macvlan networking",
			Status:   RequirementFail,
			Detail:   fmt.Sprintf("%s is not a supported deployment platform.", runtime.GOOS),
			Required: true,
		}
	}
}

func checkRepoLayout(repoRoot, deployDir string) Requirement {
	id := "repo"
	compose := filepath.Join(deployDir, "docker-compose.yml")
	macvlan := filepath.Join(deployDir, "macvlan-setup.sh")
	if _, err := os.Stat(compose); err != nil {
		return Requirement{
			ID: id, Label: "Repository layout",
			Status:   RequirementFail,
			Detail:   fmt.Sprintf("Missing %s. Run setup from the repository root or set NEST_BRIDGE_ROOT.", compose),
			Required: true,
		}
	}
	if _, err := os.Stat(macvlan); err != nil {
		return Requirement{
			ID: id, Label: "Repository layout",
			Status:   RequirementFail,
			Detail:   fmt.Sprintf("Missing %s.", macvlan),
			Required: true,
		}
	}
	return Requirement{
		ID: id, Label: "Repository layout",
		Status:   RequirementPass,
		Detail:   fmt.Sprintf("Found deploy files under %s.", deployDir),
		Required: true,
	}
}

func checkBridgeBinary(repoRoot string) Requirement {
	id := "bridge"
	if _, err := findBridgeBinary(repoRoot); err != nil {
		return Requirement{
			ID: id, Label: "nest-bridge binary",
			Status:   RequirementFail,
			Detail:   "Run bin/build-bridge (or make build) in the repository root.",
			Required: true,
		}
	}
	return Requirement{
		ID: id, Label: "nest-bridge binary",
		Status:   RequirementPass,
		Detail:   "Built nest-bridge binary found.",
		Required: true,
	}
}

func checkDockerRequirement() Requirement {
	id := "docker"
	if _, err := exec.LookPath("docker"); err != nil {
		return Requirement{
			ID: id, Label: "Docker",
			Status:   RequirementFail,
			Detail:   "docker not found in PATH. On the deployment host run: sudo bin/install-docker",
			Required: true,
		}
	}
	return Requirement{
		ID: id, Label: "Docker",
		Status:   RequirementPass,
		Detail:   "docker command available.",
		Required: true,
	}
}

func checkDockerComposeRequirement() Requirement {
	id := "compose"
	ok, msg := checkDocker()
	if !ok {
		if msg == "docker not found in PATH" {
			return Requirement{
				ID: id, Label: "Docker Compose",
				Status:   RequirementFail,
				Detail:   "Install Docker first: sudo bin/install-docker",
				Required: true,
			}
		}
		return Requirement{
			ID: id, Label: "Docker Compose",
			Status:   RequirementFail,
			Detail:   msg + " — run: sudo bin/install-docker",
			Required: true,
		}
	}
	return Requirement{
		ID: id, Label: "Docker Compose",
		Status:   RequirementPass,
		Detail:   "docker compose available.",
		Required: true,
	}
}

func checkHostNetworking() Requirement {
	id := "lan"
	ifaces, err := listHostInterfaces()
	if err != nil {
		return Requirement{
			ID: id, Label: "LAN network interface",
			Status:   RequirementFail,
			Detail:   err.Error(),
			Required: true,
		}
	}
	if len(ifaces) == 0 {
		return Requirement{
			ID: id, Label: "LAN network interface",
			Status:   RequirementFail,
			Detail:   "No up interface with a non-loopback IPv4 address was found. Connect this host to your LAN before deploying.",
			Required: true,
		}
	}
	names := make([]string, 0, len(ifaces))
	for _, iface := range ifaces {
		names = append(names, fmt.Sprintf("%s (%s)", iface.Name, iface.Addresses[0]))
	}
	return Requirement{
		ID: id, Label: "LAN network interface",
		Status:   RequirementPass,
		Detail:   "Detected: " + joinLimited(names, ", ", 4),
		Required: true,
	}
}

func checkIPCommand() Requirement {
	id := "ip"
	if runtime.GOOS != "linux" {
		return Requirement{ID: id, Label: "iproute2 (ip)", Status: RequirementFail, Detail: "Requires Linux.", Required: true}
	}
	if _, err := exec.LookPath("ip"); err != nil {
		return Requirement{
			ID: id, Label: "iproute2 (ip)",
			Status:   RequirementFail,
			Detail:   "ip command not found. Run: sudo bin/install-packages",
			Required: true,
		}
	}
	return Requirement{
		ID: id, Label: "iproute2 (ip)",
		Status:   RequirementPass,
		Detail:   "ip command available for macvlan-setup.sh.",
		Required: true,
	}
}

func checkBash() Requirement {
	id := "bash"
	if _, err := exec.LookPath("bash"); err != nil {
		return Requirement{
			ID: id, Label: "bash",
			Status:   RequirementFail,
			Detail:   "bash not found. Run: sudo bin/install-packages",
			Required: true,
		}
	}
	return Requirement{
		ID: id, Label: "bash",
		Status:   RequirementPass,
		Detail:   "bash available.",
		Required: true,
	}
}

func checkPrivilegedHelper() Requirement {
	id := "privilege"
	if os.Geteuid() == 0 {
		return Requirement{
			ID: id, Label: "Privileged setup (root or sudo)",
			Status:   RequirementPass,
			Detail:   "Running as root.",
			Required: false,
		}
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		return Requirement{
			ID: id, Label: "Privileged setup (root or sudo)",
			Status:   RequirementWarn,
			Detail:   "Not root and sudo not found. macvlan and credential install need elevation — use sudo bin/setup-host or SSH as root.",
			Required: false,
		}
	}
	return Requirement{
		ID: id, Label: "Privileged setup (root or sudo)",
		Status:   RequirementPass,
		Detail:   "sudo is available for macvlan setup during deploy.",
		Required: false,
	}
}

func joinLimited(items []string, sep string, max int) string {
	if len(items) <= max {
		return fmt.Sprintf("%s", joinStrings(items, sep))
	}
	head := joinStrings(items[:max], sep)
	return head + sep + fmt.Sprintf("and %d more", len(items)-max)
}

func joinStrings(items []string, sep string) string {
	if len(items) == 0 {
		return ""
	}
	out := items[0]
	for _, s := range items[1:] {
		out += sep + s
	}
	return out
}

func (s *Server) discovery() SystemDiscovery {
	return runSystemDiscovery(s.repoRoot, s.deployDir)
}

func (s *Server) requireReady(w http.ResponseWriter) bool {
	d := s.discovery()
	if d.Ready {
		return true
	}
	writeError(w, http.StatusServiceUnavailable, "system requirements not met; fix the issues on the System step")
	return false
}
