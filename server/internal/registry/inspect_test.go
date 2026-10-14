package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseReference(t *testing.T) {
	cases := []struct {
		in      string
		wantRef Reference
		wantErr bool
	}{
		{"nginx", Reference{"registry-1.docker.io", "library/nginx", "latest"}, false},
		{"nginx:alpine", Reference{"registry-1.docker.io", "library/nginx", "alpine"}, false},
		{"johnsmith/nginx", Reference{"registry-1.docker.io", "johnsmith/nginx", "latest"}, false},
		{"docker.io/library/redis:7.2", Reference{"registry-1.docker.io", "library/redis", "7.2"}, false},
		{"ghcr.io/xerxes-2/clewdr", Reference{"ghcr.io", "xerxes-2/clewdr", "latest"}, false},
		{"ghcr.io/xerxes-2/clewdr:v2", Reference{"ghcr.io", "xerxes-2/clewdr", "v2"}, false},
		{"registry.example.com:5000/app:v1", Reference{"registry.example.com:5000", "app", "v1"}, false},
		{"ghcr.io/Owner/Clewdr", Reference{}, true},                    // 大写仓库名
		{"ghcr.io/a/b@sha256:abc", Reference{}, true},                  // digest 引用
		{"https://ghcr.io/a/b", Reference{}, true},                     // 含 URL scheme
		{"", Reference{}, true},
		{"ghcr.io/", Reference{}, true},
		// SSRF 防护：环回/私网字面地址一律拒绝
		{"127.0.0.1:5000/app:latest", Reference{}, true},
		{"192.168.10.2:5000/app", Reference{}, true},
		{"10.0.0.3/app", Reference{}, true},
		{"localhost:5000/app", Reference{}, true},
		{"[::1]:5000/app", Reference{}, true},
		{"host.internal/app", Reference{}, true},
	}
	for _, c := range cases {
		got, err := ParseReference(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseReference(%q) 应报错，得到 %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseReference(%q) 意外报错: %v", c.in, err)
			continue
		}
		if got != c.wantRef {
			t.Errorf("ParseReference(%q) = %+v, want %+v", c.in, got, c.wantRef)
		}
	}
}

func TestLooksLikeImageRef(t *testing.T) {
	positive := []string{
		"ghcr.io/xerxes-2/clewdr",
		"ghcr.io/xerxes-2/clewdr:latest",
		"johnsmith/nginx:1.25",
		"registry.example.com:5000/app:v1",
		`"ghcr.io/foo/bar"`,
	}
	negative := []string{
		"", "nginx", "redis:alpine", // 无斜杠，歧义太大不识别
		"12:30", "hello", "johnsmith/nginx", // 无 tag 的简易路径不识别
		"https://github.com/a/b", // 这是仓库链接不是镜像
	}
	for _, s := range positive {
		if !LooksLikeImageRef(s) {
			t.Errorf("LooksLikeImageRef(%q) = false, want true", s)
		}
	}
	for _, s := range negative {
		if LooksLikeImageRef(s) {
			t.Errorf("LooksLikeImageRef(%q) = true, want false", s)
		}
	}
}

