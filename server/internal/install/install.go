// Package install provides the one-line install script endpoint and agent
// binary download, mimicking the cloud bastion pattern:
//
//	curl -kfsSL 'http://<server>:<port>/api/v1/host/install_script?os_type=linux' | sudo bash -s -- --token=<token>
//
// The script downloads the agent binary from the server, installs it as a
// systemd service (Linux) or a scheduled task (Windows), and starts it.
package install

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"watchman/internal/binval"
	"watchman/internal/version"
	"watchman/server/internal/release"
	"watchman/server/internal/rpc"

	"github.com/gin-gonic/gin"
)

// Handler returns an http.HandlerFunc group for install endpoints.
type Handler struct {
	reg          *rpc.Registry
	binDir       string // directory containing pre-built agent binaries
	log          interface{ Printf(string, ...any) }
	ManifestPath string // path to manifest.json (source of truth for agent version & checksums)

	// UpgradePubKey is the hex Ed25519 public key agents should pin
	// (-upgrade-pubkey) to verify self-upgrade signatures. Empty disables.
	UpgradePubKey string
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
		script = linuxInstallScript(serverURL, token, h.UpgradePubKey)
	case "windows":
		script = windowsInstallScript(serverURL, token, h.UpgradePubKey)
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

	// 1. Search relative to the currently running server executable
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(exeDir, filename),
			filepath.Join(exeDir, "bin", filename),
			filepath.Join(exeDir, "data", "bin", filename),
		)
	}

	// 2. Search common installation and project directories
	candidates = append(candidates,
		filepath.Join("/opt/watchman/bin", filename),
		filepath.Join("/opt/watchman/data/bin", filename),
		filepath.Join("bin", filename),
		filepath.Join("data", "bin", filename),
		filepath.Join("dist", filename),
		filepath.Join("dist", "bin", filename),
		filepath.Join("bin/linux_amd64", filename),
	)

	// Canonical bare names for linux/amd64 (matches install.FindAgentBinary dev fallback)
	if goos == "linux" && goarch == "amd64" {
		if binDir != "" {
			candidates = append(candidates, filepath.Join(binDir, "watchman-agent"))
		}
		if exe, err := os.Executable(); err == nil {
			exeDir := filepath.Dir(exe)
			candidates = append(candidates,
				filepath.Join(exeDir, "watchman-agent"),
				filepath.Join(exeDir, "bin", "watchman-agent"),
				filepath.Join(exeDir, "data", "bin", "watchman-agent"),
			)
		}
		candidates = append(candidates,
			filepath.Join("/opt/watchman/bin", "watchman-agent"),
			filepath.Join("/opt/watchman/data/bin", "watchman-agent"),
			filepath.Join("bin/linux_amd64", "watchman-agent"),
			filepath.Join("bin", "watchman-agent"),
			filepath.Join("data", "bin", "watchman-agent"),
			filepath.Join("dist", "watchman-agent"),
			filepath.Join("dist", "bin", "watchman-agent"),
		)
	}

	// Canonical bare names for windows/amd64
	if goos == "windows" && goarch == "amd64" {
		if binDir != "" {
			candidates = append(candidates, filepath.Join(binDir, "watchman-agent.exe"))
		}
		if exe, err := os.Executable(); err == nil {
			exeDir := filepath.Dir(exe)
			candidates = append(candidates,
				filepath.Join(exeDir, "watchman-agent.exe"),
				filepath.Join(exeDir, "bin", "watchman-agent.exe"),
				filepath.Join(exeDir, "data", "bin", "watchman-agent.exe"),
			)
		}
		candidates = append(candidates,
			filepath.Join("/opt/watchman/bin", "watchman-agent.exe"),
			filepath.Join("/opt/watchman/data/bin", "watchman-agent.exe"),
			filepath.Join("bin", "watchman-agent.exe"),
			filepath.Join("data", "bin", "watchman-agent.exe"),
			filepath.Join("dist", "watchman-agent.exe"),
			filepath.Join("dist", "bin", "watchman-agent.exe"),
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

var agentDownloadMu sync.Mutex

// downloadAndCacheAgent downloads the agent binary for goos/goarch on demand from
// official GitHub Releases when running as a standalone binary without local agent files.
func (h *Handler) downloadAndCacheAgent(goos, goarch string) (string, error) {
	agentDownloadMu.Lock()
	defer agentDownloadMu.Unlock()

	// Double-check if another request just downloaded it
	if p, err := FindAgentBinary(h.binDir, goos, goarch); err == nil {
		return p, nil
	}

	filename := fmt.Sprintf("watchman-agent-%s-%s", goos, goarch)
	if goos == "windows" {
		filename += ".exe"
	}

	targetVersion := version.Get()
	repo := os.Getenv("WATCHMAN_REPO")
	if repo == "" {
		repo = "lizhixu/workbench"
	}

	if !strings.HasPrefix(targetVersion, "v") {
		if envV := os.Getenv("WATCHMAN_TARGET_VERSION"); envV != "" {
			targetVersion = envV
		} else {
			// Query latest release tag from GitHub API
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo), nil)
			if err == nil {
				req.Header.Set("User-Agent", "Watchman-Server")
				client := &http.Client{Timeout: 10 * time.Second}
				if resp, err := client.Do(req); err == nil {
					defer resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						var rel struct {
							TagName string `json:"tag_name"`
						}
						if err := json.NewDecoder(resp.Body).Decode(&rel); err == nil && rel.TagName != "" {
							targetVersion = rel.TagName
						}
					}
				}
			}
		}
	}

	if !strings.HasPrefix(targetVersion, "v") {
		return "", fmt.Errorf("无法确定目标 Release 版本 (当前版本: %s)", targetVersion)
	}

	downloadURL := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, targetVersion, filename)
	mirror := os.Getenv("GITHUB_MIRROR")
	if mirror == "" {
		mirror = os.Getenv("WATCHMAN_GITHUB_MIRROR")
	}
	mirror = strings.TrimSuffix(strings.TrimSpace(mirror), "/")
	if mirror != "" && strings.HasPrefix(downloadURL, "https://github.com/") {
		downloadURL = mirror + "/" + downloadURL
	}

	// 1. Resolve expected SHA-256 from local manifest or remote CHECKSUMS.txt
	expectedSha := ""
	mPaths := []string{h.ManifestPath, "/opt/watchman/manifest.json", "data/manifest.json", "manifest.json"}
	for _, mp := range mPaths {
		if mp != "" {
			if m, err := release.Load(mp); err == nil && m != nil {
				if sha, ok := m.AgentSha256(goos, goarch); ok && sha != "" {
					expectedSha = sha
					break
				}
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client := &http.Client{Timeout: 5 * time.Minute}

	// Fallback: fetch CHECKSUMS.txt from release if expectedSha is not in local manifest
	if expectedSha == "" {
		checksumURL := fmt.Sprintf("https://github.com/%s/releases/download/%s/CHECKSUMS.txt", repo, targetVersion)
		if mirror != "" && strings.HasPrefix(checksumURL, "https://github.com/") {
			checksumURL = mirror + "/" + checksumURL
		}
		cReq, cErr := http.NewRequestWithContext(ctx, "GET", checksumURL, nil)
		if cErr == nil {
			cReq.Header.Set("User-Agent", "Watchman-Server")
			if cResp, cErr := client.Do(cReq); cErr == nil && cResp.StatusCode == http.StatusOK {
				scanner := bufio.NewScanner(cResp.Body)
				for scanner.Scan() {
					line := strings.TrimSpace(scanner.Text())
					parts := strings.Fields(line)
					if len(parts) >= 2 {
						base := filepath.Base(parts[1])
						if base == filename || strings.Contains(parts[1], filename) {
							expectedSha = strings.ToLower(parts[0])
							break
						}
					}
				}
				cResp.Body.Close()
			}
		}
	}

	// Choose appropriate local cache directory
	var cacheDir string
	if h.binDir != "" {
		cacheDir = h.binDir
	} else if exe, err := os.Executable(); err == nil && filepath.Dir(exe) != "" {
		cacheDir = filepath.Join(filepath.Dir(exe), "data", "bin")
	} else {
		cacheDir = filepath.Join("data", "bin")
	}

	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		cacheDir = filepath.Join(os.TempDir(), "watchman-bin")
		_ = os.MkdirAll(cacheDir, 0755)
	}

	targetPath := filepath.Join(cacheDir, filename)
	tmpPath := targetPath + ".downloading"

	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return "", fmt.Errorf("创建下载请求失败: %w", err)
	}
	req.Header.Set("User-Agent", "Watchman-Server")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载 Agent 二进制失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载 Agent 二进制 HTTP 状态码 %d", resp.StatusCode)
	}

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return "", fmt.Errorf("创建临时缓存文件失败: %w", err)
	}

	written, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("保存 Agent 二进制失败: %w", err)
	}
	if written < binval.MinAgentBinarySize {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("下载的 Agent 二进制大小异常 (%d 字节)，低于安全下限 (%d 字节)", written, binval.MinAgentBinarySize)
	}

	// 2. Compute SHA-256 and verify if expected SHA-256 is available
	actualSha, err := binval.FileSha256(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("计算下载 Agent SHA-256 失败: %w", err)
	}
	if expectedSha != "" && !strings.EqualFold(actualSha, expectedSha) {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("Agent 二进制 SHA-256 校验失败 (期望: %s, 实际: %s)，二进制可能损坏或被篡改", expectedSha, actualSha)
	}

	// 3. Validate binary format and architecture compatibility
	if err := binval.ValidateFormat(tmpPath, goos, goarch, binval.MinAgentBinarySize); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("Agent 二进制格式/架构安全校验失败: %w", err)
	}

	// 4. Smoke-test dry run (-version) if running on same host OS/arch
	if goos == runtime.GOOS && goarch == runtime.GOARCH {
		if _, err := binval.SmokeTest(ctx, tmpPath, goos, goarch, "watchman-agent"); err != nil {
			_ = os.Remove(tmpPath)
			return "", fmt.Errorf("Agent 二进制运行自检失败: %w", err)
		}
	}

	if err := os.Rename(tmpPath, targetPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("重命名缓存文件失败: %w", err)
	}
	_ = os.Chmod(targetPath, 0755)

	return targetPath, nil
}

