package githubapp

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

type fakePushTrigger struct {
	calls   [][2]string
	results []TriggerResult
}

func (f *fakePushTrigger) TriggerPush(repo, branch string) []TriggerResult {
	f.calls = append(f.calls, [2]string{repo, branch})
	return f.results
}

func webhookRequest(t *testing.T, store *Store, trigger PushTrigger, event, delivery, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/github-app/webhook", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hmacHex("secret", []byte(body)))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	c.Request = req
	h := NewHandlers(store, NewClient(), nil, nil, trigger)
	h.webhook(c)
	return w
}

func webhookStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetConfig(&Config{AppID: 1, Slug: "test", PrivateKeyPEM: testPrivateKey(t), WebhookSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	return store
}

// Tag pushes must not be mistaken for a branch push of the same name.
func TestWebhookPushSkipsTags(t *testing.T) {
	trigger := &fakePushTrigger{}
	body := `{"ref":"refs/tags/main","repository":{"full_name":"Acme/Web"},"installation":{"id":11}}`
	w := webhookRequest(t, webhookStore(t), trigger, "push", "d-tag", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not a branch push") {
		t.Fatalf("tag push should be skipped, got: %s", w.Body.String())
	}
	if len(trigger.calls) != 0 {
		t.Fatalf("trigger must not run for tag pushes, got %v", trigger.calls)
	}
}

func TestWebhookPushTriggersBranch(t *testing.T) {
	trigger := &fakePushTrigger{results: []TriggerResult{{AppID: "a1", DeploymentID: "dep-1"}}}
	body := `{"ref":"refs/heads/main","repository":{"full_name":"Acme/Web"},"installation":{"id":11}}`
	w := webhookRequest(t, webhookStore(t), trigger, "push", "d-branch", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(trigger.calls) != 1 || trigger.calls[0] != [2]string{"acme/web", "main"} {
		t.Fatalf("trigger should run once for acme/web main, got %v", trigger.calls)
	}
	if !strings.Contains(w.Body.String(), "dep-1") {
		t.Fatalf("response should carry the deployment id, got: %s", w.Body.String())
	}
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"ref":"refs/heads/main","repository":{"full_name":"Acme/Web"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/github-app/webhook", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", "d-bad")
	c.Request = req
	h := NewHandlers(webhookStore(t), NewClient(), nil, nil, &fakePushTrigger{})
	h.webhook(c)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}
