package setup

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsUsableIPv4(t *testing.T) {
	assert.False(t, isUsableIPv4(net.ParseIP("127.0.0.1")))
	assert.False(t, isUsableIPv4(net.ParseIP("169.254.1.1")))
	assert.False(t, isUsableIPv4(net.ParseIP("::1")))
	assert.True(t, isUsableIPv4(net.ParseIP("192.168.1.15")))
	assert.True(t, isUsableIPv4(net.ParseIP("10.0.0.5")))
}

func TestSkipInterface(t *testing.T) {
	assert.True(t, skipInterface("lo"))
	assert.True(t, skipInterface("docker0"))
	assert.True(t, skipInterface("onvif-1"))
	assert.False(t, skipInterface("eth0"))
	assert.False(t, skipInterface("ens18"))
}

func TestUsableIPv4AddrsFiltersLoopback(t *testing.T) {
	addrs := []net.Addr{
		&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
		&net.IPNet{IP: net.ParseIP("192.168.1.8"), Mask: net.CIDRMask(24, 32)},
	}
	ips := usableIPv4Addrs(addrs)
	assert.Equal(t, []string{"192.168.1.8"}, ips)
}