// agentBinary serves the pre-built agent binary for the requested OS/arch.
func (h *Handler) agentBinary(c *gin.Context) {
	goos := c.DefaultQuery("os", "linux")
	goarch := c.DefaultQuery("arch", "amd64")

	foundPath, err := FindAgentBinary(h.binDir, goos, goarch)
	if err != nil {
		// Attempt on-demand download from GitHub Releases
		if downloaded, dlErr := h.downloadAndCacheAgent(goos, goarch); dlErr == nil {
			foundPath = downloaded
		} else {
			filename := fmt.Sprintf("watchman-agent-%s-%s", goos, goarch)
			if goos == "windows" {
				filename += ".exe"
			}
			c.String(http.StatusNotFound, "binary not found: %s (auto-download error: %v; build it with: GOOS=%s GOARCH=%s go build -o %s ./agent/cmd/watchman-agent)", filename, dlErr, goos, goarch, filepath.Join(h.binDir, filename))
			return
		}
	} else {
		// Verify local binary integrity before serving
		if err := binval.ValidateFormat(foundPath, goos, goarch, binval.MinAgentBinarySize); err != nil {
			// Local file is damaged or corrupted; try re-downloading
			_ = os.Remove(foundPath)
			if downloaded, dlErr := h.downloadAndCacheAgent(goos, goarch); dlErr == nil {
				foundPath = downloaded
			} else {
				c.String(http.StatusInternalServerError, "local agent binary validation failed: %v", err)
				return
			}
		}
	}

	filename := filepath.Base(foundPath)
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))
	c.Header("Content-Type", "application/octet-stream")
	c.File(foundPath)
}

