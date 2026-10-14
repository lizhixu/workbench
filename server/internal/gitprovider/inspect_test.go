package gitprovider

import (
	"context"
	"log/slog"
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
      - "3000"               # 仅容器端口
      - "5353:53/udp"        # 非 TCP 跳过
      - "8000-8003:8000-8003" # 范围跳过
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
		t.Errorf("REDIS_HOST（export 前缀+单引号）= %q", m["REDIS_HOST"])
	}
	if v, ok := m["EMPTY_VALUE"]; !ok || v != "" {
		t.Errorf("EMPTY_VALUE 应为存在且为空: %v", m)
	}
	if _, ok := m["BROKEN LINE WITHOUT EQUALS"]; ok {
		t.Errorf("无等号行应被忽略")
	}
}

// mockGitHub emulates the subset of api.github.com used by InspectRepo, with
// a token (contents API form). 404 for unknown paths.
func mockGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	files := map[string]string{
		"docker-compose.yml": "services:\n  app:\n    image: x/y\n    ports:\n      - \"8484:8484\"\n    environment:\n      PORT: \"8484\"\n",
		"Dockerfile":         "FROM alpine\nEXPOSE 8484\nENV CLEWDR_PORT=8484\n",
		".env.example":       "COOKIE=\nPORT=8484\n",
		"go.mod":             "module x/y\n",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/o/r" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/repos/o/r/contents/") {
			path := strings.TrimPrefix(r.URL.Path, "/repos/o/r/contents/")
			if r.URL.Query().Get("ref") != "main" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if content, ok := files[path]; ok {
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

func TestInspectRepoEndToEnd(t *testing.T) {
	srv := mockGitHub(t)
	defer srv.Close()
	withMockHosts(t, srv)

	meta, err := NewClient().InspectRepo(context.Background(), "tok", "o", "r")
	if err != nil {
		t.Fatalf("InspectRepo error: %v", err)
	}
	if !meta.HasCompose || meta.ComposeFile != "docker-compose.yml" {
		t.Errorf("应检测到 compose 文件: %+v", meta)
	}
	if meta.Branch != "main" {
		t.Errorf("Branch = %q, want main", meta.Branch)
	}
	if len(meta.ComposePorts) != 1 || meta.ComposePorts[0].Host != 8484 {
		t.Errorf("ComposePorts = %+v", meta.ComposePorts)
	}
	if meta.ComposeEnv["PORT"] != "8484" {
		t.Errorf("ComposeEnv = %v", meta.ComposeEnv)
	}
	if !meta.HasDockerfile || meta.DockerfilePath != "Dockerfile" {
		t.Errorf("应检测到 Dockerfile: %+v", meta)
	}
	if len(meta.ExposedPorts) != 1 || meta.ExposedPorts[0] != 8484 {
		t.Errorf("ExposedPorts = %v", meta.ExposedPorts)
	}
	if meta.DockerfileEnv["CLEWDR_PORT"] != "8484" {
		t.Errorf("DockerfileEnv = %v", meta.DockerfileEnv)
	}
	if meta.EnvExample["PORT"] != "8484" {
		t.Errorf("EnvExample = %v", meta.EnvExample)
	}
	if meta.Language != "go" {
		t.Errorf("Language = %q, want go", meta.Language)
	}
	if meta.ComposeContent == "" {
		t.Errorf("ComposeContent 应保留原文")
	}
}

func TestStoreInspectRepoRejectsNonGitHub(t *testing.T) {
	st, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := st.InspectRepo(context.Background(), "https://gitlab.com/a/b"); err == nil {
		t.Fatal("非 GitHub 地址应报错")
	}
	if _, err := st.InspectRepo(context.Background(), "not a url"); err == nil {
		t.Fatal("非法 URL 应报错")
	}
}
