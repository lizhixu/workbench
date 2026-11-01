package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListModelsOpenAI(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o-mini"},{"id":"gpt-4o"},{"id":"gpt-4o-mini"},{"id":""}]}`))
	}))
	defer srv.Close()

	models, err := (&Assistant{}).ListModels(context.Background(), Config{BaseURL: srv.URL, APIKey: "sk-test"})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if gotPath != "/v1/models" {
		t.Errorf("path = %q, want /v1/models", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("auth = %q, want Bearer sk-test", gotAuth)
	}
	want := []string{"gpt-4o-mini", "gpt-4o"}
	if len(models) != len(want) {
		t.Fatalf("models = %v, want %v", models, want)
	}
	for i := range want {
		if models[i] != want[i] {
			t.Errorf("models[%d] = %q, want %q", i, models[i], want[i])
		}
	}
}

func TestListModelsOpenAIV1AndChatURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	for _, base := range []string{srv.URL + "/v1", srv.URL + "/v1/chat/completions"} {
		if _, err := (&Assistant{}).ListModels(context.Background(), Config{BaseURL: base}); err != nil {
			t.Fatalf("ListModels(%q): %v", base, err)
		}
		// Both spellings normalize to the same /v1/models endpoint.
		if gotPath != "/v1/models" {
			t.Errorf("base %q: path = %q, want /v1/models", base, gotPath)
		}
	}
}

func TestListModelsOllama(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"models":[{"name":"qwen2.5:14b"},{"name":"llama3:8b"}]}`))
	}))
	defer srv.Close()

	// The /api/generate suffix is the Ollama branch of the same detection
	// callLLMWithConfig uses (an httptest URL never contains :11434).
	models, err := (&Assistant{}).ListModels(context.Background(), Config{BaseURL: srv.URL + "/api/generate"})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if gotPath != "/api/tags" {
		t.Errorf("path = %q, want /api/tags", gotPath)
	}
	if len(models) != 2 || models[0] != "qwen2.5:14b" || models[1] != "llama3:8b" {
		t.Errorf("models = %v", models)
	}
}

func TestListModelsErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusUnauthorized, `{"error":{"message":"Incorrect API key"}}`, "API Key 无效"},
		{http.StatusForbidden, `{}`, "API Key 无效"},
		{http.StatusNotFound, `{}`, "不支持模型列表"},
		{http.StatusBadGateway, `{"error":{"message":"upstream down"}}`, "HTTP 502"},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, err := (&Assistant{}).ListModels(context.Background(), Config{BaseURL: srv.URL})
		srv.Close()
		if err == nil {
			t.Errorf("status %d: want error", tc.status)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("status %d: error = %q, want it to contain %q", tc.status, err.Error(), tc.want)
		}
	}
}

func TestListModelsEmptyBaseURL(t *testing.T) {
	if _, err := (&Assistant{}).ListModels(context.Background(), Config{}); err == nil {
		t.Error("want error for empty BaseURL")
	}
}
