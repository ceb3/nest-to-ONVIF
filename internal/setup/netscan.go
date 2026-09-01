package setup

import (
	"net"
	"os"
	"runtime"
	"sort"
	"strings"
)

// HostInterface is one host network interface and its usable IPv4 addresses.
type HostInterface struct {
	Name      string   `json:"name"`
	MAC       string   `json:"mac,omitempty"`
	Addresses []string `json:"addresses"`
	Default   bool     `json:"default"`
}

func listHostInterfaces() ([]HostInterface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	defaultIface := detectParentIface()
	out := make([]HostInterface, 0, len(ifaces))
	for _, iface := range ifaces {
		if skipInterface(iface.Name) {
			continue
		}
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		ips := usableIPv4Addrs(addrs)
		if len(ips) == 0 {
			continue
		}
		mac := iface.HardwareAddr.String()
		if mac == "" {
			mac = ""
		}
		out = append(out, HostInterface{
			Name:      iface.Name,
			MAC:       mac,
			Addresses: ips,
			Default:   iface.Name == defaultIface,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Default != out[j].Default {
			return out[i].Default
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func usableIPv4Addrs(addrs []net.Addr) []string {
	seen := map[string]struct{}{}
	var ips []string
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if !isUsableIPv4(ip) {
			continue
		}
		s := ip.String()
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		ips = append(ips, s)
	}
	sort.Strings(ips)
	return ips
}

func isUsableIPv4(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	if ip4.IsLoopback() || ip4.IsLinkLocalUnicast() || ip4.IsUnspecified() {
		return false
	}
	return true
}

func skipInterface(name string) bool {
	if name == "lo" {
		return true
	}
	prefixes := []string{
		"docker", "veth", "br-", "onvif-", "tun", "tap", "wg", "utun",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func detectHostIP() string {
	ifaces, err := listHostInterfaces()
	if err != nil || len(ifaces) == 0 {
		return detectHostIPFallback()
	}
	for _, iface := range ifaces {
		if iface.Default && len(iface.Addresses) > 0 {
			return iface.Addresses[0]
		}
	}
	return ifaces[0].Addresses[0]
}

func detectHostIPFallback() string {
	conn, err := net.Dial("udp4", "10.255.255.255:1")
	if err != nil {
		return ""
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return ""
	}
	if ip4 := addr.IP.To4(); ip4 != nil && isUsableIPv4(ip4) {
		return ip4.String()
	}
	return ""
}

func ipv4OnInterface(name string) string {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return ""
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return ""
	}
	ips := usableIPv4Addrs(addrs)
	if len(ips) == 0 {
		return ""
	}
	return ips[0]
}

func detectParentIface() string {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile("/proc/net/route")
		if err != nil {
			return "eth0"
		}
		for _, line := range strings.Split(string(data), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[1] == "00000000" {
				return fields[0]
			}
		}
		return "eth0"
	}
	hostIP := detectHostIPFallback()
	if hostIP == "" {
		return ""
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if skipInterface(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, ip := range usableIPv4Addrs(addrs) {
			if ip == hostIP {
				return iface.Name
			}
		}
	}
	return ""
}
