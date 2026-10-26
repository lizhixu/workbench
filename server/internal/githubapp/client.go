package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// githubAPIBase is overridable for tests; production code keeps the default.
var githubAPIBase = "https://api.github.com"

// Client talks to the GitHub REST API as a GitHub App (JWT) or as an
// installation (installation access token).
type Client struct {
	http *http.Client
}

func NewClient() *Client {
	return &Client{http: &http.Client{Timeout: 15 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path, auth string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, githubAPIBase+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "watchman-control-server")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("GitHub 凭据无效或已过期 (401 Unauthorized)")
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GitHub API 响应异常 (%d): %s", resp.StatusCode, string(b))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// MintJWT signs a GitHub App JWT (RS256, at most 10 minutes of validity).
// The JWT authenticates as the App itself: it can list installations and
// mint installation tokens, but cannot read repository contents.
func MintJWT(appID int64, privateKeyPEM string) (string, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return "", fmt.Errorf("私钥不是有效的 PEM 格式")
	}
	var priv *rsa.PrivateKey
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		priv = key
	} else if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		p, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return "", fmt.Errorf("私钥不是 RSA 私钥")
		}
		priv = p
	} else {
		return "", fmt.Errorf("无法解析私钥: %v", err)
	}
	now := time.Now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(appID, 10),
	})
	if err != nil {
		return "", err
	}
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// ManifestResult is what GitHub returns after exchanging a manifest code.
type ManifestResult struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	PEM           string `json:"pem"`
	WebhookSecret string `json:"webhook_secret"`
}

// ExchangeManifestCode completes the manifest flow. The code is only valid
// for one hour after the App is created on GitHub.
func (c *Client) ExchangeManifestCode(ctx context.Context, code string) (*ManifestResult, error) {
	var res ManifestResult
	path := "/app-manifests/" + url.PathEscape(code) + "/conversions"
	if err := c.do(ctx, http.MethodPost, path, "", nil, &res); err != nil {
		return nil, err
	}
	if res.ID <= 0 || res.PEM == "" {
		return nil, fmt.Errorf("GitHub 返回的 App 配置不完整")
	}
	return &res, nil
}

// InstallationInfo is one row of GET /app/installations.
type InstallationInfo struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		Type  string `json:"type"` // User | Organization
	} `json:"account"`
	TargetType  string `json:"target_type"` // all | selected
	SuspendedBy *struct {
		Login string `json:"login"`
	} `json:"suspended_by"` // null when active, object when suspended
}

// Suspended reports whether the installation is suspended.
func (i InstallationInfo) Suspended() bool { return i.SuspendedBy != nil }

// ListInstallations lists the accounts the App is installed on,
// following pagination up to 5 pages.
func (c *Client) ListInstallations(ctx context.Context, jwt string) ([]InstallationInfo, error) {
	var all []InstallationInfo
	for page := 1; page <= 5; page++ {
		var list []InstallationInfo
		path := fmt.Sprintf("/app/installations?per_page=100&page=%d", page)
		if err := c.do(ctx, http.MethodGet, path, jwt, nil, &list); err != nil {
			return nil, err
		}
		all = append(all, list...)
		if len(list) < 100 {
			break
		}
	}
	return all, nil
}

// InstallationToken is a short-lived (1 hour) token scoped to one
// installation. It is used for git fetch and repo listing, and is never
// persisted — only held in memory until shortly before expiry.
type InstallationToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// CreateInstallationToken mints an installation access token.
func (c *Client) CreateInstallationToken(ctx context.Context, jwt string, installationID int64) (*InstallationToken, error) {
	var tok InstallationToken
	path := fmt.Sprintf("/app/installations/%d/access_tokens", installationID)
	if err := c.do(ctx, http.MethodPost, path, jwt, map[string]any{}, &tok); err != nil {
		return nil, err
	}
	if tok.Token == "" {
		return nil, fmt.Errorf("GitHub 未返回 installation token")
	}
	return &tok, nil
}

type installationReposResp struct {
	Repositories []struct {
		FullName string `json:"full_name"`
	} `json:"repositories"`
}

// ListInstallationRepos returns the installation's repository full names
// ("owner/repo"), following pagination up to 5 pages.
func (c *Client) ListInstallationRepos(ctx context.Context, instToken string) ([]string, error) {
	var names []string
	for page := 1; page <= 5; page++ {
		var resp installationReposResp
		path := fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page)
		if err := c.do(ctx, http.MethodGet, path, instToken, nil, &resp); err != nil {
			return nil, err
		}
		for _, r := range resp.Repositories {
			names = append(names, strings.ToLower(r.FullName))
		}
		if len(resp.Repositories) < 100 {
			break
		}
	}
	return names, nil
}

// ListRepoBranches returns branch names for one repository, using an
// installation token, following pagination up to 5 pages.
func (c *Client) ListRepoBranches(ctx context.Context, instToken, owner, repo string) ([]string, error) {
	var names []string
	for page := 1; page <= 5; page++ {
		var branches []struct {
			Name string `json:"name"`
		}
		path := fmt.Sprintf("/repos/%s/%s/branches?per_page=100&page=%d", owner, repo, page)
		if err := c.do(ctx, http.MethodGet, path, instToken, nil, &branches); err != nil {
			return nil, err
		}
		for _, b := range branches {
			names = append(names, b.Name)
		}
		if len(branches) < 100 {
			break
		}
	}
	return names, nil
}
