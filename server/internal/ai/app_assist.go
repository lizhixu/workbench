package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// AI assistance for the app creation page: turn a natural-language
// requirement into a pre-filled (but never submitted) create-app form draft,
// and answer follow-up questions with the live form state as context. The
// model only ever produces drafts — the human reviews and clicks "create"
// ( AGENTS.md 3.16.7: AI outputs are drafts, high-impact actions always need
// manual confirmation).

// appCatalogRef exposes the declarative app template catalog without
// importing the apps package.
type appCatalogRef interface {
	AppTemplates() []AppTemplateInfo
}

// meshRef resolves a host's overlay (Tailscale) IP, "" when not joined.
type meshRef interface {
	MeshIP(hostID string) string
}

// AppTemplateField mirrors apps.EnvField for prompt context and redaction.
type AppTemplateField struct {
	Key         string
	Label       string
	Description string
	Default     string
	Required    bool
	IsSecret    bool
}

// AppTemplateInfo mirrors apps.AppTemplate for prompt context.
type AppTemplateInfo struct {
	ID            string
	Name          string
	Category      string
	Description   string
	Image         string
	DefaultPort   int
	ContainerPort int
	DefaultVolume string
	EnvFields     []AppTemplateField
}

// SetAppCatalog injects the app template catalog used to ground app drafts.
// Safe to call with nil (drafts then cannot pick templates).
func (a *Assistant) SetAppCatalog(ref appCatalogRef) { a.appCatalog = ref }

// SetMeshRef injects the overlay-network lookup used to ground port
// bind_scope suggestions. Safe to call with nil (mesh treated as absent).
func (a *Assistant) SetMeshRef(ref meshRef) { a.mesh = ref }

// AppPortMapping is one host:container port binding of the create-app form.
type AppPortMapping struct {
	Host      int    `json:"host"`
	Container int    `json:"container"`
	BindScope string `json:"bind_scope"` // public | mesh
}

// AppFormSnapshot is the create-app form state as sent by the frontend for
// context (draft base / chat context). Secret values inside it are redacted
// before anything is sent to the model (see redactFormForLLM).
type AppFormSnapshot struct {
	SourceMode      string            `json:"source_mode"` // template|image|compose|github|custom
	Name            string            `json:"name"`
	ContainerName   string            `json:"container_name"`
	HostID          string            `json:"host_id"`
	TemplateID      string            `json:"template_id"`
	TemplateParams  map[string]string `json:"template_params"`
	Image           string            `json:"image"`
	ComposeContent  string            `json:"compose_content"`
	RepoURL         string            `json:"repo_url"`
	Branch          string            `json:"branch"`
	AuthVaultID     string            `json:"auth_vault_id"`
	AutoDeploy      bool              `json:"auto_deploy"`
	BuildType       string            `json:"build_type"`
	Dockerfile      string            `json:"dockerfile"`
	BuildContext    string            `json:"build_context"`
	BuildTimeoutSec int               `json:"build_timeout_sec"`
	Ports           []AppPortMapping  `json:"ports"`
	EnvVars         map[string]string `json:"env_vars"`
	Volumes         []string          `json:"volumes"`
	HealthcheckURL  string            `json:"healthcheck_url"`
	Domain          string            `json:"domain"`
	ProxyMode       string            `json:"proxy_mode"`
	GatewayHostID   string            `json:"gateway_host_id"`
}

