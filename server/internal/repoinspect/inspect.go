package repoinspect

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// githubAPIBase and rawBase are overridable for tests; production keeps the defaults.
var (
	githubAPIBase = "https://api.github.com"
	rawBase       = "https://raw.githubusercontent.com"
)

// AppTokenResolver resolves a short-lived GitHub App installation token for a repository URL.
type AppTokenResolver interface {
	InstallationTokenForRepo(repoURL string) (string, bool)
}

// VaultSecretResolver looks up a user-managed secret from the credential vault.
type VaultSecretResolver func(vaultID string) (string, bool)

// InspectOptions scopes a repository metadata probe.
type InspectOptions struct {
	Branch       string
	BuildContext string
	AuthVaultID  string
}

// RepoMetadata is the ground truth read from a GitHub repository's checked-in
// deployment files (compose / Dockerfile / env example).
type RepoMetadata struct {
	RepoURL string `json:"repo_url"`
	Owner   string `json:"owner"`
	Name    string `json:"name"`
	Branch  string `json:"branch"`

	// Compose form (highest priority when present).
	HasCompose     bool              `json:"has_compose"`
	ComposeFile    string            `json:"compose_file,omitempty"`
	ComposeContent string            `json:"compose_content,omitempty"`
	ComposePorts   []ComposePort     `json:"compose_ports,omitempty"`
	ComposeEnv     map[string]string `json:"compose_env,omitempty"`

	// Dockerfile form.
	HasDockerfile  bool              `json:"has_dockerfile"`
	DockerfilePath string            `json:"dockerfile_path,omitempty"`
	ExposedPorts   []int             `json:"exposed_ports,omitempty"`
	DockerfileEnv  map[string]string `json:"dockerfile_env,omitempty"`
	Volumes        []string          `json:"volumes,omitempty"`

	// Auxiliary hints.
	EnvExample map[string]string `json:"env_example,omitempty"`
	Language   string            `json:"language,omitempty"`
}

// ComposePort is one host:container mapping inside a compose service.
type ComposePort struct {
	Service   string `json:"service"`
	Host      int    `json:"host"`
	Container int    `json:"container"`
}

const maxRepoFileSize = 64 << 10 // 64KB per probed file

var (
	composeCandidates = []string{
		"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml",
	}
	dockerfileCandidates = []string{
		"Dockerfile", "docker/Dockerfile", "deploy/Dockerfile", "build/Dockerfile",
	}
	envExampleCandidates = []string{
		".env.example", ".env.sample", ".env.template",
	}
	languageMarkers = []struct {
		path string
		lang string
	}{
		{"go.mod", "go"},
		{"package.json", "node"},
		{"requirements.txt", "python"},
		{"pyproject.toml", "python"},
		{"Cargo.toml", "rust"},
		{"pom.xml", "java"},
		{"build.gradle", "java"},
		{"pubspec.yaml", "dart"},
		{"composer.json", "php"},
		{"Gemfile", "ruby"},
	}
)

// Client probes GitHub repositories without cloning.
type Client struct {
	http *http.Client
}

// NewClient constructs a Client with a bounded HTTP timeout.
func NewClient() *Client {
	return &Client{http: &http.Client{Timeout: 15 * time.Second}}
}

// Service resolves credentials in canonical priority order
// (Vault credential > GitHub App installation token > anonymous public access)
// and delegates probing to Client.
type Service struct {
	client     *Client
	appTokens  AppTokenResolver
	vaultToken VaultSecretResolver
}

// NewService creates a repository inspection service. Either resolver may be nil.
func NewService(appTokens AppTokenResolver, vaultToken VaultSecretResolver) *Service {
	return &Service{
		client:     NewClient(),
		appTokens:  appTokens,
		vaultToken: vaultToken,
	}
}

// InspectRepo probes a GitHub repository using explicit branch / monorepo
// build_context when provided, falling back to default branch & root.
func (s *Service) InspectRepo(ctx context.Context, repoURL string, opts InspectOptions) (*RepoMetadata, error) {
	owner, repo, ok := ParseGitHubRepo(repoURL)
	if !ok {
		return nil, fmt.Errorf("目前仅支持探测 GitHub 仓库地址（https://github.com/<owner>/<repo>）")
	}
	token, err := s.resolveToken(repoURL, opts.AuthVaultID)
	if err != nil {
		return nil, err
	}
	ctxDir, err := NormalizeSubdir(opts.BuildContext)
	if err != nil {
		return nil, err
	}
	opts.BuildContext = ctxDir
	meta, err := s.client.InspectRepo(ctx, token, owner, repo, opts)
	if err != nil {
		return nil, err
	}
	meta.RepoURL = repoURL
	return meta, nil
}

