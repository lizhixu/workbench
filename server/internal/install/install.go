// Package install provides the one-line install script endpoint and agent
// binary download, mimicking the cloud bastion pattern:
//
//   curl -kfsSL 'http://<server>:<port>/api/v1/host/install_script?os_type=linux' | sudo bash -s -- --token=<token>
//
// The script downloads the agent binary from the server, installs it as a
// systemd service (Linux) or a scheduled task (Windows), and starts it.
package install

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"watchman/server/internal/rpc"

	"github.com/gin-gonic/gin"
)

// Handler returns an http.HandlerFunc group for install endpoints.
type Handler struct {
	reg     *rpc.Registry
	binDir  string // directory containing pre-built agent binaries
	log     interface{ Printf(string, ...any) }
}

// NewHandler creates an install handler. binDir should contain files like
// watchman-agent-linux-amd64, watchman-agent-linux-arm64, etc.
func NewHandler(reg *rpc.Registry, binDir string) *Handler {
	return &Handler{reg: reg, binDir: binDir}
}

// RegisterRoutes mounts install endpoints on the gin engine.
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	// Standard root /install and /install_script endpoints
	r.GET("/install", h.installScript)
	r.HEAD("/install", h.installScript)
	r.GET("/install_script", h.installScript)
	r.HEAD("/install_script", h.installScript)

	// API-namespaced install endpoints
	r.GET("/api/v1/host/install_script", h.installScript)
	r.HEAD("/api/v1/host/install_script", h.installScript)
	r.GET("/api/v1/install", h.installScript)
	r.HEAD("/api/v1/install", h.installScript)

	// Agent binary download (no auth; the install script uses this).
	r.GET("/api/v1/agent/binary", h.agentBinary)
	r.HEAD("/api/v1/agent/binary", h.agentBinary)
	r.GET("/agent/binary", h.agentBinary)
	r.HEAD("/agent/binary", h.agentBinary)
}

// extractGrpcHost derives the gRPC host:port address from the given server HTTP URL.
func extractGrpcHost(serverURL string) string {
	u := serverURL
	u = strings.TrimPrefix(u, "http://")
	u = strings.TrimPrefix(u, "https://")
	if idx := strings.Index(u, "/"); idx != -1 {
		u = u[:idx]
	}
	host := u
	if h, _, err := net.SplitHostPort(u); err == nil {
		host = h
	}
	return net.JoinHostPort(host, "9090")
}

// installScript returns a shell script that installs the agent on the target host.
func (h *Handler) installScript(c *gin.Context) {
	osType := c.DefaultQuery("os_type", "linux")
	token := c.Query("token")

	// Determine the server's external URL from the request.
	scheme := "http"
	if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	// Allow override via query param (for NAT/proxy setups).
	serverURL := c.DefaultQuery("server", fmt.Sprintf("%s://%s", scheme, c.Request.Host))

	if token == "" {
		// Do NOT mint a token silently here: an unauthenticated caller
		// could otherwise generate arbitrary enroll tokens. Tokens must
		// come from an authenticated console session via
		// POST /api/v1/hosts/enroll (or be passed explicitly).
		c.String(http.StatusBadRequest, "missing token: generate one from the console (POST /api/v1/hosts/enroll) and retry with ?token=<token>")
		return
	}

	var script string
	switch osType {
	case "linux":
		script = linuxInstallScript(serverURL, token)
	case "windows":
		script = windowsInstallScript(serverURL, token)
	default:
		c.String(http.StatusBadRequest, "unsupported os_type: %s (use linux or windows)", osType)
		return
	}

	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.String(http.StatusOK, script)
}