// mockRegistry serves a minimal Registry V2 surface covering the auth
// handshake, a single-arch manifest and a manifest list scenario.
func mockRegistry(t *testing.T, useIndex bool) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"test-token"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v2/xerxes-2/clewdr/manifests/latest":
			if r.Header.Get("Authorization") != "Bearer test-token" {
				realm := "http://" + srv.Listener.Addr().String() + "/token"
				w.Header().Set("Www-Authenticate", `Bearer realm="`+realm+`",service="mock",scope="repository:xerxes-2/clewdr:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if useIndex {
				w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
				_, _ = w.Write([]byte(`{
					"schemaVersion": 2,
					"mediaType": "application/vnd.oci.image.index.v1+json",
					"manifests": [
						{"digest":"sha256:attestation","platform":{"architecture":"unknown","os":"unknown"}},
						{"digest":"sha256:arm64","platform":{"architecture":"arm64","os":"linux"}},
						{"digest":"sha256:amd64","platform":{"architecture":"amd64","os":"linux"}}
					]
				}`))
				return
			}
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			_, _ = w.Write([]byte(`{
				"schemaVersion": 2,
				"mediaType": "application/vnd.docker.distribution.manifest.v2+json",
				"config": {"mediaType":"application/vnd.docker.container.image.v1+json","digest":"sha256:cfg"}
			}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v2/xerxes-2/clewdr/manifests/sha256:amd64":
			if r.Header.Get("Authorization") != "Bearer test-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = w.Write([]byte(`{
				"schemaVersion": 2,
				"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"config": {"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:cfg"}
			}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v2/xerxes-2/clewdr/blobs/sha256:cfg":
			w.Header().Set("Content-Type", "application/vnd.docker.container.image.v1+json")
			_, _ = w.Write([]byte(`{
				"architecture": "amd64",
				"os": "linux",
				"config": {
					"ExposedPorts": {"8484/tcp": {}, "9/udp": {}},
					"Env": ["PATH=/usr/bin","CLEWDR_PORT=8484","CLEWDR_IP=0.0.0.0","SSL_CERT_FILE=/etc/ssl/ca.crt"],
					"Volumes": {"/data": {}},
					"Cmd": ["/usr/local/bin/clewdr","--config","/etc/clewdr/clewdr.toml"],
					"Entrypoint": null,
					"WorkingDir": "/app"
				}
			}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return srv
}

func TestInspectAgainstMockSingleArch(t *testing.T) {
	srv := mockRegistry(t, false)
	defer srv.Close()

	c := &Client{Scheme: "http"}
	ref := Reference{Registry: srv.Listener.Addr().String(), Repository: "xerxes-2/clewdr", Tag: "latest"}
	meta, err := c.inspectAgainst(context.Background(), ref, ref.Registry)
	if err != nil {
		t.Fatalf("inspectAgainst error: %v", err)
	}
	if meta.OS != "linux" || meta.Architecture != "amd64" {
		t.Errorf("platform = %s/%s, want linux/amd64", meta.OS, meta.Architecture)
	}
	if len(meta.ExposedPorts) != 1 || meta.ExposedPorts[0] != 8484 {
		t.Errorf("ExposedPorts = %v, want [8484] (udp 已过滤)", meta.ExposedPorts)
	}
	if got := meta.EnvVars["CLEWDR_PORT"]; got != "8484" {
		t.Errorf("EnvVars[CLEWDR_PORT] = %q, want 8484", got)
	}
	if _, ok := meta.EnvVars["PATH"]; ok {
		t.Errorf("系统变量 PATH 未被过滤")
	}
	if _, ok := meta.EnvVars["SSL_CERT_FILE"]; ok {
		t.Errorf("系统变量 SSL_CERT_FILE 未被过滤")
	}
	if len(meta.Volumes) != 1 || meta.Volumes[0] != "/data" {
		t.Errorf("Volumes = %v, want [/data]", meta.Volumes)
	}
	if len(meta.Cmd) != 3 || meta.Cmd[0] != "/usr/local/bin/clewdr" {
		t.Errorf("Cmd = %v", meta.Cmd)
	}
	if meta.WorkingDir != "/app" {
		t.Errorf("WorkingDir = %q, want /app", meta.WorkingDir)
	}
}

func TestInspectAgainstMockIndexPicksAmd64(t *testing.T) {
	srv := mockRegistry(t, true)
	defer srv.Close()

	c := &Client{Scheme: "http"}
	ref := Reference{Registry: srv.Listener.Addr().String(), Repository: "xerxes-2/clewdr", Tag: "latest"}
	meta, err := c.inspectAgainst(context.Background(), ref, ref.Registry)
	if err != nil {
		t.Fatalf("inspectAgainst(index) error: %v", err)
	}
	if meta.Architecture != "amd64" {
		t.Errorf("应从 index 中挑选 amd64 平台，得到 %s", meta.Architecture)
	}
	if len(meta.ExposedPorts) != 1 || meta.ExposedPorts[0] != 8484 {
		t.Errorf("ExposedPorts = %v, want [8484]", meta.ExposedPorts)
	}
}

func TestInspectNotFound(t *testing.T) {
	srv := mockRegistry(t, false)
	defer srv.Close()

	c := &Client{Scheme: "http"}
	ref := Reference{Registry: srv.Listener.Addr().String(), Repository: "not/exist", Tag: "v9"}
	_, err := c.inspectAgainst(context.Background(), ref, ref.Registry)
	if err == nil {
		t.Fatal("应对不存在镜像报错")
	}
}