func (s *Service) resolveToken(repoURL, authVaultID string) (string, error) {
	if vid := strings.TrimSpace(authVaultID); vid != "" && s.vaultToken != nil {
		secret, ok := s.vaultToken(vid)
		if !ok {
			return "", fmt.Errorf("凭据金库条目 %q 不存在", vid)
		}
		if secret != "" {
			return secret, nil
		}
	}
	if s.appTokens != nil {
		if tok, ok := s.appTokens.InstallationTokenForRepo(repoURL); ok && tok != "" {
			return tok, nil
		}
	}
	return "", nil
}

// ParseGitHubRepo extracts owner and repository name from GitHub HTTPS/SSH URLs
// or bare "owner/repo" syntax when explicitly github.com.
func ParseGitHubRepo(rawURL string) (owner, repo string, ok bool) {
	raw := strings.TrimSpace(rawURL)
	if strings.HasPrefix(raw, "git@github.com:") {
		path := strings.TrimPrefix(raw, "git@github.com:")
		path = strings.TrimSuffix(path, ".git")
		parts := strings.Split(path, "/")
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			return parts[0], parts[1], true
		}
		return "", "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", false
	}
	if !strings.Contains(strings.ToLower(u.Host), "github.com") {
		return "", "", false
	}
	path := strings.Trim(u.Path, "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
		return parts[0], parts[1], true
	}
	return "", "", false
}

// NormalizeSubdir validates and cleans a repo-relative subdirectory such as
// build_context. Returns "" for "." or empty; rejects backslashes, leading
// slashes (as absolute paths), and ".." traversal segments.
func NormalizeSubdir(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" || s == "." {
		return "", nil
	}
	if strings.Contains(s, "\\") {
		return "", fmt.Errorf("构建子目录不能包含反斜杠: %q", raw)
	}
	if strings.HasPrefix(s, "/") {
		return "", fmt.Errorf("构建子目录必须为仓库相对路径: %q", raw)
	}
	var kept []string
	for _, seg := range strings.FieldsFunc(s, func(r rune) bool { return r == '/' }) {
		if seg == "" || seg == "." {
			continue
		}
		if seg == ".." {
			return "", fmt.Errorf("构建子目录不能包含 ..: %q", raw)
		}
		kept = append(kept, seg)
	}
	return strings.Join(kept, "/"), nil
}

// InspectRepo reads a GitHub repository's deployment-relevant files without cloning.
func (c *Client) InspectRepo(ctx context.Context, token, owner, repo string, opts InspectOptions) (*RepoMetadata, error) {
	meta := &RepoMetadata{
		Owner: owner,
		Name:  repo,
	}

	var branches []string
	if b := strings.TrimSpace(opts.Branch); b != "" {
		branches = []string{b}
	} else {
		cand, branchErr := c.branchCandidates(ctx, token, owner, repo)
		if branchErr != nil {
			branches = []string{"main", "master"}
		} else {
			branches = cand
		}
	}

	prefix := opts.BuildContext
	scoped := func(base []string, fallBackRoot bool) []string {
		if prefix == "" {
			return base
		}
		var out []string
		for _, p := range base {
			out = append(out, prefix+"/"+p)
		}
		if fallBackRoot {
			out = append(out, base...)
		}
		return out
	}

	fetchFirst := func(candidates []string) (string, string, string) {
		for _, path := range candidates {
			for _, br := range branches {
				content, err := c.fetchRaw(ctx, token, owner, repo, br, path)
				if err == nil {
					return path, content, br
				}
			}
		}
		return "", "", ""
	}

	if path, content, br := fetchFirst(scoped(composeCandidates, false)); path != "" {
		meta.HasCompose = true
		meta.ComposeFile = path
		meta.Branch = br
		if len(content) > maxRepoFileSize {
			content = content[:maxRepoFileSize]
		}
		meta.ComposeContent = content
		meta.ComposePorts, meta.ComposeEnv = parseCompose(content)
	}

	if path, content, br := fetchFirst(scoped(dockerfileCandidates, false)); path != "" {
		meta.HasDockerfile = true
		meta.DockerfilePath = path
		if meta.Branch == "" {
			meta.Branch = br
		}
		meta.ExposedPorts, meta.DockerfileEnv, meta.Volumes = parseDockerfile(content)
	}

	if _, content, _ := fetchFirst(scoped(envExampleCandidates, true)); content != "" {
		meta.EnvExample = parseEnvFile(content)
		if len(meta.EnvExample) == 0 {
			meta.EnvExample = nil
		}
	}

	for _, m := range languageMarkers {
		if _, content, _ := fetchFirst(scoped([]string{m.path}, true)); content != "" {
			meta.Language = m.lang
			break
		}
	}
	if meta.Branch == "" && len(branches) > 0 {
		meta.Branch = branches[0]
	}
	return meta, nil
}