// AppDraft is the model-proposed form fill. Every scalar is a pointer so
// "leave unchanged" (absent) is distinguishable from an explicit value; the
// frontend applies only the fields that are present.
type AppDraft struct {
	SourceType      *string           `json:"source_type,omitempty"` // template|image|raw_compose|git
	Name            *string           `json:"name,omitempty"`
	ContainerName   *string           `json:"container_name,omitempty"`
	HostID          *string           `json:"host_id,omitempty"`
	TemplateID      *string           `json:"template_id,omitempty"`
	TemplateParams  map[string]string `json:"template_params,omitempty"`
	Image           *string           `json:"image,omitempty"`
	ComposeContent  *string           `json:"compose_content,omitempty"`
	RepoURL         *string           `json:"repo_url,omitempty"`
	Branch          *string           `json:"branch,omitempty"`
	AutoDeploy      *bool             `json:"auto_deploy,omitempty"`
	BuildType       *string           `json:"build_type,omitempty"`
	Dockerfile      *string           `json:"dockerfile,omitempty"`
	BuildContext    *string           `json:"build_context,omitempty"`
	BuildTimeoutSec *int              `json:"build_timeout_sec,omitempty"`
	Ports           []AppPortMapping  `json:"ports,omitempty"`
	EnvVars         map[string]string `json:"env_vars,omitempty"`
	Volumes         []string          `json:"volumes,omitempty"`
	HealthcheckURL  *string           `json:"healthcheck_url,omitempty"`
	Domain          *string           `json:"domain,omitempty"`
	ProxyMode       *string           `json:"proxy_mode,omitempty"`
	GatewayHostID   *string           `json:"gateway_host_id,omitempty"`
}

// AppDraftResponse is the sanitized one-shot form draft.
type AppDraftResponse struct {
	Draft       *AppDraft `json:"draft"`
	Explanation string    `json:"explanation"`
	Missing     []string  `json:"missing"`
	Warnings    []string  `json:"warnings"`
	Model       string    `json:"model"`
}

// AppChatResponse is one conversational answer, optionally carrying a form
// patch the user may apply (never auto-applied).
type AppChatResponse struct {
	Answer        string    `json:"answer"`
	FormPatch     *AppDraft `json:"form_patch,omitempty"`
	PatchWarnings []string  `json:"patch_warnings,omitempty"`
	Model         string    `json:"model"`
}

type appHostInfo struct {
	ID       string
	Hostname string
	OS       string
	PublicIP string
	MeshIP   string
}

func (a *Assistant) appHosts() []appHostInfo {
	if a.reg == nil {
		return nil
	}
	var out []appHostInfo
	for _, ag := range a.reg.ListAgents() {
		if ag == nil || ag.Status != "online" {
			continue
		}
		h := appHostInfo{ID: ag.ID, Hostname: ag.Hostname, PublicIP: ag.PublicIP}
		h.OS = strings.TrimSpace(ag.OS + " " + ag.Distro)
		if a.mesh != nil {
			h.MeshIP = a.mesh.MeshIP(ag.ID)
		}
		out = append(out, h)
	}
	return out
}

func (a *Assistant) appTemplates() []AppTemplateInfo {
	if a.appCatalog == nil {
		return nil
	}
	return a.appCatalog.AppTemplates()
}

func hostsContext(hosts []appHostInfo) string {
	if len(hosts) == 0 {
		return "（当前没有在线主机，host_id 必须留空）"
	}
	var b strings.Builder
	for _, h := range hosts {
		mesh := "无"
		if h.MeshIP != "" {
			mesh = h.MeshIP
		}
		fmt.Fprintf(&b, "- id: %s | 主机名: %s | 系统: %s | 公网IP: %s | 组网IP: %s\n",
			h.ID, h.Hostname, h.OS, h.PublicIP, mesh)
	}
	return strings.TrimRight(b.String(), "\n")
}

