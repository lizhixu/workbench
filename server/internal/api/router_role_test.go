package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"watchman/server/internal/alert"
	"watchman/server/internal/apps"
	"watchman/server/internal/audit"
	"watchman/server/internal/auth"
	"watchman/server/internal/backup"
	"watchman/server/internal/commands"
	"watchman/server/internal/groups"
	"watchman/server/internal/metrics"
	"watchman/server/internal/network"
	"watchman/server/internal/policy"
	"watchman/server/internal/prefs"
	"watchman/server/internal/rpc"
	"watchman/server/internal/scan"
	"watchman/server/internal/session"
	"watchman/server/internal/vault"

	"github.com/gin-gonic/gin"
)

// tokenFor creates a user with the given role and returns a bearer token.
func tokenFor(t *testing.T, store *auth.Store, username string, role auth.Role) string {
	t.Helper()
	if _, err := store.Create(username, "Qa#12345678", role); err != nil {
		t.Fatalf("create %s: %v", username, err)
	}
	tok, _, err := store.Authenticate(username, "Qa#12345678")
	if err != nil {
		t.Fatalf("authenticate %s: %v", username, err)
	}
	return tok
}

// newTestRouter builds a router with every store wired, so all routes register
// and the test exercises real authorization rather than a 404.
func newTestRouter(t *testing.T) (*gin.Engine, *auth.Store) {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("store init: %v", err)
		}
	}
	authStore, err := auth.NewStore(dir, "test-jwt-key")
	must(err)
	sessStore, err := session.NewStore(dir)
	must(err)
	alertStore, err := alert.NewStore(dir)
	must(err)
	vaultStore, err := vault.NewStore(dir, "test-vault-pass")
	must(err)
	metricsStore, err := metrics.NewStore(dir, log)
	must(err)
	scanStore, err := scan.NewStore(dir, log)
	must(err)
	policyStore, err := policy.NewStore(dir, log)
	must(err)
	auditStore, err := audit.NewStore(dir, log)
	must(err)
	commandStore, err := commands.NewStore(dir, log)
	must(err)
	groupStore, err := groups.NewStore(dir, log)
	must(err)
	prefsStore, err := prefs.NewStore(dir, log)
	must(err)
	backupStore, err := backup.NewStore(dir, log)
	must(err)
	networkStore, err := network.NewStore(dir, log)
	must(err)
	appStore, err := apps.NewStore(dir, log)
	must(err)
	appEngine := apps.NewEngine(nil, appStore, vaultStore, log)

	r := Router(rpc.NewRegistry(dir, log), log, authStore, sessStore, alertStore, vaultStore,
		nil, metricsStore, scanStore, policyStore, auditStore, commandStore, groupStore,
		prefsStore, backupStore, networkStore, appStore, appEngine, nil, nil, nil)
	return r, authStore
}