func (c *Client) get(ctx context.Context, token, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPIBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "watchman-control-server")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GitHub API 响应异常 (%d): %s", resp.StatusCode, string(body))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) branchCandidates(ctx context.Context, token, owner, repo string) ([]string, error) {
	var info struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.get(ctx, token, fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo)), &info); err != nil {
		return nil, err
	}
	var out []string
	if info.DefaultBranch != "" {
		out = append(out, info.DefaultBranch)
	}
	for _, fb := range []string{"main", "master"} {
		dup := false
		for _, e := range out {
			if e == fb {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, fb)
		}
	}
	return out, nil
}

func (c *Client) fetchRaw(ctx context.Context, token, owner, repo, branch, filePath string) (string, error) {
	var fullURL string
	if token != "" {
		fullURL = fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s",
			githubAPIBase, url.PathEscape(owner), url.PathEscape(repo), escapePathSegments(filePath), url.QueryEscape(branch))
	} else {
		fullURL = fmt.Sprintf("%s/%s/%s/%s/%s",
			rawBase, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(branch), escapePathSegments(filePath))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("Accept", "application/vnd.github.raw+json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	req.Header.Set("User-Agent", "watchman-control-server")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRepoFileSize+1))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func escapePathSegments(p string) string {
	parts := strings.Split(strings.TrimLeft(p, "/"), "/")
	for i, seg := range parts {
		parts[i] = url.PathEscape(seg)
	}
	return strings.Join(parts, "/")
}

// ---------------------------------------------------------------------------
// Dockerfile / Compose / .env parsing
// ---------------------------------------------------------------------------

func parseDockerfile(content string) (exposed []int, env map[string]string, volumes []string) {
	env = map[string]string{}
	portSeen := map[int]bool{}
	volSeen := map[string]bool{}

	for _, line := range logicalLines(content) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ins, rest, _ := strings.Cut(line, " ")
		rest = strings.TrimSpace(rest)
		switch strings.ToUpper(ins) {
		case "EXPOSE":
			for _, f := range strings.Fields(rest) {
				p, proto, _ := strings.Cut(f, "/")
				if proto != "" && !strings.EqualFold(proto, "tcp") {
					continue
				}
				lo, hi, ok := strings.Cut(p, "-")
				if !ok {
					if n, err := strconv.Atoi(lo); err == nil && n >= 1 && n <= 65535 && !portSeen[n] {
						portSeen[n] = true
						exposed = append(exposed, n)
					}
					continue
				}
				a, errA := strconv.Atoi(lo)
				b, errB := strconv.Atoi(hi)
				if errA == nil && errB == nil && a >= 1 && b <= 65535 && b >= a && b-a <= 8 {
					for n := a; n <= b; n++ {
						if !portSeen[n] {
							portSeen[n] = true
							exposed = append(exposed, n)
						}
					}
				}
			}
		case "ENV", "ARG":
			pairs := parseEnvPairs(rest)
			if len(pairs) == 0 {
				if k, v, ok := strings.Cut(rest, " "); ok && strings.TrimSpace(k) != "" {
					env[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `'"`)
				}
				continue
			}
			for k, v := range pairs {
				env[k] = v
			}
		case "VOLUME":
			if strings.HasPrefix(rest, "[") {
				var arr []string
				if err := yaml.Unmarshal([]byte(rest), &arr); err == nil {
					for _, v := range arr {
						v = strings.TrimSpace(v)
						if v != "" && !volSeen[v] {
							volSeen[v] = true
							volumes = append(volumes, v)
						}
					}
					continue
				}
			}
			for _, f := range strings.Fields(rest) {
				if !volSeen[f] {
					volSeen[f] = true
					volumes = append(volumes, f)
				}
			}
		}
	}
	sort.Ints(exposed)
	sort.Strings(volumes)
	if len(env) == 0 {
		env = nil
	}
	return exposed, env, volumes
}

func logicalLines(content string) []string {
	var out []string
	var buf strings.Builder
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.HasSuffix(strings.TrimSpace(line), "\\") {
			buf.WriteString(strings.TrimSuffix(strings.TrimSpace(line), "\\"))
			buf.WriteByte(' ')
			continue
		}
		buf.WriteString(line)
		out = append(out, buf.String())
		buf.Reset()
	}
	if buf.Len() > 0 {
		out = append(out, buf.String())
	}
	return out
}

func parseEnvPairs(s string) map[string]string {
	out := map[string]string{}
	for _, tok := range splitShellWords(s) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			continue
		}
		out[k] = strings.Trim(v, `'"`)
	}
	return out
}

