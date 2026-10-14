package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// Long enough to fetch a config blob through one redirect across the CDN on
	// slow international networks (ghcr.io blobs redirect to blob.core.windows.net),
	// but short enough that an unresponsive registry fails fast.
	defaultTimeout = 12 * time.Second

	// MaxConfigSize bounds the image config blob we are willing to read
	// (configs are normally a few KB).
	maxConfigSize = 1 << 20
	// MaxManifestSize bounds a manifest / index document.
	maxManifestSize = 1 << 20
)

// Accept header covering Docker manifest v2, manifest list and both OCI forms.
const manifestAccept = "application/vnd.docker.distribution.manifest.v2+json" +
	", application/vnd.docker.distribution.manifest.list.v2+json" +
	", application/vnd.oci.image.manifest.v1+json" +
	", application/vnd.oci.image.index.v1+json"

// indexMediaTypes identify manifest lists / OCI indexes which need a second hop.
var indexMediaTypes = map[string]bool{
	"application/vnd.docker.distribution.manifest.list.v2+json": true,
	"application/vnd.oci.image.index.v1+json":                   true,
}

// Client talks to Docker Registry V2 endpoints for read-only metadata probing.
// Zero value is usable; Scheme defaults to https.
type Client struct {
	// Scheme can be overridden to "http" by tests (httptest servers).
	Scheme string
	// InsecureSkipVerify disables TLS verification for self-hosted registries
	// with self-signed certificates. Off by default.
	InsecureSkipVerify bool
	// Timeout bounds a single registry round-trip.
	Timeout time.Duration

	http *http.Client
}

// NewClient returns a Client with production defaults.
func NewClient() *Client { return &Client{} }

func (c *Client) httpClient() *http.Client {
	if c.http != nil {
		return c.http
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if c.InsecureSkipVerify {
		tr.TLSClientConfig = insecureTLSConfig()
	}
	c.http = &http.Client{Timeout: timeout, Transport: tr}
	return c.http
}

func (c *Client) baseURL(registry string) string {
	scheme := c.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s", scheme, registry)
}

// dockerHubMirrors are public Docker Hub pull-through mirrors used as a
// best-effort fallback when the upstream api host is unreachable (common on
// CN networks). Read-only metadata probing only.
var dockerHubMirrors = []string{
	"docker.m.daocloud.io",
}

// authChallenge captures a Bearer challenge from Www-Authenticate.
type authChallenge struct {
	Realm   string
	Service string
	Scope   string
}

func parseBearerChallenge(v string) *authChallenge {
	if v == "" {
		return nil
	}
	kt, rest, ok := strings.Cut(v, " ")
	if !ok || !strings.EqualFold(strings.TrimSpace(kt), "bearer") {
		return nil
	}
	ch := &authChallenge{}
	for _, part := range strings.Split(rest, ",") {
		k, val, hasEq := strings.Cut(strings.TrimSpace(part), "=")
		if !hasEq {
			continue
		}
		val = strings.Trim(val, `"`)
		switch strings.ToLower(k) {
		case "realm":
			ch.Realm = val
		case "service":
			ch.Service = val
		case "scope":
			ch.Scope = val
		}
	}
	if ch.Realm == "" {
		return nil
	}
	return ch
}

// fetchToken performs the anonymous Realm token exchange for pull scope.
func (c *Client) fetchToken(ctx context.Context, ch *authChallenge) (string, error) {
	u, err := url.Parse(ch.Realm)
	if err != nil {
		return "", fmt.Errorf("token realm 非法: %w", err)
	}
	q := u.Query()
	if ch.Service != "" {
		q.Set("service", ch.Service)
	}
	if ch.Scope != "" {
		q.Set("scope", ch.Scope)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("token 交换失败 (%s)", resp.Status)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("token 响应解析失败: %w", err)
	}
	tok := body.Token
	if tok == "" {
		tok = body.AccessToken
	}
	if tok == "" {
		return "", fmt.Errorf("token 响应为空")
	}
	return tok, nil
}

// getJSON performs an authorized GET that transparently answers Bearer
// challenges once. out receives the decoded JSON body.
func (c *Client) getJSON(ctx context.Context, fullURL string, accept, token string, out any) (string, error) {
	do := func(tok string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		req.Header.Set("User-Agent", "watchman-registry-probe")
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		return c.httpClient().Do(req)
	}

	resp, err := do(token)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	newToken := token
	if resp.StatusCode == http.StatusUnauthorized {
		ch := parseBearerChallenge(resp.Header.Get("Www-Authenticate"))
		if ch == nil || !strings.HasPrefix(ch.Realm, "http") {
			return "", fmt.Errorf("镜像仓库要求认证但未提供可处理的 Bearer 挑战 (%s)", resp.Status)
		}
		tok, err := c.fetchToken(ctx, ch)
		if err != nil {
			return "", err
		}
		newToken = tok
		resp.Body.Close()
		resp, err = do(newToken)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
	}

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("镜像或 tag 不存在 (%s)", resp.Status)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("镜像仓库拒绝匿名读取（可能为私有镜像）(%s)", resp.Status)
	}
	if resp.StatusCode/100 != 2 {
		body := make([]byte, 256)
		n, _ := resp.Body.Read(body)
		return "", fmt.Errorf("仓库响应异常 (%s): %s", resp.Status, strings.TrimSpace(string(body[:n])))
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, maxManifestSize)).Decode(out); err != nil {
		return "", fmt.Errorf("清单解析失败: %w", err)
	}
	return newToken, nil
}
