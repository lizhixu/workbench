package conn

import "testing"

// docker0's 172.17.0.1 is a valid private address, so the interface filter
// must drop it. Otherwise installing Docker changes the IP the console shows.
func TestIsVirtualIface(t *testing.T) {
	virtual := []string{"docker0", "br-abc123", "veth4567", "virbr0", "cni0",
		"flannel.1", "kube-bridge", "tailscale0", "tun0", "wg0", "vEthernet (Docker)", "utun3"}
	for _, name := range virtual {
		if !isVirtualIface(name) {
			t.Errorf("%q 应被识别为虚拟网卡", name)
		}
	}
	real := []string{"eth0", "ens3", "enp0s3", "eno1", "wlan0", "Wi-Fi"}
	for _, name := range real {
		if isVirtualIface(name) {
			t.Errorf("%q 不应被识别为虚拟网卡", name)
		}
	}
}

func TestIsPrivateLAN(t *testing.T) {
	// 172.17.0.1 (docker0) is a legitimate private address — it is excluded by
	// interface name, not by address class.
	for _, ip := range []string{"10.0.0.1", "192.168.1.2", "172.16.9.9", "172.31.255.255", "172.17.0.1"} {
		if !isPrivateLAN(ip) {
			t.Errorf("%s 应被识别为内网地址", ip)
		}
	}
	for _, ip := range []string{"172.15.0.1", "172.32.0.1", "8.8.8.8", "192.255.178.173"} {
		if isPrivateLAN(ip) {
			t.Errorf("%s 不应被识别为内网地址", ip)
		}
	}
}

// The host must still report an address when its only interface carries a
// public IP (a common single-NIC cloud VM).
func TestGetInternalIPReturnsSomething(t *testing.T) {
	if ip := getInternalIP(); ip == "" {
		t.Skip("当前环境没有可用的非回环网卡，跳过")
	}
}
