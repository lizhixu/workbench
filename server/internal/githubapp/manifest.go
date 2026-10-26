package githubapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// BuildManifest renders the GitHub App manifest for the create flow. The
// manifest is submitted by the browser (form POST) to GitHub, which creates
// the App and redirects back with a one-hour code for the exchange step.
// Permissions stay minimal for v1: read repository contents and metadata,
// subscribe to push events only. Pull-request/checks permissions (needed for
// PR preview deployments) are deliberately left out of v1.
//
// The CSRF state is embedded in redirect_url's query string, NOT in the
// submit URL: GitHub does not promise to carry arbitrary query parameters
// from the manifest submit URL through to the redirect, but it does append
// the code to redirect_url preserving its existing query string. The panel
// callback reads github_app_state back and matches it against the state
// issued with the manifest.
func BuildManifest(panelURL, appName, state string) map[string]any {
	redirect := strings.TrimRight(panelURL, "/") + "/settings?s=git"
	if state != "" {
		redirect += "&github_app_state=" + state
	}
	return map[string]any{
		"name": appName,
		"url":  panelURL,
		"hook_attributes": map[string]any{
			"url":    strings.TrimRight(panelURL, "/") + "/api/v1/github-app/webhook",
			"active": true,
		},
		"redirect_url": redirect,
		"public":       true,
		"default_permissions": map[string]string{
			"contents": "read",
			"metadata": "read",
		},
		"default_events": []string{"push"},
	}
}

// ManifestSubmitURL returns the GitHub page the manifest form posts to.
// Personal account apps and organization apps have different paths.
func ManifestSubmitURL(target, org string) string {
	if target == "organization" && org != "" {
		return "https://github.com/organizations/" + org + "/settings/apps/new"
	}
	return "https://github.com/settings/apps/new"
}

// VerifySignature checks the X-Hub-Signature-256 header against the raw
// request body using the App's webhook secret. GitHub signs every delivery
// with this secret; a mismatch means the request did not come from GitHub.
func VerifySignature(secret string, body []byte, sigHeader string) bool {
	if secret == "" || sigHeader == "" {
		return false
	}
	const prefix = "sha256="
	if !strings.HasPrefix(sigHeader, prefix) {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(sigHeader, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

// PushEvent is the subset of a GitHub push delivery the panel acts on.
type PushEvent struct {
	Ref            string
	RepoFullName   string
	InstallationID int64
}

// Branch returns the pushed branch name ("refs/heads/main" -> "main").
func (e *PushEvent) Branch() string {
	return branchFromRef(e.Ref)
}

func branchFromRef(ref string) string {
	ref = strings.TrimSpace(ref)
	for _, prefix := range []string{"refs/heads/", "refs/tags/", "refs/"} {
		if strings.HasPrefix(ref, prefix) {
			return strings.TrimPrefix(ref, prefix)
		}
	}
	return ref
}

// ParsePushEvent extracts the push routing fields from a delivery body.
func ParsePushEvent(body []byte) (*PushEvent, error) {
	var payload struct {
		Ref        string `json:"ref"`
		Repository *struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Installation *struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if payload.Repository == nil || payload.Repository.FullName == "" {
		return nil, fmt.Errorf("push 事件缺少仓库信息")
	}
	ev := &PushEvent{Ref: payload.Ref, RepoFullName: strings.ToLower(payload.Repository.FullName)}
	if payload.Installation != nil {
		ev.InstallationID = payload.Installation.ID
	}
	return ev, nil
}

// NormalizeRepoURL reduces any GitHub remote form to a lowercase
// "owner/repo" name: https/ssh/git URLs, with or without a .git suffix,
// or a bare "owner/repo" string. Anything else yields "".
func NormalizeRepoURL(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	if s == "" {
		return ""
	}
	if idx := strings.Index(s, "github.com"); idx >= 0 {
		rest := s[idx+len("github.com"):]
		rest = strings.TrimLeft(rest, "/:")
		return strings.ToLower(rest)
	}
	if strings.Count(s, "/") == 1 && !strings.ContainsAny(s, " :@") {
		return strings.ToLower(s)
	}
	return ""
}
