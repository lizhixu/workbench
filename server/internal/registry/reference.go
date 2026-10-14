// Package registry implements a pull-free Docker Registry V2 / OCI image
// metadata prober. It resolves an image reference, performs the anonymous
// Bearer token handshake when challenged, fetches the manifest (or OCI index)
// and reads the tiny config blob to obtain the image's true ExposedPorts,
// Env, Cmd, Volumes and platform without pulling any layer.
//
// The prober is the ground-truth source used to stop the AI app assistant
// from hallucinating container ports for niche images ( AGENTS.md 3.7 §创建页
// AI 辅助): its results are injected into the prompt as authoritative facts
// and enforced again in the server-side sanitize pass.
package registry

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// DefaultRegistry is the canonical Docker Hub host. The API is actually
// served by registry-1.docker.io; the index.docker.io alias is normalized to it.
const DefaultRegistry = "registry-1.docker.io"

const defaultTag = "latest"

// Reference is a parsed image reference split into registry, repository and tag.
type Reference struct {
	Registry   string // e.g. "registry-1.docker.io", "ghcr.io"
	Repository string // e.g. "library/nginx", "xerxes-2/clewdr"
	Tag        string // e.g. "latest", "alpine"
}

// String returns the normalized human-facing form (registry included for
// non-Docker-Hub registries, matching what docker pull would accept).
func (r Reference) String() string {
	if r.Registry == DefaultRegistry {
		repo := strings.TrimPrefix(r.Repository, "library/")
		return fmt.Sprintf("%s:%s", repo, r.Tag)
	}
	return fmt.Sprintf("%s/%s:%s", r.Registry, r.Repository, r.Tag)
}

var errInvalidRef = errors.New("invalid image reference")

// ParseReference splits an OCI image reference into registry/repository/tag.
// Rules mirror docker's familiar normalization:
//   - "nginx" / "nginx:alpine"            → docker.io library/nginx
//   - "johnsmith/nginx"                   → docker.io johnsmith/nginx
//   - "ghcr.io/owner/img[:tag]"           → ghcr.io
//   - "host[:port]/repo[:tag]"            → host is a registry iff the first
//     path segment contains "." or ":" or is "localhost"
//
// Digest references (@sha256:...) are not supported — only tag probing.
func ParseReference(raw string) (Reference, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Reference{}, errInvalidRef
	}
	if strings.Contains(raw, "@") {
		return Reference{}, fmt.Errorf("digest 引用暂不支持探测，请改用 tag 形态（如 repo:tag）")
	}
	if strings.Contains(raw, "://") {
		return Reference{}, fmt.Errorf("镜像引用不应包含 URL 协议头")
	}
	// Whitespace/control characters are always invalid.
	if strings.ContainsAny(raw, " \t\r\n") {
		return Reference{}, errInvalidRef
	}

	// Split tag off the last path segment.
	name := raw
	tag := defaultTag
	if i := strings.LastIndexByte(raw, ':'); i >= 0 {
		if last := raw[i+1:]; last != "" && !strings.ContainsRune(last, '/') {
			name = raw[:i]
			tag = last
		}
	}
	if name == "" {
		return Reference{}, errInvalidRef
	}
	if strings.ToLower(name) != name {
		// The repository part must be lowercase (Docker enforces this).
		return Reference{}, fmt.Errorf("仓库名必须为小写: %q", name)
	}

	registry := DefaultRegistry
	repo := name
	if i := strings.IndexByte(name, '/'); i >= 0 {
		first := name[:i]
		if first == "docker.io" || first == "index.docker.io" {
			registry = DefaultRegistry
			repo = name[i+1:]
		} else if first == "localhost" || strings.ContainsAny(first, ".:") {
			registry = first
			repo = name[i+1:]
		}
	}
	if repo == "" || strings.HasSuffix(repo, "/") {
		return Reference{}, errInvalidRef
	}

	if registry == DefaultRegistry && !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	if err := guardRegistryHost(registry); err != nil {
		return Reference{}, err
	}
	return Reference{Registry: registry, Repository: repo, Tag: tag}, nil
}

// guardRegistryHost rejects registries that resolve to loopback/RFC1918
// addresses so the prober cannot be abused as an SSRF trampoline against the
// control plane's internal network. Custom local registries are reachable by
// the agent-side docker CLI at deploy time anyway — probing is an AI assist
// feature targeting public registries.
func guardRegistryHost(hostport string) error {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return fmt.Errorf("拒绝探测内网/本机仓库地址: %s", hostport)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return fmt.Errorf("拒绝探测内网/本机仓库地址: %s", hostport)
		}
	}
	return nil
}

// LooksLikeImageRef is a conservative heuristic used to spot a container image
// reference inside free-form natural-language requirements. It deliberately
// requires a slash+dotted-registry or an explicit :tag on a slash-name so that
// plain words and times ("12:30") are never mistaken for images.
func LooksLikeImageRef(token string) bool {
	token = strings.Trim(token, " \t\"'`,;()[]，。；：")
	if token == "" {
		return false
	}
	// A bare "word" or "word:tag" is too ambiguous in Chinese/English prose;
	// require either a registry prefix segment (contains . or :) or a
	// slash-separated multi-segment path.
	slash := strings.IndexByte(token, '/')
	if slash < 0 {
		return false
	}
	first := token[:slash]
	if !(strings.ContainsAny(first, ".:") || first == "localhost") {
		// e.g. "johnsmith/nginx" — accept only with an explicit tag.
		return strings.Contains(token[slash+1:], ":")
	}
	_, err := ParseReference(token)
	return err == nil
}
