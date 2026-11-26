package repoinspect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestParseDockerfile(t *testing.T) {
	dockerfile := `# builder stage
FROM golang:1.24 AS build
WORKDIR /src

FROM alpine:3.20
# 注释行应忽略
ENV CLEWDR_PORT=8484 \
    CLEWDR_IP="0.0.0.0"
ENV LEGACY_VALUE some legacy words
ARG BUILD_FLAG=1
EXPOSE 8484 53/udp 9000-9002
VOLUME ["/data", "/etc/clewdr"]
VOLUME /var/log/clewdr
ENTRYPOINT ["/usr/local/bin/clewdr"]
`
	ports, env, volumes := parseDockerfile(dockerfile)

	wantPorts := []int{8484, 9000, 9001, 9002}
	if len(ports) != len(wantPorts) {
		t.Fatalf("ports = %v, want %v (53/udp 应被跳过)", ports, wantPorts)
	}
	for i, p := range ports {
		if p != wantPorts[i] {
			t.Fatalf("ports = %v, want %v", ports, wantPorts)
		}
	}
	if env["CLEWDR_PORT"] != "8484" {
		t.Errorf("env CLEWDR_PORT = %q", env["CLEWDR_PORT"])
	}
	if env["CLEWDR_IP"] != "0.0.0.0" {
		t.Errorf("env CLEWDR_IP = %q（引号应被去除）", env["CLEWDR_IP"])
	}
	if env["LEGACY_VALUE"] != "some legacy words" {
		t.Errorf("legacy ENV KEY value 解析失败: %q", env["LEGACY_VALUE"])
	}
	if env["BUILD_FLAG"] != "1" {
		t.Errorf("ARG 也应收集: %q", env["BUILD_FLAG"])
	}
	if len(volumes) != 3 {
		t.Fatalf("volumes = %v, want [/data /etc/clewdr /var/log/clewdr]", volumes)
	}
}

func TestParseCompose(t *testing.T) {
	compose := `
services:
  web:
    image: ghcr.io/xerxes-2/clewdr:latest
    ports:
      - "8484:8484"
      - "127.0.0.1:9090:80"
      - "3000"
      - "5353:53/udp"
      - "8000-8003:8000-8003"
    environment:
      CLEWDR_PORT: "8484"
      CLEWDR_IP: 0.0.0.0
      DEBUG: true
  worker:
    image: example/worker
    ports:
      - target: 8081
        published: "18081"
        protocol: tcp
    environment:
      - QUEUE_URL=redis://redis:6379
      - ONLY_KEY
`
	ports, env := parseCompose(compose)

	want := []ComposePort{
		{Service: "web", Host: 8484, Container: 8484},
		{Service: "web", Host: 9090, Container: 80},
		{Service: "web", Host: 3000, Container: 3000},
		{Service: "worker", Host: 18081, Container: 8081},
	}
	if len(ports) != len(want) {
		t.Fatalf("ports = %+v, want %d 条", ports, len(want))
	}
	for i, p := range ports {
		if p != want[i] {
			t.Errorf("ports[%d] = %+v, want %+v", i, p, want[i])
		}
	}
	if env["CLEWDR_PORT"] != "8484" || env["CLEWDR_IP"] != "0.0.0.0" || env["DEBUG"] != "true" {
		t.Errorf("map 形式 env 解析有误: %v", env)
	}
	if env["QUEUE_URL"] != "redis://redis:6379" {
		t.Errorf("list 形式 env 解析有误: %v", env)
	}
	if v, ok := env["ONLY_KEY"]; !ok || v != "" {
		t.Errorf("无等号的 env 应记录空值: %v", env)
	}
}

func TestParseEnvFile(t *testing.T) {
	content := `# 数据库配置
DATABASE_URL="postgres://u:p@db:5432/x"
export REDIS_HOST='redis'
EMPTY_VALUE=
BROKEN LINE WITHOUT EQUALS
`
	m := parseEnvFile(content)
	if m["DATABASE_URL"] != "postgres://u:p@db:5432/x" {
		t.Errorf("DATABASE_URL = %q", m["DATABASE_URL"])
	}
	if m["REDIS_HOST"] != "redis" {
		t.Errorf("REDIS_HOST = %q", m["REDIS_HOST"])
	}
	if v, ok := m["EMPTY_VALUE"]; !ok || v != "" {
		t.Errorf("EMPTY_VALUE 应为存在且为空: %v", m)
	}
	if _, ok := m["BROKEN LINE WITHOUT EQUALS"]; ok {
		t.Errorf("无等号行应被忽略")
	}
}

func mockGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	files := map[string]string{
		"main/docker-compose.yml":   "services:\n  app:\n    image: x/y\n    ports:\n      - \"8484:8484\"\n    environment:\n      PORT: \"8484\"\n",
		"main/Dockerfile":           "FROM alpine\nEXPOSE 8484\nENV CLEWDR_PORT=8484\n",
		"main/.env.example":         "COOKIE=\nPORT=8484\n",
		"main/go.mod":               "module x/y\n",
		"dev/Dockerfile":            "FROM node:20\nEXPOSE 3000\n",
		"dev/apps/web/compose.yaml": "services:\n  web:\n    build: .\n    ports:\n      - \"5173:5173\"\n",
		"dev/apps/web/Dockerfile":   "FROM node:20\nEXPOSE 5173\n",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/repos/o/r/contents/") {
			if r.Header.Get("Authorization") != "Bearer tok-app" && r.Header.Get("Authorization") != "Bearer tok-vault" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			path := strings.TrimPrefix(r.URL.Path, "/repos/o/r/contents/")
			ref := r.URL.Query().Get("ref")
			if content, ok := files[ref+"/"+path]; ok {
				w.Header().Set("Content-Type", "application/vnd.github.raw+json")
				_, _ = w.Write([]byte(content))
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	return httptest.NewServer(mux)
}

func withMockHosts(t *testing.T, srv *httptest.Server) {
	t.Helper()
	u, _ := url.Parse(srv.URL)
	origAPI, origRaw := githubAPIBase, rawBase
	githubAPIBase, rawBase = "http://"+u.Host, "http://"+u.Host
	t.Cleanup(func() { githubAPIBase, rawBase = origAPI, origRaw })
}

type fakeAppTokens struct{ tok string }

func (f fakeAppTokens) InstallationTokenForRepo(repoURL string) (string, bool) {
	if strings.Contains(repoURL, "github.com/o/r") {
		return f.tok, true
	}
	return "", false
}

func TestInspectRepoEndToEndAndPrecedence(t *testing.T) {
	srv := mockGitHub(t)
	defer srv.Close()
	withMockHosts(t, srv)

	svc := NewService(fakeAppTokens{tok: "tok-app"}, func(id string) (string, bool) {
		if id == "v1" {
			return "tok-vault", true
		}
		return "", false
	})

	meta, err := svc.InspectRepo(context.Background(), "https://github.com/o/r", InspectOptions{})
	if err != nil {
		t.Fatalf("InspectRepo error: %v", err)
	}
	if !meta.HasCompose || meta.ComposeFile != "docker-compose.yml" || meta.Branch != "main" {
		t.Errorf("unexpected meta: %+v", meta)
	}
	if len(meta.ComposePorts) != 1 || meta.ComposePorts[0].Host != 8484 {
		t.Errorf("ComposePorts = %+v", meta.ComposePorts)
	}
	if meta.ComposeEnv["PORT"] != "8484" || meta.EnvExample["PORT"] != "8484" || meta.Language != "go" {
		t.Errorf("unexpected compose/env/lang: %+v", meta)
	}

	// Explicit branch + monorepo build_context + Vault override.
	subMeta, err := svc.InspectRepo(context.Background(), "https://github.com/o/r.git", InspectOptions{
		Branch:       "dev",
		BuildContext: "apps/web",
		AuthVaultID:  "v1",
	})
	if err != nil {
		t.Fatalf("InspectRepo subdir error: %v", err)
	}
	if subMeta.Branch != "dev" || !subMeta.HasCompose || subMeta.ComposeFile != "apps/web/compose.yaml" {
		t.Errorf("unexpected subdir compose meta: %+v", subMeta)
	}
	if !subMeta.HasDockerfile || subMeta.DockerfilePath != "apps/web/Dockerfile" {
		t.Errorf("unexpected subdir dockerfile meta: %+v", subMeta)
	}
	if len(subMeta.ExposedPorts) != 1 || subMeta.ExposedPorts[0] != 5173 {
		t.Errorf("ExposedPorts = %v, want [5173]", subMeta.ExposedPorts)
	}
}

func TestServiceRejectsInvalidInputs(t *testing.T) {
	svc := NewService(nil, nil)
	if _, err := svc.InspectRepo(context.Background(), "https://gitlab.com/a/b", InspectOptions{}); err == nil {
		t.Fatal("non-GitHub repo should fail")
	}
	if _, err := svc.InspectRepo(context.Background(), "not a url", InspectOptions{}); err == nil {
		t.Fatal("invalid URL should fail")
	}
	if _, err := svc.InspectRepo(context.Background(), "https://github.com/o/r", InspectOptions{BuildContext: "../secret"}); err == nil {
		t.Fatal("traversal build_context should fail")
	}
}
