package api

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"watchman/server/internal/auth"
	"watchman/server/internal/backup"
)

func TestControlBackupPermissionsAndAsyncLifecycle(t *testing.T) {
	r, authStore, _ := newTestRouter(t)
	admin := tokenFor(t, authStore, "backup_admin", auth.RoleAdmin)
	viewer := tokenFor(t, authStore, "backup_viewer", auth.RoleViewer)
	operator := tokenFor(t, authStore, "backup_operator", auth.RoleOperator)
	call := func(method, path, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	for _, ep := range []struct{ method, path string }{
		{"GET", "/api/v1/system/backups"}, {"GET", "/api/v1/system/backups/status"}, {"GET", "/api/v1/system/backups/policy"},
		{"PUT", "/api/v1/system/backups/policy"}, {"POST", "/api/v1/system/backup"}, {"POST", "/api/v1/system/restore"},
		{"DELETE", "/api/v1/system/restore/pending"}, {"GET", "/api/v1/system/backups/watchman-backup-test.tar.gz/download"},
		{"POST", "/api/v1/system/backups/watchman-backup-test.tar.gz/restore"}, {"DELETE", "/api/v1/system/backups/watchman-backup-test.tar.gz"},
	} {
		for _, token := range []string{viewer, operator, ""} {
			want := 403
			if token == "" {
				want = 401
			}
			if got := call(ep.method, ep.path, token, "{}"); got.Code != want {
				t.Fatalf("%s %s role gate=%d", ep.method, ep.path, got.Code)
			}
		}
	}
	if w := call("PUT", "/api/v1/system/backups/policy", admin, `{"enabled":false,"time":"02:30","retention":3,"target_id":"local"}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := call("POST", "/api/v1/system/backup", admin, `{"async":true,"note":"fixture","target_id":"local"}`); w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	wait := func() backup.Task {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			w := call("GET", "/api/v1/system/backups/status", admin, "")
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			var res struct {
				Data backup.Task `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
				t.Fatal(err)
			}
			if res.Data.Status != "running" {
				return res.Data
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("timeout")
		return backup.Task{}
	}
	status := wait()
	if status.Status != "success" || status.Archive == nil {
		t.Fatalf("backup failed: %+v", status)
	}
	name := status.Archive.Name
	w := call("GET", "/api/v1/system/backups/"+name+"/download?token="+url.QueryEscape(admin), "", "")
	if w.Code != 200 || !bytes.HasPrefix(w.Body.Bytes(), []byte{0x1f, 0x8b}) {
		t.Fatal("authenticated download failed")
	}
	w = call("POST", "/api/v1/system/backups/"+name+"/restore", admin, "{}")
	if w.Code != 202 {
		t.Fatalf("restore rejected: %s", w.Body.String())
	}
	status = wait()
	if status.Status != "ready" || status.Result == nil || !status.Result.Pending {
		t.Fatalf("restore not prepared: %+v", status)
	}
	if w := call("DELETE", "/api/v1/system/restore/pending", admin, ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := call("DELETE", "/api/v1/system/backups/"+name, admin, ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = call("GET", "/api/v1/audit", admin, "")
	for _, action := range []string{"system_backup", "system_restore", "system_restore_cancel", "system_backup_policy", "system_backup_delete"} {
		if !strings.Contains(w.Body.String(), `"action":"`+action+`"`) {
			t.Errorf("audit missing %s", action)
		}
	}
}
