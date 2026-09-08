package gitprovider

import "time"

// Account represents an authorized GitHub connection.
type Account struct {
	Login     string    `json:"login"`
	Name      string    `json:"name"`
	AvatarURL string    `json:"avatar_url"`
	HTMLURL   string    `json:"html_url"`
	Token     string    `json:"token,omitempty"`
	AuthType  string    `json:"auth_type"` // "token" | "oauth"
	UpdatedAt time.Time `json:"updated_at"`
}

// Redacted returns a safe public view of the account with token removed.
func (a *Account) Redacted() *Account {
	if a == nil {
		return nil
	}
	cp := *a
	cp.Token = ""
	return &cp
}

// OAuthConfig holds optional GitHub OAuth App client credentials.
type OAuthConfig struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// RepositoryDTO represents a GitHub repository exposed to the UI selector.
type RepositoryDTO struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`           // e.g. "my-repo"
	FullName      string    `json:"full_name"`      // e.g. "octocat/my-repo"
	Private       bool      `json:"private"`
	HTMLURL       string    `json:"html_url"`
	CloneURL      string    `json:"clone_url"`
	Description   string    `json:"description"`
	DefaultBranch string    `json:"default_branch"`
	PushedAt      time.Time `json:"pushed_at"`
	Language      string    `json:"language"`
	Stargazers    int       `json:"stargazers_count"`
}

// BranchDTO represents a branch in a selected repository.
type BranchDTO struct {
	Name      string `json:"name"`
	Protected bool   `json:"protected"`
	SHA       string `json:"sha"`
}
