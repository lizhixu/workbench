package cert

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// publicIPEchoServices are tried in order to discover the server's public
// egress IP (used for IP certificate issuance).
var publicIPEchoServices = []string{
	"https://api.ipify.org",
	"https://ifconfig.me/ip",
}

// DetectPublicIP returns the server's public IP as seen from the internet.
func DetectPublicIP() (string, error) {
	client := &http.Client{Timeout: 8 * time.Second}
	for _, url := range publicIPEchoServices {
		if ip := fetchEchoIP(client, url); ip != "" {
			return ip, nil
		}
	}
	return "", fmt.Errorf("无法检测公网 IP，请手动填写")
}

func fetchEchoIP(client *http.Client, url string) string {
	resp, err := client.Get(url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return ""
	}
	ip := strings.TrimSpace(string(body))
	if net.ParseIP(ip) == nil {
		return ""
	}
	return ip
}
