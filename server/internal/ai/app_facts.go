package ai

// Ground-truth probing for the app-creation assistant: when the requirement
// text or the form snapshot names a container image or a GitHub repository,
// we fetch the objective metadata (exposed ports, env keys, compose layout)
// *before* calling the model and inject it into the prompt, and afterwards
// reconcile the sanitized draft against those facts. The goal is to keep the
// AI from inventing ports/variables for software it has never heard of
// ( AGENTS.md 3.7/3.16.7: drafts grounded in facts, never auto-created).

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"watchman/server/internal/registry"
)

// AppImageProbe is the ai-package mirror of registry.ImageMetadata. Err
// carries a human-readable probe failure; probing is best-effort and must
// never block a draft.
type AppImageProbe struct {
	Reference string
	Ports     []int
	EnvVars   map[string]string
	Volumes   []string
	Cmd       string
	Err       string
}

// AppRepoProbe mirrors gitprovider.RepoMetadata. ComposeContent is
// deliberately not mirrored: the full file is available through the
// inspect-repo API for the create form, but injecting it into the prompt
// would blow up token usage and could leak credentials from the repo.
type AppRepoProbe struct {
	RepoURL        string
	Branch         string
	HasCompose     bool
	ComposeFile    string
	ComposePorts   []AppPortMapping
	HasDockerfile  bool
	DockerfilePath string
	ExposedPorts   []int
	DockerfileEnv  map[string]string
	EnvExampleKeys []string
	Err            string
}

// AppProbers are the pluggable ground-truth fetchers, wired in main with
// adapters over registry.Inspect / gitprovider.Store.InspectRepo. Either
// field may be nil (capability absent).
type AppProbers struct {
	Image func(ctx context.Context, ref string) *AppImageProbe
	Repo  func(ctx context.Context, url string) *AppRepoProbe
}

// SetAppProbers injects the image/repository metadata probers.
func (a *Assistant) SetAppProbers(p AppProbers) { a.appProbers = p }

// appFacts carries one probe run: both the result and the raw reference it
// was queried with (needed to decide later whether the draft still points at
// the same target).
type appFacts struct {
	image    *AppImageProbe
	imageRef string
	repo     *AppRepoProbe
	repoURL  string
}

// appProbeCacheEntry is a short-lived memoized probe result; registry/GitHub
// calls are the slowest part of a draft/chat round, and the create-page chat
// fires one call per message, so successes are cached for 5 minutes.
type appProbeCacheEntry struct {
	image   *AppImageProbe
	repo    *AppRepoProbe
	expires time.Time
}

const appProbeCacheTTL = 5 * time.Minute
const appProbeCacheCap = 64

var githubRepoURLRe = regexp.MustCompile(`(?i)https?://github\.com/[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*`)

// findRepoURLInText extracts the first github.com owner/repo URL from prose.
func findRepoURLInText(text string) string {
	return strings.TrimSuffix(githubRepoURLRe.FindString(text), ".git")
}

// findImageRefInText extracts the first plausible image reference from prose.
func findImageRefInText(text string) string {
	for _, tok := range strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("，。、；：「」（）()<>\"'“”", r)
	}) {
		tok = strings.Trim(tok, ".,;:!?\"'()[]{}<>")
		if tok != "" && registry.LooksLikeImageRef(tok) {
			return tok
		}
	}
	return ""
}

// parseRepoIdentity lowercases the owner/repo pair of a GitHub URL for an
// equality check that ignores scheme/case noise.
func parseRepoIdentity(u string) string {
	m := githubRepoURLRe.FindString(strings.TrimSpace(u))
	if m == "" {
		return ""
	}
	m = strings.TrimSuffix(m, ".git")
	parts := strings.Split(strings.SplitN(m, "://", 2)[1], "/")
	if len(parts) < 3 {
		return ""
	}
	return strings.ToLower(parts[1] + "/" + parts[2])
}

func (a *Assistant) probeCacheGet(key string, isImage bool) (*AppImageProbe, *AppRepoProbe, bool) {
	a.probeMu.Lock()
	defer a.probeMu.Unlock()
	if a.probeCache == nil {
		return nil, nil, false
	}
	e, ok := a.probeCache[key]
	if !ok || time.Now().After(e.expires) {
		delete(a.probeCache, key)
		return nil, nil, false
	}
	if isImage {
		return e.image, nil, true
	}
	return nil, e.repo, true
}

