package ai

import (
	"strings"
	"testing"
)

func testHosts() []appHostInfo {
	return []appHostInfo{
		{ID: "h1", Hostname: "web-1", OS: "linux Debian 12", PublicIP: "1.2.3.4", MeshIP: "100.64.0.1"},
		{ID: "h2", Hostname: "db-1", OS: "linux Ubuntu 24.04"},
	}
}

func testTemplates() []AppTemplateInfo {
	return []AppTemplateInfo{
		{
			ID: "redis", Name: "Redis", Category: "database", Image: "redis:alpine",
			DefaultPort: 6379, ContainerPort: 6379,
			EnvFields: []AppTemplateField{
				{Key: "REDIS_PASSWORD", Label: "访问密码", Required: true, IsSecret: true},
				{Key: "REDIS_DB", Label: "数据库编号", Default: "0"},
			},
		},
		{ID: "nginx", Name: "Nginx", Category: "web", Image: "nginx:alpine", DefaultPort: 80, ContainerPort: 80},
	}
}

func TestSanitizeAppDraftDropsInvalidChoices(t *testing.T) {
	d := &AppDraft{
		SourceType: strPtr("docker"), // unsupported
		HostID:     strPtr("ghost"),  // not online
		TemplateID: strPtr("nope"),   // not in catalog
		RepoURL:    strPtr("git@github.com:x/y.git"),
		Ports: []AppPortMapping{
			{Host: 0, Container: 80, BindScope: "public"},
			{Host: 8080, Container: 80, BindScope: "weird"},
			{Host: 9000, Container: 70000, BindScope: "public"},
		},
		EnvVars: map[string]string{"GOOD_KEY": "v", "bad-key": "v", "9BAD": "v"},
	}
	missing, warnings := sanitizeAppDraft(d, AppFormSnapshot{}, testHosts(), testTemplates())
	if d.SourceType != nil || d.HostID != nil || d.TemplateID != nil || d.RepoURL != nil {
		t.Fatalf("invalid fields not dropped: %+v", d)
	}
	if len(d.Ports) != 1 || d.Ports[0].Host != 8080 || d.Ports[0].BindScope != "public" {
		t.Fatalf("ports not sanitized: %+v", d.Ports)
	}
	if len(d.EnvVars) != 1 {
		t.Fatalf("env vars not filtered: %+v", d.EnvVars)
	}
	if len(warnings) < 4 {
		t.Fatalf("expected several warnings, got %v", warnings)
	}
	// No host selected anywhere => missing must call it out.
	found := false
	for _, m := range missing {
		if m == "目标主机" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing should include 目标主机: %v", missing)
	}
}

func TestSanitizeAppDraftTemplateParamsAndMissing(t *testing.T) {
	d := &AppDraft{
		SourceType:     strPtr("template"),
		HostID:         strPtr("h1"),
		TemplateID:     strPtr("redis"),
		TemplateParams: map[string]string{"REDIS_DB": "2", "UNKNOWN_KEY": "x"},
	}
	missing, _ := sanitizeAppDraft(d, AppFormSnapshot{}, testHosts(), testTemplates())
	if _, ok := d.TemplateParams["UNKNOWN_KEY"]; ok {
		t.Fatalf("unknown template param kept: %+v", d.TemplateParams)
	}
	if d.TemplateParams["REDIS_DB"] != "2" {
		t.Fatalf("valid template param lost: %+v", d.TemplateParams)
	}
	found := false
	for _, m := range missing {
		if strings.Contains(m, "访问密码") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing should include required 访问密码 param: %v", missing)
	}

	// Same draft with the password already in the form: nothing missing.
	form := AppFormSnapshot{TemplateParams: map[string]string{"REDIS_PASSWORD": "s3cret"}}
	missing, _ = sanitizeAppDraft(d, form, testHosts(), testTemplates())
	for _, m := range missing {
		if strings.Contains(m, "访问密码") {
			t.Fatalf("password present in form but still missing: %v", missing)
		}
	}
}

