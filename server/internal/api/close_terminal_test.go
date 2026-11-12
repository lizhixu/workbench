package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	"watchman/server/internal/auth"
	"watchman/server/internal/ws"
)

// DELETE /api/v1/terminals/:sid：显式强制关闭。只有关闭者本人
// （RegisterSession 里记的操作人）或 admin 能调；未知 sid 404；关闭后映射即
// 清，第二次调 404。未接入过的会话没有 live PTY，closed=false，但映射同样
// 清掉。
func TestCloseTerminal(t *testing.T) {
	r, authStore, _ := newTestRouter(t)
	op := tokenFor(t, authStore, "close_op", auth.RoleOperator)
	other := tokenFor(t, authStore, "close_other", auth.RoleOperator)
	viewer := tokenFor(t, authStore, "close_viewer", auth.RoleViewer)
	admin := tokenFor(t, authStore, "close_admin", auth.RoleAdmin)

	call := func(sid, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("DELETE", "/api/v1/terminals/"+sid, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	ws.RegisterSession("close-sid-1", "agent-x", "close_op", "host-1")
	defer ws.UnregisterSession("close-sid-1")

	if w := call("close-sid-1", ""); w.Code != 401 {
		t.Errorf("无令牌应 401，实际 %d", w.Code)
	}
	if w := call("close-sid-1", viewer); w.Code != 403 {
		t.Errorf("viewer 应 403，实际 %d", w.Code)
	}
	if w := call("close-sid-1", other); w.Code != 403 {
		t.Errorf("非本人非 admin 应 403，实际 %d", w.Code)
	}
	if w := call("close-sid-1", op); w.Code != 200 {
		t.Fatalf("本人关闭应 200，实际 %d %s", w.Code, w.Body.String())
	} else if !strings.Contains(w.Body.String(), `"closed":false`) {
		t.Errorf("未接入的会话 closed 应为 false，实际 %s", w.Body.String())
	}
	if _, ok := ws.LookupSessionInfo("close-sid-1"); ok {
		t.Error("关闭后会话映射应已清除")
	}
	if w := call("close-sid-1", op); w.Code != 404 {
		t.Errorf("第二次关闭应 404，实际 %d", w.Code)
	}
	if w := call("no-such-sid", op); w.Code != 404 {
		t.Errorf("未知 sid 应 404，实际 %d", w.Code)
	}

	// admin 可以关别人的会话。
	ws.RegisterSession("close-sid-2", "agent-x", "close_op", "host-1")
	defer ws.UnregisterSession("close-sid-2")
	if w := call("close-sid-2", admin); w.Code != 200 {
		t.Errorf("admin 关闭应 200，实际 %d %s", w.Code, w.Body.String())
	}
}
