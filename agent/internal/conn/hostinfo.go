package conn

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
)

type HostHardwareInfo struct {
	Distro     string
	CPUCores   int32
	MemTotal   int64
	InternalIP string
	PublicIP   string
	Location   string
	Uptime     int64
}

// CollectHostHardwareInfo gathers OS, CPU, memory, network, and location info safely.
func CollectHostHardwareInfo() HostHardwareInfo {
	info := HostHardwareInfo{
		Distro:     getDistro(),
		CPUCores:   getCPUCores(),
		MemTotal:   getMemTotal(),
		InternalIP: getInternalIP(),
	}

	if up, err := host.Uptime(); err == nil {
		info.Uptime = int64(up)
	}

	// Try fetching public IP and geo location with a 2-second timeout so it never blocks
	pubIP, loc := getPublicIPAndLocation()
	info.PublicIP = pubIP
	info.Location = loc

	return info
}

func getDistro() string {
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/etc/os-release"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "PRETTY_NAME=") {
					val := strings.TrimPrefix(line, "PRETTY_NAME=")
					val = strings.Trim(val, `"'`)
					if val != "" {
						return val
					}
				}
			}
		}
	}
	if hi, err := host.Info(); err == nil && hi != nil {
		p := hi.Platform
		if p != "" {
			// Capitalize first letter
			pCap := strings.ToUpper(p[:1]) + p[1:]
			if hi.PlatformVersion != "" {
				return pCap + " " + hi.PlatformVersion
			}
			return pCap
		}
	}
	if runtime.GOOS == "windows" {
		return "Windows"
	}
	if runtime.GOOS == "darwin" {
		return "macOS"
	}
	return runtime.GOOS
}

func getCPUCores() int32 {
	if n, err := cpu.Counts(true); err == nil && n > 0 {
		return int32(n)
	}
	return int32(runtime.NumCPU())
}

func getMemTotal() int64 {
	if v, err := mem.VirtualMemory(); err == nil && v != nil && v.Total > 0 {
		return int64(v.Total)
	}
	return 0
}

// virtualIfacePrefixes match interfaces that belong to container, VM or VPN
// plumbing rather than to the host's own network. They must be skipped when
// reporting the host address: Docker's bridge is 172.17.0.1, a perfectly valid
// private address, so without this filter installing Docker silently changes
// the IP the console shows for the host.
var virtualIfacePrefixes = []string{
	"docker", "br-", "veth", "virbr", "vmnet", "cni", "flannel", "kube",
	"tailscale", "zt", "wg", "tun", "tap", "utun", "vethernet", "hyper-v",
	"nebula", "wireguard", "zerotier",
}

func isVirtualIface(name string) bool {
	n := strings.ToLower(name)
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

func isPrivateLAN(ip string) bool {
	if strings.HasPrefix(ip, "10.") || strings.HasPrefix(ip, "192.168.") {
		return true
	}
	return strings.HasPrefix(ip, "172.") && is172Private(net.ParseIP(ip))
}

// getInternalIP reports the host's own IPv4 address, preferring a private LAN
// address and ignoring container/VPN interfaces.
func getInternalIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	var privateAddrs, publicAddrs []string
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		if isVirtualIface(ifi.Name) {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP.IsLoopback() || ipNet.IP.IsLinkLocalUnicast() {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil {
				continue
			}
			s := ip4.String()
			if isPrivateLAN(s) {
				privateAddrs = append(privateAddrs, s)
			} else {
				publicAddrs = append(publicAddrs, s)
			}
		}
	}
	if len(privateAddrs) > 0 {
		return privateAddrs[0]
	}
	if len(publicAddrs) > 0 {
		return publicAddrs[0]
	}
	return ""
}

func is172Private(ip net.IP) bool {
	if len(ip) == 4 || len(ip) == 16 {
		ip4 := ip.To4()
		if ip4 != nil && ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31 {
			return true
		}
	}
	return false
}

type ipAPIResponse struct {
	Status     string `json:"status"`
	Country    string `json:"country"`
	RegionName string `json:"regionName"`
	City       string `json:"city"`
	Query      string `json:"query"`
}

func getPublicIPAndLocation() (string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", "http://ip-api.com/json/?lang=zh-CN", nil)
	if err != nil {
		return "", ""
	}
	req.Header.Set("User-Agent", "curl/7.88.1")

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", ""
	}

	var res ipAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil || res.Status != "success" {
		return "", ""
	}

	pubIP := res.Query
	var locParts []string
	if res.RegionName != "" {
		locParts = append(locParts, res.RegionName)
	}
	if res.City != "" && res.City != res.RegionName {
		locParts = append(locParts, res.City)
	}
	location := strings.Join(locParts, "-")
	if location == "" && res.Country != "" {
		location = res.Country
	}

	return pubIP, location
}