// FindAgentBinary returns the path to the pre-built agent binary for the requested OS/arch.
// binDir is searched first if non-empty, followed by standard fallback locations.
func FindAgentBinary(binDir, goos, goarch string) (string, error) {
	if goos == "" {
		goos = "linux"
	}
	if goarch == "" {
		goarch = "amd64"
	}

	filename := fmt.Sprintf("watchman-agent-%s-%s", goos, goarch)
	if goos == "windows" {
		filename += ".exe"
	}

	var candidates []string
	if binDir != "" {
		candidates = append(candidates, filepath.Join(binDir, filename))
	}
	candidates = append(candidates,
		filepath.Join("/opt/watchman/bin", filename),
		filepath.Join("bin", filename),
		filepath.Join("bin/linux_amd64", filename),
	)
	if goos == "linux" && goarch == "amd64" {
		if binDir != "" {
			candidates = append(candidates, filepath.Join(binDir, "watchman-agent"))
		}
		candidates = append(candidates,
			filepath.Join("/opt/watchman/bin", "watchman-agent"),
			filepath.Join("bin/linux_amd64", "watchman-agent"),
			filepath.Join("bin", "watchman-agent"),
		)
	}
	if goos == "windows" && goarch == "amd64" {
		if binDir != "" {
			candidates = append(candidates, filepath.Join(binDir, "watchman-agent.exe"))
		}
		candidates = append(candidates,
			filepath.Join("bin", "watchman-agent.exe"),
		)
	}

	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("agent binary not found for %s/%s", goos, goarch)
}

// AgentBinarySha256 computes the hex-encoded SHA-256 checksum of the agent binary for OS/arch.
func AgentBinarySha256(binDir, goos, goarch string) (string, error) {
	path, err := FindAgentBinary(binDir, goos, goarch)
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// agentBinary serves the pre-built agent binary for the requested OS/arch.
func (h *Handler) agentBinary(c *gin.Context) {
	goos := c.DefaultQuery("os", "linux")
	goarch := c.DefaultQuery("arch", "amd64")

	foundPath, err := FindAgentBinary(h.binDir, goos, goarch)
	if err != nil {
		filename := fmt.Sprintf("watchman-agent-%s-%s", goos, goarch)
		if goos == "windows" {
			filename += ".exe"
		}
		c.String(http.StatusNotFound, "binary not found: %s (build it with: GOOS=%s GOARCH=%s go build -o %s ./agent/cmd/watchman-agent)", filename, goos, goarch, filepath.Join(h.binDir, filename))
		return
	}

	filename := filepath.Base(foundPath)
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))
	c.Header("Content-Type", "application/octet-stream")
	c.File(foundPath)
}

// linuxInstallScript generates a bash install script for Linux agents.
func linuxInstallScript(serverURL, token string) string {
	grpcHost := extractGrpcHost(serverURL)
	return fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail

# Watchman Agent - one-line installer (Linux)
# Generated by the control server for token: %s

WATCHMAN_SERVER="%s"
WATCHMAN_GRPC="%s"
WATCHMAN_TOKEN="%s"
WATCHMAN_BIN="/usr/local/bin/watchman-agent"
WATCHMAN_DIR="/var/lib/watchman"
WATCHMAN_CONF="${WATCHMAN_DIR}/agent.conf"
SYSTEMD_SERVICE="/etc/systemd/system/watchman-agent.service"

# Detect architecture.
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64)  GOARCH="amd64" ;;
  aarch64|arm64) GOARCH="arm64" ;;
  *) echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

echo ">>> Detecting init system..."
if command -v systemctl &>/dev/null; then
  INIT="systemd"
else
  INIT="sysvinit"
fi

echo ">>> Creating directories..."
sudo mkdir -p "$WATCHMAN_DIR"

echo ">>> Downloading agent binary (linux/$GOARCH)..."
sudo curl -kfsSL "${WATCHMAN_SERVER}/api/v1/agent/binary?os=linux&arch=${GOARCH}" -o "$WATCHMAN_BIN"
sudo chmod +x "$WATCHMAN_BIN"

echo ">>> Writing configuration..."
sudo tee "$WATCHMAN_CONF" > /dev/null <<EOF
server=${WATCHMAN_GRPC}
token=${WATCHMAN_TOKEN}
EOF

if [ "$INIT" = "systemd" ]; then
  echo ">>> Installing systemd service..."
  sudo tee "$SYSTEMD_SERVICE" > /dev/null <<'UNITEOF'