func (a *Assistant) probeCacheSet(key string, image *AppImageProbe, repo *AppRepoProbe) {
	a.probeMu.Lock()
	defer a.probeMu.Unlock()
	if a.probeCache == nil {
		a.probeCache = make(map[string]appProbeCacheEntry)
	}
	if len(a.probeCache) >= appProbeCacheCap { // naive flush eviction
		a.probeCache = make(map[string]appProbeCacheEntry, appProbeCacheCap)
	}
	a.probeCache[key] = appProbeCacheEntry{image: image, repo: repo, expires: time.Now().Add(appProbeCacheTTL)}
}

// probeAppFacts detects image references / GitHub URLs in the requirement
// text or the current form and fetches their ground-truth metadata with
// bounded timeouts. Any single failure degrades to an Err marker; the caller
// then only surfaces a warning. Never returns nil when targets exist.
func (a *Assistant) probeAppFacts(ctx context.Context, text string, form AppFormSnapshot) *appFacts {
	facts := &appFacts{}

	imgRef := strings.TrimSpace(form.Image)
	if imgRef != "" {
		// An explicit form entry is probed whenever it parses as a
		// registry reference (guards against prose typed into the field).
		if _, err := registry.ParseReference(imgRef); err != nil {
			imgRef = ""
		}
	}
	if imgRef == "" {
		imgRef = findImageRefInText(text)
	}
	if imgRef != "" && a.appProbers.Image != nil {
		if hit, _, ok := a.probeCacheGet("img:"+imgRef, true); ok {
			facts.image = hit
		} else {
			c, cancel := context.WithTimeout(ctx, 6*time.Second)
			facts.image = a.appProbers.Image(c, imgRef)
			cancel()
			a.probeCacheSet("img:"+imgRef, facts.image, nil)
		}
		facts.imageRef = imgRef
	}

	repoURL := strings.TrimSpace(form.RepoURL)
	if !githubRepoURLRe.MatchString(repoURL) {
		repoURL = findRepoURLInText(text)
	} else {
		repoURL = strings.TrimSuffix(githubRepoURLRe.FindString(repoURL), ".git")
	}
	if repoURL != "" && a.appProbers.Repo != nil {
		if _, hit, ok := a.probeCacheGet("repo:"+repoURL, false); ok {
			facts.repo = hit
		} else {
			c, cancel := context.WithTimeout(ctx, 8*time.Second)
			facts.repo = a.appProbers.Repo(c, repoURL)
			cancel()
			a.probeCacheSet("repo:"+repoURL, nil, facts.repo)
		}
		facts.repoURL = repoURL
	}
	return facts
}

// secretValueKeyRe flags env keys whose value must not be sent to the model
// even when it came from a public image config (defence in depth).
var secretValueKeyRe = regexp.MustCompile(`(?i)password|passwd|secret|token|api[_-]?key|access[_-]?key`)

// summarizeEnv renders "K=V、K=***" for at most max entries, values trimmed.
func summarizeEnv(env map[string]string, max int) string {
	if len(env) == 0 {
		return ""
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	n := 0
	for _, k := range keys {
		if n == max {
			fmt.Fprintf(&b, " 等共 %d 项", len(keys))
			break
		}
		v := env[k]
		if secretValueKeyRe.MatchString(k) {
			v = "***"
		}
		if len(v) > 64 {
			v = v[:64] + "…"
		}
		if n > 0 {
			b.WriteString("、")
		}
		fmt.Fprintf(&b, "%s=%s", k, v)
		n++
	}
	return b.String()
}

func formatIntPorts(ports []int) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, fmt.Sprintf("%d", p))
	}
	return strings.Join(parts, "/")
}

