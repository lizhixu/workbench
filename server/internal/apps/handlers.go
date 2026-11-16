// Package apps provides application templates and Docker installation utilities.
package apps

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/gin-gonic/gin"
	"watchman/proto/agentpb"
	"watchman/server/internal/rpc"
)

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Handlers manages legacy App Store read-only routes and host-level Docker utilities.
type Handlers struct {
	reg *rpc.Registry
}

// NewHandlers creates a new apps handler instance.
func NewHandlers(reg *rpc.Registry) *Handlers {
	return &Handlers{reg: reg}
}

// Register mounts read-only routes on rg and mutating routes on writeRG.
func (h *Handlers) Register(rg, writeRG *gin.RouterGroup) {
	rg.GET("/apps/catalog", h.getCatalog)
	writeRG.POST("/hosts/:id/docker/install-script", h.installDockerScript)
}

func (h *Handlers) getCatalog(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": Catalog})
}

// dockerInstallScript is the one-click Docker installer pushed to the agent.
//
// Domestic hosts frequently cannot even resolve get.docker.com (blocked or
// hijacked DNS), so the official convenience script alone is not a usable
// path there: the domestic docker-ce mirrors are tried first and the official
// script is the last resort. The script always runs on the target host, never
// in the browser, so the commands executed cannot be tampered with by clients.
var dockerInstallScript = `#!/bin/sh
# Watchman one-click Docker installer.
set -u

if command -v docker >/dev/null 2>&1; then
    echo "Docker is already installed."
    docker --version
    exit 0
fi

log() { echo "[watchman] $*"; }

# fetch URL OUTFILE — curl first, wget fallback (minimal images often lack curl).
fetch() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --connect-timeout 8 --max-time 30 "$1" -o "$2"
    elif command -v wget >/dev/null 2>&1; then
        wget -q -T 30 -O "$2" "$1"
    else
        log "neither curl nor wget is available"
        return 1
    fi
}

# Start the daemon and enable it at boot; best effort on both init systems.
start_docker() {
    if command -v systemctl >/dev/null 2>&1; then
        systemctl enable --now docker 2>/dev/null || systemctl start docker 2>/dev/null || true
    fi
    if ! docker info >/dev/null 2>&1; then
        service docker start 2>/dev/null || true
    fi
}

# Print the Debian/Ubuntu codename (e.g. bookworm, jammy).
debian_codename() {
    if [ -r /etc/os-release ]; then
        # shellcheck disable=SC1091
        . /etc/os-release
        if [ -n "${VERSION_CODENAME:-}" ]; then
            echo "$VERSION_CODENAME"
            return 0
        fi
        if [ -n "${UBUNTU_CODENAME:-}" ]; then
            echo "$UBUNTU_CODENAME"
            return 0
        fi
    fi
    return 1
}

# Package id used in docker-ce repository paths. RHEL rebuilds (Rocky, Alma)
# are served by the centos tree: the mirror does not publish rocky/almalinux
# directories, while centos/$releasever/$basearch is ABI compatible.
docker_repo_distro() {
    case "$1" in
        ubuntu|debian|centos|rhel|fedora) echo "$1" ;;
        rocky|almalinux) echo "centos" ;;
        *) return 1 ;;
    esac
}

# install_apt MIRROR DISTRO — docker-ce from that mirror's apt repository.
install_apt() {
    mirror="$1"; distro="$2"
    code="$(debian_codename)" || { log "cannot detect $distro codename"; return 1; }
    arch="$(dpkg --print-architecture 2>/dev/null || echo amd64)"
    log "configuring apt repository: ${mirror}/linux/${distro} (${code}/${arch})"
    install -m 0755 -d /etc/apt/keyrings || return 1
    if ! fetch "${mirror}/linux/${distro}/gpg" /etc/apt/keyrings/docker.asc; then
        log "failed to fetch the GPG key from ${mirror}"
        return 1
    fi
    chmod a+r /etc/apt/keyrings/docker.asc
    echo "deb [arch=${arch} signed-by=/etc/apt/keyrings/docker.asc] ${mirror}/linux/${distro} ${code} stable" \
        > /etc/apt/sources.list.d/docker.list
    export DEBIAN_FRONTEND=noninteractive
    # A broken third-party source elsewhere in sources.list must not abort
    # the whole run; if this repo is unusable the install below fails and the
    # next mirror is tried.
    apt-get update -y >/dev/null 2>&1 || true
    apt-get install -y -o Dpkg::Options::=--force-confold \
        docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin && return 0
    # Older repositories may not carry the buildx/compose plugin packages;
    # retry with the core set so the engine still gets installed.
    log "full package set unavailable, retrying with the core packages"
    apt-get install -y -o Dpkg::Options::=--force-confold \
        docker-ce docker-ce-cli containerd.io && return 0
    # Leave no half-configured source behind: a broken docker.list would make
    # every later apt-get update on this host fail too.
    rm -f /etc/apt/sources.list.d/docker.list
    return 1
}

# install_rpm MIRROR DISTRO — docker-ce from that mirror's yum/dnf repository.
install_rpm() {
    mirror="$1"; distro="$2"
    log "configuring yum repository: ${mirror}/linux/${distro}"
    if ! fetch "${mirror}/linux/${distro}/docker-ce.repo" /etc/yum.repos.d/docker-ce.repo; then
        log "failed to fetch the repo file from ${mirror}"
        return 1
    fi
    if command -v dnf >/dev/null 2>&1; then
        dnf install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin && return 0
    else
        yum install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin && return 0
    fi
    log "full package set unavailable, retrying with the core packages"
    if command -v dnf >/dev/null 2>&1; then
        dnf install -y docker-ce docker-ce-cli containerd.io && return 0
    else
        yum install -y docker-ce docker-ce-cli containerd.io && return 0
    fi
    rm -f /etc/yum.repos.d/docker-ce.repo
    return 1
}

install_from_mirrors() {
    if [ ! -r /etc/os-release ]; then
        log "cannot read /etc/os-release, skipping the mirror path"
        return 1
    fi
    # shellcheck disable=SC1091
    . /etc/os-release
    if [ "${ID:-}" = "alpine" ]; then
        # Alpine has no docker-ce repository; the distro package set ships the
        # engine and the compose plugin.
        log "Alpine detected, installing from the distribution repository"
        apk add --no-cache docker docker-cli-compose || return 1
        rc-update add docker default 2>/dev/null || true
        return 0
    fi
    repo_distro="$(docker_repo_distro "${ID:-}")" || {
        log "unsupported distribution: ${PRETTY_NAME:-unknown}"
        return 1
    }
    case "$(uname -m)" in
        x86_64|amd64|aarch64|arm64) ;;
        *) log "unsupported architecture: $(uname -m)"; return 1 ;;
    esac

    # Mirror order: Aliyun first (best reachability in mainland China), then
    # Huawei Cloud, then Tsinghua TUNA. The first success wins.
    for mirror in \
        "https://mirrors.aliyun.com/docker-ce" \
        "https://mirrors.huaweicloud.com/docker-ce" \
        "https://mirrors.tuna.tsinghua.edu.cn/docker-ce"
    do
        log "trying docker-ce mirror: ${mirror}"
        case "$repo_distro" in
            ubuntu|debian)
                install_apt "$mirror" "$repo_distro" && { log "installed from ${mirror}"; return 0; }
                ;;
            *)
                install_rpm "$mirror" "$repo_distro" && { log "installed from ${mirror}"; return 0; }
                ;;
        esac
        log "mirror failed, trying the next one"
    done
    return 1
}

install_from_official() {
    log "trying the official convenience script: https://get.docker.com"
    if ! fetch https://get.docker.com /tmp/watchman-get-docker.sh; then
        log "cannot download https://get.docker.com (DNS blocked or unreachable)"
        return 1
    fi
    sh /tmp/watchman-get-docker.sh || return 1
    rm -f /tmp/watchman-get-docker.sh
    return 0
}

log "starting one-click Docker installation"
if ! install_from_mirrors; then
    log "all domestic mirrors were unusable, falling back to the official script"
    if ! install_from_official; then
        log "ERROR: Docker could not be installed."
        log "Neither the docker-ce mirrors nor get.docker.com were reachable from this host."
        log "Check the host DNS/proxy settings, or install Docker manually:"
        log "  https://docs.docker.com/engine/install/"
        exit 1
    fi
fi

start_docker

if ! command -v docker >/dev/null 2>&1; then
    log "ERROR: the installation step reported success but the docker command is missing"
    exit 1
fi

echo "Docker installation complete."
docker --version
`