func templatesContext(templates []AppTemplateInfo) string {
	if len(templates) == 0 {
		return "（无可用模板）"
	}
	var b strings.Builder
	for _, t := range templates {
		fmt.Fprintf(&b, "- id: %s | 名称: %s | 分类: %s | 镜像: %s | 默认端口: %d->%d | 说明: %s\n",
			t.ID, t.Name, t.Category, t.Image, t.DefaultPort, t.ContainerPort, t.Description)
		for _, f := range t.EnvFields {
			req := "可选"
			if f.Required {
				req = "必填"
			}
			secret := ""
			if f.IsSecret {
				secret = " 密钥"
			}
			fmt.Fprintf(&b, "  参数 %s（%s）%s%s 默认:%q 说明:%s\n",
				f.Key, f.Label, req, secret, f.Default, f.Description)
		}
		if t.DefaultVolume != "" {
			fmt.Fprintf(&b, "  默认卷: %s\n", t.DefaultVolume)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// extractJSONPayload strips markdown fences and surrounding prose so a
// strict JSON object can be unmarshalled (mirrors the Nl2Command cleanup).
func extractJSONPayload(answer string) string {
	s := strings.TrimSpace(answer)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "{"); i > 0 {
		s = s[i:]
	}
	if j := strings.LastIndex(s, "}"); j >= 0 && j < len(s)-1 {
		s = s[:j+1]
	}
	return s
}

var composeSecretLineRe = regexp.MustCompile(`(?i)((?:password|passwd|secret|token|api[_-]?key)\s*[:=]\s*)\S+`)

// redactFormForLLM returns a copy of the form snapshot safe to send to the
// model: env values and secret template params are masked, compose lines
// that look like credentials are masked, and the vault credential id (an
// internal identifier the model must never pick) is cleared.
func redactFormForLLM(form AppFormSnapshot, templates []AppTemplateInfo) AppFormSnapshot {
	out := form
	out.AuthVaultID = ""
	if len(form.EnvVars) > 0 {
		out.EnvVars = make(map[string]string, len(form.EnvVars))
		for k := range form.EnvVars {
			out.EnvVars[k] = "***"
		}
	}
	if len(form.TemplateParams) > 0 {
		out.TemplateParams = make(map[string]string, len(form.TemplateParams))
		secretKeys := map[string]bool{}
		for _, t := range templates {
			if t.ID == form.TemplateID {
				for _, f := range t.EnvFields {
					if f.IsSecret {
						secretKeys[f.Key] = true
					}
				}
			}
		}
		for k, v := range form.TemplateParams {
			if secretKeys[k] {
				out.TemplateParams[k] = "***"
			} else {
				out.TemplateParams[k] = v
			}
		}
	}
	if form.ComposeContent != "" {
		out.ComposeContent = composeSecretLineRe.ReplaceAllString(form.ComposeContent, "${1}***")
	}
	return out
}

func formContextJSON(form AppFormSnapshot, templates []AppTemplateInfo) string {
	redacted := redactFormForLLM(form, templates)
	if b, err := json.MarshalIndent(redacted, "", "  "); err == nil {
		return string(b)
	}
	return "{}"
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func strPtr(s string) *string { return &s }

// sanitizeAppDraft validates a model-proposed draft against the real host
// list and template catalog, nil-ing out anything invalid so the frontend
// can apply the result blindly. It also computes what is still missing for a
// submittable form, taking the current form state as the fallback base.
// It returns deduplicated missing/warning lists in Chinese.
func sanitizeAppDraft(d *AppDraft, form AppFormSnapshot, hosts []appHostInfo, templates []AppTemplateInfo) (missing, warnings []string) {
	if d == nil {
		return []string{"目标主机", "应用名称"}, nil
	}
	missingSet := map[string]bool{}
	addMissing := func(s string) {
		if !missingSet[s] {
			missingSet[s] = true
			missing = append(missing, s)
		}
	}
	warnSet := map[string]bool{}
	addWarning := func(s string) {
		if !warnSet[s] {
			warnSet[s] = true
			warnings = append(warnings, s)
		}
	}

	hostByID := make(map[string]appHostInfo, len(hosts))
	for _, h := range hosts {
		hostByID[h.ID] = h
	}
	tplByID := make(map[string]AppTemplateInfo, len(templates))
	for _, t := range templates {
		tplByID[t.ID] = t
	}

	// Source type whitelist.
	if d.SourceType != nil {
		switch *d.SourceType {
		case "template", "image", "raw_compose", "git":
		default:
			addWarning(fmt.Sprintf("AI 给出的部署来源 %q 不受支持，已忽略", *d.SourceType))
			d.SourceType = nil
		}
	}

	// Host must be one of the online hosts.
	if d.HostID != nil {
		if _, ok := hostByID[*d.HostID]; !ok {
			addWarning("AI 选择的目标主机不存在或不在线，已忽略，请手动选择")
			d.HostID = nil
		}
	}

	// Template must exist; params are filtered to its declared fields.
	var draftTpl *AppTemplateInfo
	if d.TemplateID != nil {
		if t, ok := tplByID[*d.TemplateID]; ok {
			draftTpl = &t
		} else {
			addWarning(fmt.Sprintf("AI 选择的模板 %q 不存在，已忽略", *d.TemplateID))
			d.TemplateID = nil
		}
	}
	// A chat patch may omit template_id and only tweak params of the
	// template already selected in the form; filter against that one.
	if draftTpl == nil && d.TemplateParams != nil && form.TemplateID != "" {
		if t, ok := tplByID[form.TemplateID]; ok {
			draftTpl = &t
		}
	}
	if d.TemplateParams != nil {
		if draftTpl == nil {
			addWarning("AI 给出的模板参数没有对应模板，已忽略")
			d.TemplateParams = nil
		} else {
			allowed := make(map[string]AppTemplateField, len(draftTpl.EnvFields))
			for _, f := range draftTpl.EnvFields {
				allowed[f.Key] = f
			}
			for k, v := range d.TemplateParams {
				if _, ok := allowed[k]; !ok {
					delete(d.TemplateParams, k)
					continue
				}
				if len(v) > 4096 {
					d.TemplateParams[k] = v[:4096]
				}
			}
			if len(d.TemplateParams) == 0 {
				d.TemplateParams = nil
			}
		}
	}

	// Ports: keep only valid mappings.
	if d.Ports != nil {
		kept := d.Ports[:0]
		dropped := false
		for _, p := range d.Ports {
			if p.Host < 1 || p.Host > 65535 || p.Container < 1 || p.Container > 65535 {
				dropped = true
				continue
			}
			if p.BindScope != "mesh" {
				p.BindScope = "public"
			}
			kept = append(kept, p)
		}
		if dropped {
			addWarning("AI 给出的个别端口映射不合法（端口须在 1~65535），已丢弃")
		}
		if len(kept) > 16 {
			kept = kept[:16]
			addWarning("端口映射过多，已截断为前 16 条")
		}
		d.Ports = kept
		// A mesh binding needs a mesh IP on the chosen host.
		effHost := ""
		if d.HostID != nil {
			effHost = *d.HostID
		} else {
			effHost = form.HostID
		}
		if effHost != "" {
			if h, ok := hostByID[effHost]; ok && h.MeshIP == "" {
				for _, p := range d.Ports {
					if p.BindScope == "mesh" {
						addWarning("目标主机未加入异地组网，「仅异地组网」端口将无法绑定，请改主机或改公网开放")
						break
					}
				}
			}
		}
	}

	// Scalar hygiene.
	trim := func(p **string, max int) {
		if *p == nil {
			return
		}
		s := strings.TrimSpace(**p)
		if len(s) > max {
			s = s[:max]
		}
		**p = s
	}
	trim(&d.Name, 128)
	trim(&d.ContainerName, 128)
	trim(&d.Image, 512)
	trim(&d.RepoURL, 1024)
	trim(&d.Branch, 255)
	trim(&d.Dockerfile, 255)
	trim(&d.BuildContext, 255)
	trim(&d.HealthcheckURL, 1024)
	trim(&d.Domain, 253)
	if d.ComposeContent != nil && len(*d.ComposeContent) > 64<<10 {
		c := (*d.ComposeContent)[:64<<10]
		d.ComposeContent = &c
		addWarning("Compose 内容过长，已截断")
	}
	if d.RepoURL != nil && *d.RepoURL != "" && !strings.HasPrefix(*d.RepoURL, "http://") && !strings.HasPrefix(*d.RepoURL, "https://") {
		addWarning("AI 给出的 Git 仓库地址不是 http(s) URL，已忽略")
		d.RepoURL = nil
	}
	if d.BuildType != nil && *d.BuildType != "dockerfile" && *d.BuildType != "compose" {
		d.BuildType = nil
	}
	if d.BuildTimeoutSec != nil {
		if *d.BuildTimeoutSec < 60 {
			*d.BuildTimeoutSec = 60
		}
		if *d.BuildTimeoutSec > 3600 {
			*d.BuildTimeoutSec = 3600
		}
	}
	if d.Domain != nil && (*d.Domain == "" || strings.ContainsAny(*d.Domain, " /:")) {
		addWarning("AI 给出的域名格式可疑，已忽略")
		d.Domain = nil
	}
	if d.ProxyMode != nil && *d.ProxyMode != "local" && *d.ProxyMode != "gateway" {
		d.ProxyMode = nil
	}
	if d.GatewayHostID != nil {
		if _, ok := hostByID[*d.GatewayHostID]; !ok {
			addWarning("AI 选择的网关主机不存在或不在线，已忽略")
			d.GatewayHostID = nil
		}
	}

	// Env vars: valid keys only.
	if d.EnvVars != nil {
		for k, v := range d.EnvVars {
			if !envKeyRe.MatchString(k) {
				delete(d.EnvVars, k)
				addWarning(fmt.Sprintf("环境变量名 %q 不合法，已丢弃", k))
				continue
			}
			if len(v) > 8192 {
				d.EnvVars[k] = v[:8192]
			}
		}
		if len(d.EnvVars) > 64 {
			addWarning("环境变量过多，请人工精简")
		}
		if len(d.EnvVars) == 0 {
			d.EnvVars = nil
		}
	}
	if len(d.Volumes) > 32 {
		d.Volumes = d.Volumes[:32]
		addWarning("挂载卷过多，已截断为前 32 条")
	}

	// Missing computation (draft overlaid on the current form).
	effSource := ""
	if d.SourceType != nil {
		effSource = *d.SourceType
	} else {
		switch form.SourceMode {
		case "template", "image":
			effSource = form.SourceMode
		case "compose":
			effSource = "raw_compose"
		case "github", "custom":
			effSource = "git"
		}
	}
	effHost := form.HostID
	if d.HostID != nil {
		effHost = *d.HostID
	}
	if effHost == "" {
		addMissing("目标主机")
	}
	pickStr := func(dv *string, fv string) string {
		if dv != nil && *dv != "" {
			return *dv
		}
		return fv
	}
	switch effSource {
	case "template":
		effTplID := pickStr(d.TemplateID, form.TemplateID)
		if effTplID == "" {
			addMissing("应用模板")
		} else if tpl, ok := tplByID[effTplID]; ok {
			for _, f := range tpl.EnvFields {
				if !f.Required {
					continue
				}
				v := form.TemplateParams[f.Key]
				if d.TemplateParams != nil {
					if dv, ok := d.TemplateParams[f.Key]; ok {
						v = dv
					}
				}
				if strings.TrimSpace(v) == "" {
					addMissing("模板参数「" + f.Label + "」")
				}
			}
		}
	case "image":
		if pickStr(d.Image, form.Image) == "" {
			addMissing("镜像地址")
		}
	case "raw_compose":
		if pickStr(d.ComposeContent, form.ComposeContent) == "" {
			addMissing("Compose 内容")
		}
	case "git":
		if pickStr(d.RepoURL, form.RepoURL) == "" {
			addMissing("Git 仓库地址")
		}
	}
	if effSource != "template" && pickStr(d.Name, form.Name) == "" {
		addMissing("应用名称")
	}
	if missing == nil {
		missing = []string{}
	}
	if warnings == nil {
		warnings = []string{}
	}
	return missing, warnings
}

const appAssistSystemRules = `规则：
1. 只返回一个 JSON 对象，不要输出任何解释性文字或 markdown 围栏。
2. source_type 只能是 "template"、"image"、"raw_compose"、"git" 之一；用户说"部署某个现成软件/模板"时优先 template；给镜像名时用 image；用户贴了或要求 compose 编排时用 raw_compose；给 Git 仓库时用 git。
3. host_id 只能从「可用主机」清单中选择 id；用户按主机名或 IP 指代某台时据此匹配；完全没提部署到哪里时留空（不要瞎猜）。
4. template_id 只能从「模板目录」中选择 id；template_params 的键只能是该模板声明的参数键；必填的密钥参数除非用户明确要求生成，否则留空并在 missing 里列出。
5. 端口 bind_scope 只能是 "public" 或 "mesh"；仅当目标主机有组网IP 且用户要求内网/组网访问，或部署数据库类服务（redis/mysql/postgres 等）时才建议 mesh。
6. 不要输出 auth_vault_id（Git 凭据必须由用户在页面手选）。git 来源 branch 缺省 "main"，build_type 缺省 "dockerfile"。
7. env_vars 只填用户明确要求的环境变量；不要编造连接串、口令等敏感值（用户要求生成时除外）。
8. missing 列出还缺哪些用户必须补充的信息（中文短语，如 "目标主机"、"模板参数「数据库密码」"）；warnings 列出需要提醒用户的注意事项（中文）。explanation 用一两句中文说明你的选型理由。
9. 能确定的字段都填上，不确定的留空并写入 missing，不要为了填满而编造。`

const appDraftJSONSpec = `返回 JSON 格式（字段没把握就省略该字段，不要填 null）：
{"draft":{"source_type":"template","name":"应用名","container_name":"","host_id":"主机id","template_id":"模板id","template_params":{"键":"值"},"image":"镜像","compose_content":"YAML","repo_url":"https://...","branch":"main","build_type":"dockerfile","dockerfile":"Dockerfile","build_context":".","build_timeout_sec":900,"auto_deploy":true,"ports":[{"host":8080,"container":80,"bind_scope":"public"}],"env_vars":{"K":"V"},"volumes":["/host:/container"],"healthcheck_url":"","domain":"","proxy_mode":"local","gateway_host_id":""},"explanation":"选型说明","missing":["目标主机"],"warnings":[]}`

// AppDraft turns a natural-language requirement into a sanitized create-app
// form draft. It never creates anything: the frontend fills the form with
// the draft and the human submits through the regular audited endpoint.
func (a *Assistant) AppDraft(ctx context.Context, requirement string, form AppFormSnapshot) (*AppDraftResponse, error) {
	cfg := a.Config()
	if !cfg.Enabled || cfg.BaseURL == "" {
		return nil, fmt.Errorf("AI 辅助功能未启用或未配置端点，请前往「系统设置」-「AI 大模型配置」中配置。")
	}
	requirement = strings.TrimSpace(requirement)
	if requirement == "" {
		return nil, fmt.Errorf("requirement required")
	}
	if len([]rune(requirement)) > 4000 {
		return nil, fmt.Errorf("需求描述过长（上限 4000 字）")
	}

	hosts := a.appHosts()
	templates := a.appTemplates()

	prompt := fmt.Sprintf(`你是一个应用部署配置助手。用户会用自然语言描述想部署的应用，请你把需求转换成「创建应用」表单草稿。

可用主机（只能从中选择 host_id）：
%s

模板目录（template_id 与参数只能从中选）：
%s

用户当前表单状态（JSON，已脱敏，*** 表示已填写的密钥；可在此基础上补全）：
%s

用户需求：%s

%s

%s`, hostsContext(hosts), templatesContext(templates), formContextJSON(form, templates), requirement, appAssistSystemRules, appDraftJSONSpec)

	answer, err := a.callLLM(ctx, prompt)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Draft       *AppDraft `json:"draft"`
		Explanation string    `json:"explanation"`
		Missing     []string  `json:"missing"`
		Warnings    []string  `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(extractJSONPayload(answer)), &parsed); err != nil || parsed.Draft == nil {
		return nil, fmt.Errorf("AI 返回的草稿无法解析，请换个说法重试")
	}
	missing, warnings := sanitizeAppDraft(parsed.Draft, form, hosts, templates)
	for _, m := range parsed.Missing {
		found := false
		for _, have := range missing {
			if have == m {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, m)
		}
	}
	warnings = append(warnings, parsed.Warnings...)
	if len(missing) > 12 {
		missing = missing[:12]
	}
	if len(warnings) > 12 {
		warnings = warnings[:12]
	}
	return &AppDraftResponse{
		Draft:       parsed.Draft,
		Explanation: strings.TrimSpace(parsed.Explanation),
		Missing:     missing,
		Warnings:    warnings,
		Model:       cfg.Model,
	}, nil
}

// AppChat answers a question about the in-progress create-app form. The
// live (redacted) form state rides along as context on every call, and the
// model may propose a form_patch that the UI offers as an explicit
// "apply" action — it is never applied silently.
func (a *Assistant) AppChat(ctx context.Context, question string, history []ChatMessage, form AppFormSnapshot) (*AppChatResponse, error) {
	cfg := a.Config()
	if !cfg.Enabled || cfg.BaseURL == "" {
		return nil, fmt.Errorf("AI 辅助功能未启用或未配置端点，请前往「系统设置」-「AI 大模型配置」中配置。")
	}
	question = strings.TrimSpace(question)
	if question == "" {
		return nil, fmt.Errorf("question required")
	}
	if len([]rune(question)) > 4000 {
		return nil, fmt.Errorf("问题过长（上限 4000 字）")
	}

	hosts := a.appHosts()
	templates := a.appTemplates()

	// Cap the carried history so the prompt stays bounded.
	if len(history) > 10 {
		history = history[len(history)-10:]
	}
	var hist strings.Builder
	for _, m := range history {
		role := "用户"
		if m.Role == "assistant" {
			role = "助手"
		}
		fmt.Fprintf(&hist, "%s：%s\n", role, m.Content)
	}
	histText := strings.TrimRight(hist.String(), "\n")
	if histText == "" {
		histText = "（无）"
	}

	prompt := fmt.Sprintf(`你是一个应用部署配置助手，正在协助用户填写「创建应用」表单。用户会一边填表一边向你提问。

可用主机：
%s

模板目录：
%s

用户当前表单状态（JSON，已脱敏，*** 表示已填写的密钥）：
%s

对话历史：
%s

用户问题：%s

要求：
1. 只返回一个 JSON 对象：{"answer":"中文回答","form_patch":null}。answer 用中文、简洁可执行。
2. 当且仅当用户的意图是修改表单时，才在 form_patch 中给出要修改的字段（格式与表单草稿相同，只包含需要改动的字段，没把握的字段不要出现）；只是咨询问题时 form_patch 必须为 null。
3. form_patch 的 host_id/template_id 只能从上面清单中选；不要给出 auth_vault_id；端口 bind_scope 仅在目标主机有组网IP 时才可建议 mesh。
4. 表单里 *** 的字段是用户已填的密钥，不要要求用户重复提供，也不要在回答中猜测其内容。`, hostsContext(hosts), templatesContext(templates), formContextJSON(form, templates), histText, question)

	answer, err := a.callLLM(ctx, prompt)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Answer    string    `json:"answer"`
		FormPatch *AppDraft `json:"form_patch"`
	}
	resp := &AppChatResponse{Model: cfg.Model}
	if err := json.Unmarshal([]byte(extractJSONPayload(answer)), &parsed); err != nil {
		// Degrade gracefully: a non-JSON reply is still a usable answer.
		resp.Answer = strings.TrimSpace(answer)
		return resp, nil
	}
	resp.Answer = strings.TrimSpace(parsed.Answer)
	if parsed.FormPatch != nil {
		_, patchWarnings := sanitizeAppDraft(parsed.FormPatch, form, hosts, templates)
		resp.FormPatch = parsed.FormPatch
		resp.PatchWarnings = patchWarnings
	}
	if resp.Answer == "" && resp.FormPatch == nil {
		return nil, fmt.Errorf("AI 返回为空，请重试")
	}
	return resp, nil
}
