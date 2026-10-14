package registry

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

func insecureTLSConfig() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit opt-in for self-hosted registries
}

// ImageMetadata is the ground truth extracted from an image's config blob.
type ImageMetadata struct {
	Registry     string            `json:"registry"`
	Repository   string            `json:"repository"`
	Tag          string            `json:"tag"`
	Reference    string            `json:"reference"`
	OS           string            `json:"os"`
	Architecture string            `json:"architecture"`
	ExposedPorts []int             `json:"exposed_ports"`
	EnvVars      map[string]string `json:"env_vars"`
	Volumes      []string          `json:"volumes"`
	Cmd          []string          `json:"cmd"`
	Entrypoint   []string          `json:"entrypoint"`
	WorkingDir   string            `json:"working_dir"`
}

// systemEnvKeys are base-image bookkeeping variables that never matter to the
// operator and are filtered out of the reported EnvVars.
var systemEnvKeys = map[string]bool{
	"PATH":          true,
	"SSL_CERT_FILE": true,
	"LANG":          true,
	"LANGUAGE":      true,
	"LC_ALL":        true,
	"NODE_VERSION":  true,
	"GOLANG_VERSION": true,
}

type manifestDoc struct {
	SchemaVersion int    `json:"schemaVersion"`
	MediaType     string `json:"mediaType"`
	Config        struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
	} `json:"config"`
	Manifests []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Platform  struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
			Variant      string `json:"variant"`
		} `json:"platform"`
	} `json:"manifests"`
}

type imageConfig struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Config       struct {
		ExposedPorts map[string]json.RawMessage `json:"ExposedPorts"`
		Env          []string                     `json:"Env"`
		Volumes      map[string]json.RawMessage `json:"Volumes"`
		Cmd          []string                     `json:"Cmd"`
		Entrypoint   []string                     `json:"Entrypoint"`
		WorkingDir   string                       `json:"WorkingDir"`
	} `json:"config"`
}

// Inspect probes a registry for the image's true runtime metadata without
// pulling layers: manifest → config blob. Docker Hub references fall back to
// configured pull-through mirrors when the primary host is unreachable.
//
// Errors are descriptive Chinese strings suitable for surfacing in the UI;
// callers should treat any error as "probe unavailable" and keep working
// without the ground truth.
func Inspect(ctx context.Context, imageRef string) (*ImageMetadata, error) {
	return NewClient().Inspect(ctx, imageRef)
}