[Unit]
Description=Watchman Cloud Bastion Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/watchman-agent -server ${WATCHMAN_GRPC} -enroll ${WATCHMAN_TOKEN} -state /var/lib/watchman/state.json
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
UNITEOF

  # Replace placeholders with actual values.
  sudo sed -i "s|\${WATCHMAN_GRPC}|${WATCHMAN_GRPC}|g" "$SYSTEMD_SERVICE"
  sudo sed -i "s|\${WATCHMAN_TOKEN}|${WATCHMAN_TOKEN}|g" "$SYSTEMD_SERVICE"

  sudo systemctl daemon-reload
  sudo systemctl enable watchman-agent
  sudo systemctl restart watchman-agent
  echo ">>> Agent started! Check status: sudo systemctl status watchman-agent"
else
  echo ">>> Starting agent (no systemd)..."
  sudo "$WATCHMAN_BIN" -server "$WATCHMAN_GRPC" -enroll "$WATCHMAN_TOKEN" -state "$WATCHMAN_DIR/state.json" &
  echo ">>> Agent started in background."
fi

echo ""
echo "✅ Watchman Agent installed successfully!"
echo "   Server: ${WATCHMAN_SERVER}"
echo "   gRPC:   ${WATCHMAN_GRPC}"
echo "   Token:  ${WATCHMAN_TOKEN:0:8}..."
echo ""
echo "The agent will auto-register with the control server and appear in the host list."
`, token, serverURL, grpcHost, token)
}

// windowsInstallScript generates a PowerShell install script for Windows agents.
func windowsInstallScript(serverURL, token string) string {
	grpcHost := extractGrpcHost(serverURL)
	return fmt.Sprintf(`# Watchman Agent - one-line installer (Windows)
# Run in PowerShell as Administrator:
#   irm %s/install?os_type=windows^&token=%s | iex

$ErrorActionPreference = "Stop"

$WatchmanServer = "%s"
$WatchmanGrpc   = "%s"
$WatchmanToken  = "%s"
$WatchmanBin    = "C:\Program Files\Watchman\watchman-agent.exe"
$WatchmanDir    = "C:\Program Files\Watchman"
$WatchmanState  = "C:\ProgramData\Watchman\state.json"

Write-Host ">>> Creating directories..."
New-Item -ItemType Directory -Force -Path $WatchmanDir | Out-Null
New-Item -ItemType Directory -Force -Path (Split-Path $WatchmanState) | Out-Null

Write-Host ">>> Downloading agent binary..."
Invoke-WebRequest -Uri "$WatchmanServer/api/v1/agent/binary?os=windows&arch=amd64" -OutFile $WatchmanBin -UseBasicParsing

Write-Host ">>> Registering as scheduled task..."
$action = New-ScheduledTaskAction -Execute $WatchmanBin -Argument "-server $WatchmanGrpc -enroll $WatchmanToken -state $WatchmanState"
$trigger = New-ScheduledTaskTrigger -AtStartup
$settings = New-ScheduledTaskSettingsSet -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -DontStopIfGoingOnBatteries -AllowStartIfOnBatteries
$principal = New-ScheduledTaskPrincipal -UserId "SYSTEM" -LogonType ServiceAccount -RunLevel Highest

Register-ScheduledTask -TaskName "WatchmanAgent" -Action $action -Trigger $trigger -Settings $settings -Principal $principal -Force | Out-Null

Write-Host ">>> Starting agent..."
Start-ScheduledTask -TaskName "WatchmanAgent"

Write-Host ""
Write-Host "✅ Watchman Agent installed successfully!"
Write-Host "   Server: $WatchmanServer"
Write-Host "   gRPC:   $WatchmanGrpc"
Write-Host "   Token:  $($WatchmanToken.Substring(0,8))..."
`, serverURL, token, serverURL, grpcHost, token)
}

// InstallCmdForOS returns the one-line install command for the given OS and token.
func InstallCmdForOS(serverURL, osType, token string) string {
	if token == "" {
		token = "<TOKEN>"
	}
	switch osType {
	case "windows":
		return fmt.Sprintf(`irm %s/install?os_type=windows^&token=%s | iex`, serverURL, token)
	default:
		return fmt.Sprintf(`curl -kfsSL '%s/install?token=%s' | sudo bash`, serverURL, token)
	}
}

// AvailableBinaries returns a list of available agent binaries in the bin dir.
func AvailableBinaries(binDir string) []string {
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "watchman-agent") {
			out = append(out, e.Name())
		}
	}
	return out
}