// linuxInstallScript generates a bash install script for Linux agents.
// upgradePubKey (hex) is passed to the agent as -upgrade-pubkey so it
// verifies self-upgrade signatures; empty disables.
func linuxInstallScript(serverURL, token, upgradePubKey string) string {
	grpcHost := extractGrpcHost(serverURL)
	pubkeyFlag := ""
	if upgradePubKey != "" {
		pubkeyFlag = " -upgrade-pubkey " + upgradePubKey
	}
	return fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail

# Watchman Agent - one-line installer (Linux)
# Generated by the control server for token: %s
#
# Install:   curl -kfsSL '%s/install?token=<token>' | sudo bash
# Uninstall: curl -kfsSL '%s/install?token=<token>' | sudo bash -s -- uninstall

WATCHMAN_SERVER="%s"
WATCHMAN_GRPC="%s"
WATCHMAN_TOKEN="%s"
WATCHMAN_BIN="/usr/local/bin/watchman-agent"
WATCHMAN_DIR="/var/lib/watchman"
WATCHMAN_CONF="${WATCHMAN_DIR}/agent.conf"
SYSTEMD_SERVICE="/etc/systemd/system/watchman-agent.service"

# --- Uninstall mode ---------------------------------------------------
if [ "${1:-}" = "uninstall" ]; then
  echo ">>> Uninstalling Watchman Agent..."
  if [ -f "$SYSTEMD_SERVICE" ]; then
    sudo systemctl stop watchman-agent 2>/dev/null || true
    sudo systemctl disable watchman-agent 2>/dev/null || true
    sudo rm -f "$SYSTEMD_SERVICE"
    sudo systemctl daemon-reload
    echo ">>> Removed systemd service."
  fi
  if [ -f /etc/init.d/watchman-agent ]; then
    sudo service watchman-agent stop 2>/dev/null || true
    sudo rm -f /etc/init.d/watchman-agent
    echo ">>> Removed sysvinit service."
  fi
  sudo pkill -f "watchman-agent" 2>/dev/null || true
  sudo rm -f "$WATCHMAN_BIN"
  sudo rm -rf "$WATCHMAN_DIR"
  echo ">>> Watchman Agent uninstalled."
  exit 0
fi
# --- End uninstall mode -----------------------------------------------

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
ExecStart=/usr/local/bin/watchman-agent -server ${WATCHMAN_GRPC} -enroll ${WATCHMAN_TOKEN} -state /var/lib/watchman/state.json%s
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
  sudo "$WATCHMAN_BIN" -server "$WATCHMAN_GRPC" -enroll "$WATCHMAN_TOKEN" -state "$WATCHMAN_DIR/state.json"%s &
  echo ">>> Agent started in background."
fi

echo ""
echo "✅ Watchman Agent installed successfully!"
echo "   Server: ${WATCHMAN_SERVER}"
echo "   gRPC:   ${WATCHMAN_GRPC}"
echo "   Token:  ${WATCHMAN_TOKEN:0:8}..."
echo ""
echo "The agent will auto-register with the control server and appear in the host list."
`, token, serverURL, serverURL, serverURL, grpcHost, token, pubkeyFlag, pubkeyFlag)
}

// windowsInstallScript generates a PowerShell install script for Windows agents.
// upgradePubKey (hex) is passed to the agent as -upgrade-pubkey so it
// verifies self-upgrade signatures; empty disables.
func windowsInstallScript(serverURL, token, upgradePubKey string) string {
	grpcHost := extractGrpcHost(serverURL)
	pubkeyArg := ""
	if upgradePubKey != "" {
		pubkeyArg = " -upgrade-pubkey " + upgradePubKey
	}
	return fmt.Sprintf(`# Watchman Agent - one-line installer (Windows)
# Run in PowerShell as Administrator:
#   Install:   irm %s/install?os_type=windows^&token=%s | iex
#   Uninstall: $f="$env:TEMP\watchman-install.ps1"; irm %s/install?os_type=windows^&token=%s -OutFile $f; & $f -Uninstall
#     (piping to iex cannot pass the -Uninstall switch, so save to a file first)
param([switch]$Uninstall)

$ErrorActionPreference = "Stop"

$WatchmanServer = "%s"
$WatchmanGrpc   = "%s"
$WatchmanToken  = "%s"
$WatchmanBin    = "C:\Program Files\Watchman\watchman-agent.exe"
$WatchmanDir    = "C:\Program Files\Watchman"
$WatchmanState  = "C:\ProgramData\Watchman\state.json"

if ($Uninstall) {
    Write-Host ">>> Uninstalling Watchman Agent..."
    Unregister-ScheduledTask -TaskName "WatchmanAgent" -Confirm:$false -ErrorAction SilentlyContinue
    Stop-ScheduledTask -TaskName "WatchmanAgent" -ErrorAction SilentlyContinue
    Get-Process -Name "watchman-agent" -ErrorAction SilentlyContinue | Stop-Process -Force
    Start-Sleep -Seconds 2
    Remove-Item -Path $WatchmanBin -Force -ErrorAction SilentlyContinue
    Remove-Item -Path $WatchmanDir -Recurse -Force -ErrorAction SilentlyContinue
    Remove-Item -Path (Split-Path $WatchmanState) -Recurse -Force -ErrorAction SilentlyContinue
    Write-Host ">>> Watchman Agent uninstalled."
    exit 0
}

Write-Host ">>> Creating directories..."
New-Item -ItemType Directory -Force -Path $WatchmanDir | Out-Null
New-Item -ItemType Directory -Force -Path (Split-Path $WatchmanState) | Out-Null

Write-Host ">>> Downloading agent binary..."
Invoke-WebRequest -Uri "$WatchmanServer/api/v1/agent/binary?os=windows&arch=amd64" -OutFile $WatchmanBin -UseBasicParsing

Write-Host ">>> Registering as scheduled task..."
$action = New-ScheduledTaskAction -Execute $WatchmanBin -Argument "-server $WatchmanGrpc -enroll $WatchmanToken -state $WatchmanState%s"
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
`, serverURL, token, serverURL, token, serverURL, grpcHost, token, pubkeyArg)
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