// Inspect is the Client method backing the package-level Inspect.
func (c *Client) Inspect(ctx context.Context, imageRef string) (*ImageMetadata, error) {
	ref, err := ParseReference(imageRef)
	if err != nil {
		return nil, err
	}
	registries := []string{ref.Registry}
	if ref.Registry == DefaultRegistry {
		registries = append(registries, dockerHubMirrors...)
	}
	var lastErr error
	for i, reg := range registries {
		meta, err := c.inspectAgainst(ctx, ref, reg)
		if err == nil {
			return meta, nil
		}
		lastErr = err
		// Only mirror on transport/timeout failures: a definitive registry
		// answer (not found / private) is already final.
		if !isUnreachable(err) {
			break
		}
		if i == 0 {
			_ = reg // primary unreachable; fall through to mirrors
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("连接仓库失败: %w", lastErr)
	}
	return nil, fmt.Errorf("无法探测镜像")
}

// isUnreachable reports whether err denotes a registry that could not be
// reached at all (DNS / dial / timeout), as opposed to an HTTP-level reply.
func isUnreachable(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return strings.Contains(err.Error(), "dial tcp") ||
		strings.Contains(err.Error(), "no such host") ||
		strings.Contains(err.Error(), "context deadline exceeded") ||
		strings.Contains(err.Error(), "connection refused") ||
		strings.Contains(err.Error(), "TLS handshake timeout") ||
		strings.Contains(err.Error(), "EOF")
}

// inspectAgainst runs the probe against one concrete registry host. For the
// Docker Hub fallback flow the reference's repository is reused verbatim
// (mirrors serve the same docker.io path space including library/ prefix).
func (c *Client) inspectAgainst(ctx context.Context, ref Reference, registry string) (*ImageMetadata, error) {
	base := c.baseURL(registry)
	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", base, ref.Repository, ref.Tag)

	var doc manifestDoc
	token, err := c.getJSON(ctx, manifestURL, manifestAccept, "", &doc)
	if err != nil {
		return nil, err
	}

	// Resolve an index/list to a concrete platform manifest, preferring the
	// most common architecture running behind the control plane.
	if doc.Config.Digest == "" && (len(doc.Manifests) > 0 || indexMediaTypes[doc.MediaType]) {
		choice := pickManifest(doc)
		if choice == "" {
			return nil, fmt.Errorf("镜像清单中没有可用的平台入口")
		}
		subURL := fmt.Sprintf("%s/v2/%s/manifests/%s", base, ref.Repository, choice)
		var sub manifestDoc
		if _, err = c.getJSON(ctx, subURL, manifestAccept, token, &sub); err != nil {
			return nil, err
		}
		doc = sub
	}
	if doc.Config.Digest == "" {
		return nil, fmt.Errorf("清单缺少 config 引用（可能是 OCI artifact 清单）")
	}

	cfg, err := c.fetchConfig(ctx, base, ref.Repository, doc.Config.Digest, token)
	if err != nil {
		return nil, err
	}

	meta := &ImageMetadata{
		Registry:     ref.Registry,
		Repository:   ref.Repository,
		Tag:          ref.Tag,
		Reference:    ref.String(),
		OS:           cfg.OS,
		Architecture: cfg.Architecture,
		EnvVars:      map[string]string{},
		Cmd:          cfg.Config.Cmd,
		Entrypoint:   cfg.Config.Entrypoint,
		WorkingDir:   cfg.Config.WorkingDir,
	}
	for p := range cfg.Config.ExposedPorts {
		portStr, proto, _ := strings.Cut(p, "/")
		if proto != "" && !strings.EqualFold(proto, "tcp") {
			continue // udp/sctp ports are not web-relevant
		}
		if n, cerr := strconv.Atoi(portStr); cerr == nil && n > 0 && n <= 65535 {
			meta.ExposedPorts = append(meta.ExposedPorts, n)
		}
	}
	sort.Ints(meta.ExposedPorts)
	for v := range cfg.Config.Volumes {
		meta.Volumes = append(meta.Volumes, v)
	}
	sort.Strings(meta.Volumes)
	for _, kv := range cfg.Config.Env {
		k, v2, ok := strings.Cut(kv, "=")
		if !ok || k == "" || systemEnvKeys[k] {
			continue
		}
		if len(v2) > 4096 {
			v2 = v2[:4096]
		}
		if len(meta.EnvVars) >= 64 {
			break
		}
		meta.EnvVars[k] = v2
	}
	if len(meta.EnvVars) == 0 {
		meta.EnvVars = nil
	}
	return meta, nil
}

// pickManifest chooses the amd64/linux entry when present, else the first.
func pickManifest(doc manifestDoc) string {
	var attestation string
	for _, m := range doc.Manifests {
		if m.Platform.Architecture == "unknown" {
			attestation = m.Digest // OCI attestation sidecar — never select
			continue
		}
		if m.Platform.OS == "linux" && (m.Platform.Architecture == "amd64" || m.Platform.Architecture == "") {
			return m.Digest
		}
	}
	for _, m := range doc.Manifests {
		if m.Digest != attestation {
			return m.Digest
		}
	}
	return ""
}

// fetchConfig reads the config blob JSON, following registry redirects handled
// by net/http (blob CDNs normally answer 307 to a signed URL).
func (c *Client) fetchConfig(ctx context.Context, base, repo, digest, token string) (*imageConfig, error) {
	blobURL := fmt.Sprintf("%s/v2/%s/blobs/%s", base, repo, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, blobURL, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("读取 config blob 失败 (%s)", resp.Status)
	}
	var cfg imageConfig
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxConfigSize)).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config blob 解析失败: %w", err)
	}
	return &cfg, nil
}
