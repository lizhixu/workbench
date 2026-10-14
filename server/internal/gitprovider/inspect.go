package gitprovider

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// RepoMetadata is the ground truth read from a GitHub repository's checked-in
// deployment files (compose / Dockerfile / env example), used to stop the AI
// app assistant from inventing container ports and configuration keys for
// repos it has never seen in training data.
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

// candidate deployment/config files, probed in priority order.
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

// InspectRepo reads a GitHub repository's deployment-relevant files without
// cloning: it resolves the default branch, then tries the candidate file list
// via raw download, parsing whatever is found. token may be "" for public
// repositories; private repos require the caller's stored GitHub token.
//
// Missing files are never an error — an empty metadata result simply means
// "nothing recognizable found, fall back to model knowledge".
func (c *Client) InspectRepo(ctx context.Context, token, owner, repo string) (*RepoMetadata, error) {
	meta := &RepoMetadata{
		Owner: owner,
		Name:  repo,
	}

	branches, branchErr := c.branchCandidates(ctx, token, owner, repo)
	if branchErr != nil {
		// Anonymous probing may hit GitHub rate limits; degrade to guessing
		// the two overwhelmingly common default branch names.
		branches = []string{"main", "master"}
	}

	// fetchFirst walks candidate files × branch candidates and returns the
	// first readable file (path, content, resolved branch).
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

	if path, content, br := fetchFirst(composeCandidates); path != "" {
		meta.HasCompose = true
		meta.ComposeFile = path
		meta.Branch = br
		if len(content) > maxRepoFileSize {
			content = content[:maxRepoFileSize]
		}
		meta.ComposeContent = content
		meta.ComposePorts, meta.ComposeEnv = parseCompose(content)
	}

	if path, content, br := fetchFirst(dockerfileCandidates); path != "" {
		meta.HasDockerfile = true
		meta.DockerfilePath = path
		if meta.Branch == "" {
			meta.Branch = br
		}
		meta.ExposedPorts, meta.DockerfileEnv, meta.Volumes = parseDockerfile(content)
	}

	if _, content, _ := fetchFirst(envExampleCandidates); content != "" {
		meta.EnvExample = parseEnvFile(content)
		if len(meta.EnvExample) == 0 {
			meta.EnvExample = nil
		}
	}

	for _, m := range languageMarkers {
		if _, content, _ := fetchFirst([]string{m.path}); content != "" {
			meta.Language = m.lang
			break
		}
	}
	if meta.Branch == "" && len(branches) > 0 {
		meta.Branch = branches[0]
	}
	return meta, nil
}

// branchCandidates returns the repository's default branch (when the metadata
// API answers) ahead of the conventional main/master guess list.
func (c *Client) branchCandidates(ctx context.Context, token, owner, repo string) ([]string, error) {
	var info struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.get(ctx, token, fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo)), &info); err != nil {
		return nil, err
	}
	out := []string{}
	if info.DefaultBranch != "" {
		out = append(out, info.DefaultBranch)
	}
	for _, fb := range []string{"main", "master"} {
		dup := false
		for _, e := range out {
			if e == fb {
				dup = true
			}
		}
		if !dup {
			out = append(out, fb)
		}
	}
	return out, nil
}

// fetchRaw downloads one file. With a token it goes through the contents API
// (handles private repos); without one it uses raw.githubusercontent.com.
// A missing file returns ("", errNotFound)-style error by design.
func (c *Client) fetchRaw(ctx context.Context, token, owner, repo, branch, path string) (string, error) {
	var fullURL string
	if token != "" {
		fullURL = fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s",
			githubAPIBase, url.PathEscape(owner), url.PathEscape(repo), path, url.QueryEscape(branch))
	} else {
		fullURL = fmt.Sprintf("%s/%s/%s/%s/%s",
			rawBase, url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(branch), path)
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

// InspectRepo is the store-level wrapper used by HTTP handlers: it parses the
// URL, picks up the stored GitHub token when available, and probes.
func (s *Store) InspectRepo(ctx context.Context, repoURL string) (*RepoMetadata, error) {
	owner, repo, ok := ParseGitHubRepo(repoURL)
	if !ok {
		return nil, fmt.Errorf("目前仅支持探测 GitHub 仓库地址（https://github.com/<owner>/<repo>）")
	}
	token, _ := s.GetToken()
	meta, err := s.client.InspectRepo(ctx, token, owner, repo)
	if err != nil {
		return nil, err
	}
	meta.RepoURL = repoURL
	return meta, nil
}

// ---------------------------------------------------------------------------
// Dockerfile parsing
// ---------------------------------------------------------------------------

// parseDockerfile extracts EXPOSE / ENV / VOLUME instructions. Only simple
// single-stage facts are wanted, so instruction parsing stays deliberately
// line-based (the compose/build pipeline reuses the original file verbatim).
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
				// Port range — only expand small ones.
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
			// Modern K=V form (possibly multiple), or legacy "ENV KEY value".
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

// logicalLines joins backslash-continued lines — Dockerfile instructions can
// span multiple physical lines.
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

// parseEnvPairs parses the modern "ENV A=1 B="two words"" form.
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

// splitShellWords splits on whitespace while honoring simple quotes.
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

// ---------------------------------------------------------------------------
// Compose parsing
// ---------------------------------------------------------------------------

type composeDoc struct {
	Services map[string]struct {
		Image       string `yaml:"image"`
		Ports       []any  `yaml:"ports"`
		Environment any    `yaml:"environment"`
	} `yaml:"services"`
}

// parseCompose extracts port mappings and environment variables from every
// service. Compose long syntax, ip prefixes, protocols and env list/map forms
// are all normalized into the same shape the app form uses.
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

// parseComposePortEntry handles both short ("host:container") and long
// (map with target/published) forms.
func parseComposePortEntry(p any) (ComposePort, bool) {
	switch v := p.(type) {
	case string:
		return parseComposePortString(v)
	case int:
		// "ports: [3000]" — container-only, host port is ephemeral.
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

// parseComposePortString parses "8484", "8484:8484", "127.0.0.1:8484:8484"
// (optionally with /tcp suffix). Port ranges and non-TCP mappings are skipped.
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
		// Drop an optional IP host prefix ("127.0.0.1:...").
		hp := parts[len(parts)-2]
		cp := parts[len(parts)-1]
		if strings.Contains(hp, "-") || strings.Contains(cp, "-") {
			return ComposePort{}, false // skip ranges
		}
		h, errH := strconv.Atoi(hp)
		cont, errC := strconv.Atoi(cp)
		if errH == nil && errC == nil && h >= 1 && h <= 65535 && cont >= 1 && cont <= 65535 {
			return ComposePort{Host: h, Container: cont}, true
		}
		// "${PORT}:8484"-style interpolation: keep container port only.
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

// ---------------------------------------------------------------------------
// .env example parsing
// ---------------------------------------------------------------------------

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
