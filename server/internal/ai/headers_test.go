package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCallOpenAICustomHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hello"}}]}`))
	}))
	defer srv.Close()

	cfg := Config{
		BaseURL: srv.URL,
		Model:   "test-model",
		APIKey:  "sk-default",
		Headers: map[string]string{
			"X-Custom-Key":  "custom-value",
			"Authorization": "ApiKey override-value",
			"  ":           "blank-key-ignored",
			"X-Empty":       "   ",
		},
	}
	out, err := callOpenAI(context.Background(), cfg, srv.URL, "hi")
	if err != nil {
		t.Fatalf("callOpenAI: %v", err)
	}
	if out != "hello" {
		t.Fatalf("got %q, want hello", out)
	}
	if got.Get("X-Custom-Key") != "custom-value" {
		t.Fatalf("X-Custom-Key = %q, want custom-value", got.Get("X-Custom-Key"))
	}
	// Custom Authorization must override the default "Bearer <api_key>".
	if got.Get("Authorization") != "ApiKey override-value" {
		t.Fatalf("Authorization = %q, want override", got.Get("Authorization"))
	}
	if got.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q", got.Get("Content-Type"))
	}
}

func TestCallOpenAINoCustomHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	cfg := Config{BaseURL: srv.URL, Model: "m", APIKey: "sk-123"}
	if _, err := callOpenAI(context.Background(), cfg, srv.URL, "hi"); err != nil {
		t.Fatalf("callOpenAI: %v", err)
	}
	if got.Get("Authorization") != "Bearer sk-123" {
		t.Fatalf("Authorization = %q, want Bearer default", got.Get("Authorization"))
	}
}