// TestRoleGates locks in the read/write split across roles. A viewer must never
// reach a route that changes a host, and host lifecycle stays admin-only.
func TestRoleGates(t *testing.T) {
	r, authStore := newTestRouter(t)
	viewer := tokenFor(t, authStore, "qa_viewer", auth.RoleViewer)
	operator := tokenFor(t, authStore, "qa_operator", auth.RoleOperator)
	admin := tokenFor(t, authStore, "qa_admin", auth.RoleAdmin)

	// No agent is registered, so a permitted call fails somewhere past
	// authorization (404/503) while a rejected one is stopped with 403.
	const host = "no-such-agent"
	cases := []struct {
		name        string
		method      string
		path        string
		viewerAllow bool
		operAllow   bool
	}{
		{"读取主机列表", "GET", "/api/v1/hosts", true, true},
		{"读取主机详情", "GET", "/api/v1/hosts/" + host, true, true},
		{"读取监控", "GET", "/api/v1/hosts/" + host + "/metrics", true, true},
		{"读取系统状态", "GET", "/api/v1/hosts/" + host + "/sysinfo/process", true, true},
		{"列目录", "GET", "/api/v1/hosts/" + host + "/files?path=/", true, true},
		{"下载文件", "GET", "/api/v1/hosts/" + host + "/files/download?path=/etc/hostname", true, true},
		{"会话列表", "GET", "/api/v1/sessions", true, true},
		{"扫描结果列表", "GET", "/api/v1/scans", true, true},

		{"开终端", "POST", "/api/v1/hosts/" + host + "/terminals", false, true},
		{"执行命令", "POST", "/api/v1/hosts/" + host + "/exec", false, true},
		{"批量推送", "POST", "/api/v1/hosts/batch-exec", false, true},
		{"新建目录", "POST", "/api/v1/hosts/" + host + "/files/mkdir", false, true},
		{"删除文件", "DELETE", "/api/v1/hosts/" + host + "/files?path=/tmp/x", false, true},
		{"上传文件", "POST", "/api/v1/hosts/" + host + "/files/upload?path=/tmp/x", false, true},
		{"结束进程", "POST", "/api/v1/hosts/" + host + "/processes/1/kill", false, true},
		{"Docker 操作", "POST", "/api/v1/hosts/" + host + "/docker/start", false, true},
		{"触发安全扫描", "POST", "/api/v1/hosts/" + host + "/scans", false, true},

		{"解绑主机", "DELETE", "/api/v1/hosts/" + host, false, false},
		{"改主机分组", "PUT", "/api/v1/hosts/" + host + "/group", false, false},
		{"改主机标签", "PUT", "/api/v1/hosts/" + host + "/tags", false, false},
		{"升级 agent", "POST", "/api/v1/hosts/" + host + "/upgrade", false, false},
		{"删除会话录像", "DELETE", "/api/v1/sessions/abc", false, false},
		{"新建用户", "POST", "/api/v1/users", false, false},
		{"读凭据金库", "GET", "/api/v1/vault/credentials", false, false},
		{"读操作审计", "GET", "/api/v1/audit", false, false},
		{"改高危策略", "PUT", "/api/v1/policy/command", false, false},
		{"新建分组", "POST", "/api/v1/groups", false, false},
	}

	for _, tc := range cases {
		for _, actor := range []struct {
			role    string
			token   string
			allowed bool
		}{
			{"viewer", viewer, tc.viewerAllow},
			{"operator", operator, tc.operAllow},
			{"admin", admin, true},
		} {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer "+actor.token)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			forbidden := w.Code == http.StatusForbidden
			switch {
			case actor.allowed && forbidden:
				t.Errorf("%s：%s 应被允许，却返回 403", tc.name, actor.role)
			case !actor.allowed && !forbidden:
				body := strings.TrimSpace(w.Body.String())
				if len(body) > 80 {
					body = body[:80]
				}
				t.Errorf("%s：%s 应被拒绝，却返回 %d %s", tc.name, actor.role, w.Code, body)
			}
		}
	}
}

// An unauthenticated request must never reach a handler.
func TestUnauthenticatedRejected(t *testing.T) {
	r, _ := newTestRouter(t)
	for _, path := range []string{
		"/api/v1/hosts", "/api/v1/sessions", "/api/v1/users", "/api/v1/audit",
		"/api/v1/vault/credentials", "/api/v1/groups", "/api/v1/alerts/rules",
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s 未带令牌应返回 401，实际 %d", path, w.Code)
		}
	}
}

// Login must not leak the stored password hash.
func TestLoginResponseHasNoHash(t *testing.T) {
	r, _ := newTestRouter(t)
	req := httptest.NewRequest("POST", "/api/v1/auth/login",
		strings.NewReader(`{"username":"admin","password":"admin"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("默认 admin 登录失败：%d %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON：%v", err)
	}
	if strings.Contains(w.Body.String(), "$2a$") || strings.Contains(w.Body.String(), "password_hash") {
		t.Errorf("登录响应泄漏口令哈希：%s", w.Body.String())
	}
}

var _ = os.Getenv // keep os imported for future fixtures