func TestSanitizeAppDraftPatchParamsAgainstFormTemplate(t *testing.T) {
	// Chat patch without template_id: params are filtered against the
	// template already selected in the form instead of being dropped.
	d := &AppDraft{TemplateParams: map[string]string{"REDIS_DB": "5", "UNKNOWN_KEY": "x"}}
	form := AppFormSnapshot{SourceMode: "template", TemplateID: "redis"}
	_, warnings := sanitizeAppDraft(d, form, testHosts(), testTemplates())
	if d.TemplateParams == nil || d.TemplateParams["REDIS_DB"] != "5" {
		t.Fatalf("form-template params should survive: %+v", d.TemplateParams)
	}
	if _, ok := d.TemplateParams["UNKNOWN_KEY"]; ok {
		t.Fatalf("unknown param kept: %+v", d.TemplateParams)
	}
	for _, w := range warnings {
		if strings.Contains(w, "没有对应模板") {
			t.Fatalf("unexpected no-template warning: %v", warnings)
		}
	}
}

func TestSanitizeAppDraftMeshWarning(t *testing.T) {
	// h2 has no mesh IP; a mesh port on it must warn (but stay, so the
	// human sees and fixes it).
	d := &AppDraft{
		SourceType: strPtr("image"),
		HostID:     strPtr("h2"),
		Image:      strPtr("redis:7"),
		Ports:      []AppPortMapping{{Host: 6379, Container: 6379, BindScope: "mesh"}},
	}
	_, warnings := sanitizeAppDraft(d, AppFormSnapshot{}, testHosts(), testTemplates())
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "异地组网") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected mesh warning, got %v", warnings)
	}
}

func TestRedactFormForLLM(t *testing.T) {
	form := AppFormSnapshot{
		SourceMode:     "template",
		TemplateID:     "redis",
		AuthVaultID:    "vault-secret-id",
		TemplateParams: map[string]string{"REDIS_PASSWORD": "p@ss", "REDIS_DB": "3"},
		EnvVars:        map[string]string{"API_TOKEN": "tok123"},
		ComposeContent: "services:\n  db:\n    environment:\n      - MYSQL_PASSWORD=hunter2\n      - PLAIN=ok\n",
	}
	out := redactFormForLLM(form, testTemplates())
	if out.AuthVaultID != "" {
		t.Fatalf("vault id leaked to LLM context")
	}
	if out.TemplateParams["REDIS_PASSWORD"] != "***" {
		t.Fatalf("secret template param not masked: %q", out.TemplateParams["REDIS_PASSWORD"])
	}
	if out.TemplateParams["REDIS_DB"] != "3" {
		t.Fatalf("non-secret param should stay visible: %q", out.TemplateParams["REDIS_DB"])
	}
	if out.EnvVars["API_TOKEN"] != "***" {
		t.Fatalf("env value not masked: %q", out.EnvVars["API_TOKEN"])
	}
	if strings.Contains(out.ComposeContent, "hunter2") {
		t.Fatalf("compose password leaked: %q", out.ComposeContent)
	}
	if !strings.Contains(out.ComposeContent, "PLAIN=ok") {
		t.Fatalf("compose content mangled: %q", out.ComposeContent)
	}
	// The original form must be untouched.
	if form.TemplateParams["REDIS_PASSWORD"] != "p@ss" || form.EnvVars["API_TOKEN"] != "tok123" {
		t.Fatalf("redaction mutated the caller's form")
	}
}

func TestExtractJSONPayload(t *testing.T) {
	cases := map[string]string{
		"```json\n{\"a\":1}\n```": `{"a":1}`,
		"好的，结果如下：{\"a\":1} 完毕":    `{"a":1}`,
		`{"a":1}`:       `{"a":1}`,
		"  {\"a\":1}  ": `{"a":1}`,
	}
	for in, want := range cases {
		if got := extractJSONPayload(in); got != want {
			t.Fatalf("extractJSONPayload(%q) = %q, want %q", in, got, want)
		}
	}
}
