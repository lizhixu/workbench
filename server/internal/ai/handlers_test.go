package ai

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestWriteAIErrorWithTrace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	trace := &CallTrace{
		Steps: []CallStep{
			{Key: "facts", Label: "事实探测", Status: "done", DurationMs: 5, Detail: "无事实可探测"},
			{Key: "llm", Label: "模型调用", Status: "failed", DurationMs: 12, Detail: "boom"},
		},
		TotalMs: 17,
	}
	writeAIError(c, &TracedError{Trace: trace, Err: errors.New("boom")})

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	var body struct {
		Error string     `json:"error"`
		Trace *CallTrace `json:"trace"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error != "boom" {
		t.Fatalf("error = %q, want boom", body.Error)
	}
	if body.Trace == nil || len(body.Trace.Steps) != 2 || body.Trace.Steps[1].Status != "failed" {
		t.Fatalf("trace = %+v, want 2 steps with failed llm step", body.Trace)
	}
}

func TestWriteAIErrorPlain(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	writeAIError(c, errors.New("plain failure"))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "plain failure" {
		t.Fatalf("error = %v, want plain failure", body["error"])
	}
	if _, ok := body["trace"]; ok {
		t.Fatalf("plain error must not carry a trace, got %v", body["trace"])
	}
}

// A TracedError without a trace must fall back to the plain shape: the
// response must never leak a trace:null key.
func TestWriteAIErrorTracedWithoutTrace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	writeAIError(c, &TracedError{Trace: nil, Err: errors.New("no trace")})

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "no trace" {
		t.Fatalf("error = %v, want no trace", body["error"])
	}
	if _, ok := body["trace"]; ok {
		t.Fatalf("nil trace must not be serialized, got %v", body["trace"])
	}
}
