package ai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCallTracerRun(t *testing.T) {
	tr := newCallTracer()
	if _, err := tr.run("a", "第一步", func() (string, error) {
		return "ok", nil
	}); err != nil {
		t.Fatalf("run ok step: %v", err)
	}
	sentinel := errors.New("boom")
	if _, err := tr.run("b", "第二步", func() (string, error) {
		return "", sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("run failed step should return its error, got %v", err)
	}

	trace := tr.trace()
	if len(trace.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(trace.Steps))
	}
	if trace.Steps[0].Status != "done" || trace.Steps[0].Detail != "ok" {
		t.Fatalf("first step = %+v, want done/ok", trace.Steps[0])
	}
	if trace.Steps[1].Status != "failed" || trace.Steps[1].Detail != "boom" {
		t.Fatalf("failed step detail should fall back to the error, got %+v", trace.Steps[1])
	}
	if trace.TotalMs < 0 {
		t.Fatalf("total_ms = %d, want >= 0", trace.TotalMs)
	}
}

func TestCallOpenAIUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}],` +
			`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer srv.Close()

	text, usage, err := callOpenAI(context.Background(), Config{Model: "test"}, srv.URL, "hello")
	if err != nil {
		t.Fatalf("callOpenAI: %v", err)
	}
	if text != "hi" {
		t.Fatalf("text = %q, want hi", text)
	}
	if usage == nil || usage.PromptTokens != 10 || usage.CompletionTokens != 5 || usage.TotalTokens != 15 {
		t.Fatalf("usage = %+v, want 10/5/15", usage)
	}
	if got := usage.summary(); !strings.Contains(got, "10") || !strings.Contains(got, "5") {
		t.Fatalf("summary = %q", got)
	}
}

func TestCallOllamaUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":"hi","prompt_eval_count":7,"eval_count":3}`))
	}))
	defer srv.Close()

	text, usage, err := callOllama(context.Background(), Config{Model: "test"}, srv.URL, "hello")
	if err != nil {
		t.Fatalf("callOllama: %v", err)
	}
	if text != "hi" {
		t.Fatalf("text = %q, want hi", text)
	}
	if usage == nil || usage.PromptTokens != 7 || usage.CompletionTokens != 3 {
		t.Fatalf("usage = %+v, want 7/3", usage)
	}
}

func TestSummarizeFacts(t *testing.T) {
	if got := summarizeFacts(nil); got != "未执行事实探测" {
		t.Fatalf("nil facts = %q", got)
	}
	if got := summarizeFacts(&appFacts{}); !strings.Contains(got, "无事实可探测") {
		t.Fatalf("empty facts = %q", got)
	}
	facts := &appFacts{
		image: &AppImageProbe{
			Reference: "redis:7",
			Ports:     []int{6379},
			EnvVars:   map[string]string{"A": "1", "B": "2"},
			Volumes:   []string{"/data"},
		},
		repo: &AppRepoProbe{
			RepoURL:       "https://github.com/x/y",
			Branch:        "main",
			HasDockerfile: true,
		},
	}
	got := summarizeFacts(facts)
	if !strings.Contains(got, "1 个端口") || !strings.Contains(got, "2 个环境变量") ||
		!strings.Contains(got, "1 个挂载卷") || !strings.Contains(got, "Dockerfile") {
		t.Fatalf("summarizeFacts = %q", got)
	}
}
