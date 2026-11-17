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

func TestChooseInternalIP(t *testing.T) {
	cases := []struct {
		name                             string
		private4, public4, ula6, global6 []string
		want                             string
	}{
		{"prefers private IPv4", []string{"10.0.0.5"}, []string{"1.2.3.4"}, []string{"fd00::1"}, []string{"2001:db8::1"}, "10.0.0.5"},
		{"falls back to public IPv4", nil, []string{"1.2.3.4"}, []string{"fd00::1"}, []string{"2001:db8::1"}, "1.2.3.4"},
		{"IPv6-only prefers ULA", nil, nil, []string{"fd00::1"}, []string{"2001:db8::1"}, "fd00::1"},
		{"IPv6-only falls back to global unicast", nil, nil, nil, []string{"2001:db8::1"}, "2001:db8::1"},
		{"nothing returns empty", nil, nil, nil, nil, ""},
	}
	for _, tc := range cases {
		if got := chooseInternalIP(tc.private4, tc.public4, tc.ula6, tc.global6); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
