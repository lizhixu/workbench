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

func getInternalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	var fallbacks []string
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() {
			if ip4 := ipNet.IP.To4(); ip4 != nil {
				ipStr := ip4.String()
				// Prefer standard private LAN IP blocks
				if strings.HasPrefix(ipStr, "10.") ||
					strings.HasPrefix(ipStr, "192.168.") ||
					(strings.HasPrefix(ipStr, "172.") && is172Private(ip4)) {
					return ipStr
				}
				fallbacks = append(fallbacks, ipStr)
			}
		}
	}
	if len(fallbacks) > 0 {
		return fallbacks[0]
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