// factsContext renders the prompt block injected above the user requirement.
// Empty string when nothing was detected/probed.
func factsContext(f *appFacts) string {
	if f == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("【已客观读取的真实配置（来自镜像仓库 / GitHub 探测，优先级最高的客观事实，必须严格遵循，禁止臆造）】\n")
	if f.image != nil {
		if f.image.Err != "" {
			fmt.Fprintf(&b, "- 镜像 %s：探测失败（%s），配置只能按经验推断并须在 warnings 提醒用户核实。\n", f.imageRef, f.image.Err)
		} else {
			fmt.Fprintf(&b, "- 镜像 %s：真实暴露端口 %s", f.image.Reference, formatIntPorts(f.image.Ports))
			if f.image.Cmd != "" {
				cmd := f.image.Cmd
				if len(cmd) > 200 {
					cmd = cmd[:200] + "…"
				}
				fmt.Fprintf(&b, "；启动命令 %q", cmd)
			}
			if env := summarizeEnv(f.image.EnvVars, 12); env != "" {
				fmt.Fprintf(&b, "；预设环境变量 %s", env)
			}
			if len(f.image.Volumes) > 0 {
				fmt.Fprintf(&b, "；数据卷路径 %s", strings.Join(f.image.Volumes, "、"))
			}
			b.WriteString("\n  对应要求：以该镜像部署时 ports 按真实暴露端口映射（宿主端口默认=容器端口，用户指定除外）；env_vars 只可用上述变量名；不得臆造镜像里不存在的端口/变量。\n")
		}
	}
	if f.repo != nil {
		if f.repo.Err != "" {
			fmt.Fprintf(&b, "- GitHub 仓库 %s：探测失败（%s），配置只能按经验推断并须在 warnings 提醒用户核实。\n", f.repoURL, f.repo.Err)
		} else {
			fmt.Fprintf(&b, "- GitHub 仓库 %s（默认分支 %s）：", f.repo.RepoURL, f.repo.Branch)
			if f.repo.HasCompose {
				fmt.Fprintf(&b, "检测到编排文件 %s", f.repo.ComposeFile)
				if len(f.repo.ComposePorts) > 0 {
					parts := make([]string, 0, len(f.repo.ComposePorts))
					for _, p := range f.repo.ComposePorts {
						parts = append(parts, fmt.Sprintf("%d:%d", p.Host, p.Container))
					}
					b.WriteString("（端口映射 " + strings.Join(parts, "、") + "）")
				} else {
					b.WriteString("（未声明端口映射）")
				}
			}
			if f.repo.HasDockerfile {
				fmt.Fprintf(&b, "；检测到 %s", f.repo.DockerfilePath)
				if len(f.repo.ExposedPorts) > 0 {
					fmt.Fprintf(&b, "（EXPOSE %s）", formatIntPorts(f.repo.ExposedPorts))
				}
				if env := summarizeEnv(f.repo.DockerfileEnv, 10); env != "" {
					fmt.Fprintf(&b, "；预设变量 %s", env)
				}
			}
			if !f.repo.HasCompose && !f.repo.HasDockerfile {
				b.WriteString("既无 Compose 编排也无 Dockerfile，只能按语言栈经验推断")
			}
			if len(f.repo.EnvExampleKeys) > 0 {
				fmt.Fprintf(&b, "；.env 示例键：%s", strings.Join(f.repo.EnvExampleKeys, "、"))
			}
			b.WriteString("\n  对应要求：构建方式以检出内容为准（有编排文件优先 compose，否则 dockerfile）；端口/变量以上述探测结果为准，不得臆造。\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// sameImageRef compares two references after registry normalization.
func sameImageRef(a, b string) bool {
	ra, ea := registry.ParseReference(strings.TrimSpace(a))
	rb, eb := registry.ParseReference(strings.TrimSpace(b))
	if ea == nil && eb == nil {
		return ra.Registry == rb.Registry && ra.Repository == rb.Repository && ra.Tag == rb.Tag
	}
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func effectiveSourceForAlign(d *AppDraft, form AppFormSnapshot) string {
	if d.SourceType != nil {
		return *d.SourceType
	}
	switch form.SourceMode {
	case "template", "image":
		return form.SourceMode
	case "compose":
		return "raw_compose"
	case "github", "custom":
		return "git"
	}
	return ""
}

func pickStrValue(dv *string, fv string) string {
	if dv != nil && *dv != "" {
		return *dv
	}
	return fv
}

func defaultPortMappings(ports []int) []AppPortMapping {
	out := make([]AppPortMapping, 0, len(ports))
	for _, p := range ports {
		out = append(out, AppPortMapping{Host: p, Container: p, BindScope: "public"})
	}
	return out
}

// alignDraftWithFacts reconciles the draft against probe facts. It runs after
// the generic port validation, so every port here is already in 1..65535.
func alignDraftWithFacts(d *AppDraft, form AppFormSnapshot, facts *appFacts, addWarning func(string)) {
	if d == nil || facts == nil {
		return
	}
	effSource := effectiveSourceForAlign(d, form)

	var real []int
	srcLabel := ""
	if effSource == "image" && facts.image != nil && facts.image.Err == "" {
		effImage := pickStrValue(d.Image, form.Image)
		// Only enforce when the draft still deploys the probed image.
		if sameImageRef(effImage, facts.imageRef) {
			real = facts.image.Ports
			srcLabel = facts.image.Reference
		}
	}
	if effSource == "git" && facts.repo != nil && facts.repo.Err == "" {
		repoOK := true
		if effRepo := pickStrValue(d.RepoURL, form.RepoURL); effRepo != "" && facts.repoURL != "" {
			repoOK = parseRepoIdentity(effRepo) == parseRepoIdentity(facts.repoURL)
		}
		if repoOK {
			bt := strings.TrimSpace(pickStrValue(d.BuildType, form.BuildType))
			if bt == "" {
				bt = "dockerfile"
			}
			switch {
			case facts.repo.HasCompose && !facts.repo.HasDockerfile && bt != "compose":
				d.BuildType = strPtr("compose")
				bt = "compose"
				addWarning(fmt.Sprintf("仓库检出 %s 但未发现 Dockerfile，构建方式已按事实对齐为 compose", facts.repo.ComposeFile))
			case facts.repo.HasDockerfile && !facts.repo.HasCompose && bt == "compose":
				d.BuildType = strPtr("dockerfile")
				bt = "dockerfile"
				addWarning(fmt.Sprintf("仓库检出 %s 但未发现 Compose 编排，构建方式已按事实对齐为 dockerfile", facts.repo.DockerfilePath))
			}
			if facts.repo.HasDockerfile && facts.repo.DockerfilePath != "" && facts.repo.DockerfilePath != "Dockerfile" {
				if cur := strings.TrimSpace(pickStrValue(d.Dockerfile, form.Dockerfile)); cur == "" || cur == "Dockerfile" {
					d.Dockerfile = strPtr(facts.repo.DockerfilePath)
				}
			}
			if bt == "dockerfile" {
				real = facts.repo.ExposedPorts
				srcLabel = "仓库 " + facts.repo.DockerfilePath
			}
		}
	}
	if len(real) == 0 {
		return
	}

	realSet := make(map[int]bool, len(real))
	for _, p := range real {
		realSet[p] = true
	}
	effPorts := d.Ports
	if len(effPorts) == 0 {
		effPorts = form.Ports
	}
	realFmt := formatIntPorts(real)
	switch {
	case len(effPorts) == 0:
		d.Ports = defaultPortMappings(real)
		addWarning(fmt.Sprintf("已按 %s 的真实暴露端口预填映射：%s", srcLabel, realFmt))
	default:
		kept := effPorts[:0]
		for _, p := range effPorts {
			if realSet[p.Container] {
				kept = append(kept, p)
			}
		}
		if len(kept) == len(effPorts) {
			// Every mapping already targets a real container port; only
			// mention real ports left unmapped.
			var unmapped []int
			for _, p := range real {
				found := false
				for _, m := range kept {
					if m.Container == p {
						found = true
						break
					}
				}
				if !found {
					unmapped = append(unmapped, p)
				}
			}
			if len(unmapped) > 0 {
				addWarning(fmt.Sprintf("%s 还暴露端口 %s（当前未映射，如需要可自行添加）", srcLabel, formatIntPorts(unmapped)))
			}
			return
		}
		if len(kept) == 0 {
			d.Ports = defaultPortMappings(real)
		} else {
			cp := make([]AppPortMapping, len(kept))
			copy(cp, kept)
			d.Ports = cp
		}
		addWarning(fmt.Sprintf("端口映射与 %s 的真实暴露端口（%s）不符，已按客观探测自动对齐", srcLabel, realFmt))
	}
}
