package gitprovider

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

const githubAPIBase = "https://api.github.com"

// Client interacts with GitHub's REST API using the stored account token.
type Client struct {
	http *http.Client
}

// NewClient creates a GitHub API client.
func NewClient() *Client {
	return &Client{
		http: &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *Client) get(ctx context.Context, token, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPIBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GitHub API 响应异常 (%d): %s", resp.StatusCode, string(body))
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

// VerifyToken calls /user to validate the token and fetch account identity.
func (c *Client) VerifyToken(ctx context.Context, token string) (*Account, error) {
	var ghUser struct {
		Login     string `json:"login"`
		Name      string `json:"name"`
		AvatarURL string `json:"avatar_url"`
		HTMLURL   string `json:"html_url"`
	}
	if err := c.get(ctx, token, "/user", &ghUser); err != nil {
		return nil, err
	}
	name := ghUser.Name
	if name == "" {
		name = ghUser.Login
	}
	return &Account{
		Login:     ghUser.Login,
		Name:      name,
		AvatarURL: ghUser.AvatarURL,
		HTMLURL:   ghUser.HTMLURL,
		Token:     token,
		AuthType:  "token",
		UpdatedAt: time.Now(),
	}, nil
}

// ListRepositories returns user-accessible repositories (owned, collabs, orgs),
// sorted by last pushed, with optional search query filter.
func (c *Client) ListRepositories(ctx context.Context, token, query string) ([]RepositoryDTO, error) {
	// Request up to 100 repositories, sorted by updated
	path := "/user/repos?sort=pushed&per_page=100&type=all"
	var repos []struct {
		ID            int64     `json:"id"`
		Name          string    `json:"name"`
		FullName      string    `json:"full_name"`
		Private       bool      `json:"private"`
		HTMLURL       string    `json:"html_url"`
		CloneURL      string    `json:"clone_url"`
		Description   string    `json:"description"`
		DefaultBranch string    `json:"default_branch"`
		PushedAt      time.Time `json:"pushed_at"`
		Language      string    `json:"language"`
		Stargazers    int       `json:"stargazers_count"`
	}
	if err := c.get(ctx, token, path, &repos); err != nil {
		return nil, err
	}

	q := strings.TrimSpace(strings.ToLower(query))
	out := make([]RepositoryDTO, 0, len(repos))
	for _, r := range repos {
		if q != "" {
			if !strings.Contains(strings.ToLower(r.FullName), q) &&
				!strings.Contains(strings.ToLower(r.Description), q) {
				continue
			}
		}
		defBranch := r.DefaultBranch
		if defBranch == "" {
			defBranch = "main"
		}
		out = append(out, RepositoryDTO{
			ID:            r.ID,
			Name:          r.Name,
			FullName:      r.FullName,
			Private:       r.Private,
			HTMLURL:       r.HTMLURL,
			CloneURL:      r.CloneURL,
			Description:   r.Description,
			DefaultBranch: defBranch,
			PushedAt:      r.PushedAt,
			Language:      r.Language,
			Stargazers:    r.Stargazers,
		})
	}
	return out, nil
}

// ListBranches returns branches for a given repository.
func (c *Client) ListBranches(ctx context.Context, token, owner, repo string) ([]BranchDTO, error) {
	path := fmt.Sprintf("/repos/%s/%s/branches?per_page=100", url.PathEscape(owner), url.PathEscape(repo))
	var branches []struct {
		Name      string `json:"name"`
		Protected bool   `json:"protected"`
		Commit    struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := c.get(ctx, token, path, &branches); err != nil {
		return nil, err
	}

	out := make([]BranchDTO, 0, len(branches))
	for _, b := range branches {
		out = append(out, BranchDTO{
			Name:      b.Name,
			Protected: b.Protected,
			SHA:       b.Commit.SHA,
		})
	}
	return out, nil
}

func (c *Client) post(ctx context.Context, token, path string, in, out any) error {
	var bodyReader io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		bodyReader = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, githubAPIBase+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "watchman-control-server")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("GitHub 凭据无效或已过期 (401 Unauthorized)")
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GitHub 权限不足或仓库不存在 (%d): %s", resp.StatusCode, string(body))
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GitHub API 响应异常 (%d): %s", resp.StatusCode, string(body))
	}

	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) delete(ctx context.Context, token, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, githubAPIBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "watchman-control-server")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("GitHub 凭据无效或已过期 (401 Unauthorized)")
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("GitHub API 删除失败 (%d): %s", resp.StatusCode, string(body))
}

type gitHubHook struct {
	ID     int64 `json:"id"`
	Config struct {
		URL string `json:"url"`
	} `json:"config"`
}

// EnsureRepoWebhook registers a push webhook on the target GitHub repository if
// not already present. It returns the existing or created hook ID.
func (c *Client) EnsureRepoWebhook(ctx context.Context, token, owner, repo, webhookURL string) (int64, error) {
	ownerEnc := url.PathEscape(owner)
	repoEnc := url.PathEscape(repo)
	listPath := fmt.Sprintf("/repos/%s/%s/hooks?per_page=100", ownerEnc, repoEnc)

	var existing []gitHubHook
	if err := c.get(ctx, token, listPath, &existing); err == nil {
		for _, h := range existing {
			if strings.EqualFold(strings.TrimRight(h.Config.URL, "/"), strings.TrimRight(webhookURL, "/")) {
				return h.ID, nil
			}
		}
	}

	createPath := fmt.Sprintf("/repos/%s/%s/hooks", ownerEnc, repoEnc)
	payload := map[string]any{
		"name":   "web",
		"active": true,
		"events": []string{"push"},
		"config": map[string]string{
			"url":          webhookURL,
			"content_type": "json",
			"insecure_ssl": "0",
		},
	}

	var created gitHubHook
	if err := c.post(ctx, token, createPath, payload, &created); err != nil {
		// If it failed because it already exists, retry list
		if strings.Contains(err.Error(), "already exists") {
			var retryList []gitHubHook
			if err2 := c.get(ctx, token, listPath, &retryList); err2 == nil {
				for _, h := range retryList {
					if strings.EqualFold(strings.TrimRight(h.Config.URL, "/"), strings.TrimRight(webhookURL, "/")) {
						return h.ID, nil
					}
				}
			}
		}
		return 0, err
	}
	return created.ID, nil
}

// DeleteRepoWebhook removes a push webhook from the target GitHub repository.
func (c *Client) DeleteRepoWebhook(ctx context.Context, token, owner, repo string, hookID int64) error {
	if hookID <= 0 {
		return nil
	}
	deletePath := fmt.Sprintf("/repos/%s/%s/hooks/%d", url.PathEscape(owner), url.PathEscape(repo), hookID)
	return c.delete(ctx, token, deletePath)
}