// installDockerScript triggers the one-click Docker installation script.
func (h *Handlers) installDockerScript(c *gin.Context) {
	hostID := c.Param("id")
	hub := h.reg.Hub(hostID)
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent offline"})
		return
	}

	installScript := dockerInstallScript

	opID := "dk-inst-" + randomToken(4)
	resultCh := make(chan *agentpb.ExecResult, 1)
	hub.SetRespHandler(opID, func(msg *agentpb.AgentMessage) {
		hub.SetRespHandler(opID, nil)
		if msg != nil {
			resultCh <- msg.GetExecResult()
		} else {
			resultCh <- nil
		}
	})

	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_Exec{
			Exec: &agentpb.ExecRequest{
				ExecId:   opID,
				Command:  installScript,
				IsScript: true,
				// The script walks up to three mirrors plus the official
				// fallback before giving up, so a slow link can legitimately
				// take longer than the old single-source 300s budget.
				TimeoutSec: 600,
			},
		},
	})

	select {
	case res := <-resultCh:
		if res == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent disconnected"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"ok":        res.GetExitCode() == 0,
			"exit_code": res.GetExitCode(),
			"stdout":    string(res.GetStdout()),
			"stderr":    string(res.GetStderr()),
		})
	case <-c.Request.Context().Done():
		hub.SetRespHandler(opID, nil)
	}
}