func splitShellWords(s string) []string {
	var out []string
	var buf strings.Builder
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			buf.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t':
			if buf.Len() > 0 {
				out = append(out, buf.String())
				buf.Reset()
			}
		default:
			if r != '\n' && r != '\r' {
				buf.WriteRune(r)
			}
		}
	}
	if buf.Len() > 0 {
		out = append(out, buf.String())
	}
	return out
}

type composeDoc struct {
	Services map[string]struct {
		Image       string `yaml:"image"`
		Ports       []any  `yaml:"ports"`
		Environment any    `yaml:"environment"`
	} `yaml:"services"`
}

func parseCompose(content string) (ports []ComposePort, env map[string]string) {
	if len(content) > maxRepoFileSize {
		content = content[:maxRepoFileSize]
	}
	var doc composeDoc
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return nil, nil
	}
	env = map[string]string{}
	sortKeys := make([]string, 0, len(doc.Services))
	for name := range doc.Services {
		sortKeys = append(sortKeys, name)
	}
	sort.Strings(sortKeys)

	for _, name := range sortKeys {
		svc := doc.Services[name]
		for _, p := range svc.Ports {
			if cp, ok := parseComposePortEntry(p); ok {
				cp.Service = name
				ports = append(ports, cp)
			}
		}
		for k, v := range parseComposeEnv(svc.Environment) {
			if _, exists := env[k]; !exists {
				env[k] = v
			}
		}
	}
	if len(env) == 0 {
		env = nil
	}
	return ports, env
}

type composePortLong struct {
	Target    int    `yaml:"target"`
	Published any    `yaml:"published"`
	Protocol  string `yaml:"protocol"`
}

func parseComposePortEntry(p any) (ComposePort, bool) {
	switch v := p.(type) {
	case string:
		return parseComposePortString(v)
	case int:
		return ComposePort{Host: v, Container: v}, v >= 1 && v <= 65535
	case map[string]any:
		b, _ := yaml.Marshal(v)
		var l composePortLong
		if err := yaml.Unmarshal(b, &l); err != nil || l.Target <= 0 {
			return ComposePort{}, false
		}
		if l.Protocol != "" && !strings.EqualFold(l.Protocol, "tcp") {
			return ComposePort{}, false
		}
		host := l.Target
		switch pv := l.Published.(type) {
		case int:
			host = pv
		case string:
			if n, err := strconv.Atoi(pv); err == nil {
				host = n
			}
		}
		if host < 1 || host > 65535 {
			host = l.Target
		}
		return ComposePort{Host: host, Container: l.Target}, true
	}
	return ComposePort{}, false
}

func parseComposePortString(s string) (ComposePort, bool) {
	s = strings.Trim(s, `'"`)
	s, proto, _ := strings.Cut(s, "/")
	if proto != "" && !strings.EqualFold(proto, "tcp") {
		return ComposePort{}, false
	}
	parts := strings.Split(s, ":")
	switch len(parts) {
	case 1:
		n, err := strconv.Atoi(parts[0])
		if err != nil || n < 1 || n > 65535 {
			return ComposePort{}, false
		}
		return ComposePort{Host: n, Container: n}, true
	case 2, 3:
		hp := parts[len(parts)-2]
		cp := parts[len(parts)-1]
		if strings.Contains(hp, "-") || strings.Contains(cp, "-") {
			return ComposePort{}, false
		}
		h, errH := strconv.Atoi(hp)
		cont, errC := strconv.Atoi(cp)
		if errH == nil && errC == nil && h >= 1 && h <= 65535 && cont >= 1 && cont <= 65535 {
			return ComposePort{Host: h, Container: cont}, true
		}
		if errC == nil && cont >= 1 && cont <= 65535 && strings.HasPrefix(hp, "${") {
			return ComposePort{Host: cont, Container: cont}, true
		}
	}
	return ComposePort{}, false
}

func parseComposeEnv(raw any) map[string]string {
	out := map[string]string{}
	switch v := raw.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch val := v[k].(type) {
			case string:
				out[k] = val
			case int, int64, float64:
				out[k] = fmt.Sprintf("%v", val)
			case bool:
				out[k] = strconv.FormatBool(val)
			default:
				out[k] = ""
			}
		}
	case []any:
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				continue
			}
			k, val, has := strings.Cut(s, "=")
			if !has {
				val = ""
			}
			if k = strings.TrimSpace(k); k != "" {
				out[k] = strings.TrimSpace(val)
			}
		}
	}
	return out
}

func parseEnvFile(content string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader([]byte(content)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if k == "" || len(out) >= 64 {
			continue
		}
		out[k] = strings.Trim(strings.TrimSpace(v), `'"`)
	}
	return out
}
