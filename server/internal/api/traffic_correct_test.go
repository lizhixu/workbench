package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"watchman/proto/agentpb"
	"watchman/server/internal/rpc"

	"github.com/gin-gonic/gin"
)

// POST /api/v1/hosts/:id/traffic/correct: one-time manual override of the
// host's monthly traffic totals. Validation (malformed JSON, negative,
// absurd GB) is 400; an unknown host is 404; a known but offline agent
// (registered, no live stream) is 503 and the correction is not applied.
// The happy path (online agent) needs a live gRPC stream, so it is covered
// by the agent-side setCorrection test instead.
func TestCorrectHostTraffic(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := rpc.NewRegistry(dir, log)
	h := &handlers{reg: reg}

	call := func(hostID, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/hosts/"+hostID+"/traffic/correct", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Params = gin.Params{gin.Param{Key: "id", Value: hostID}}
		h.correctHostTraffic(c)
		return w
	}

	// Validation errors come before the host lookup.
	if w := call("no-such-host", `{"rx_gb": -1, "tx_gb": 1}`); w.Code != http.StatusBadRequest {
		t.Errorf("negative rx_gb should be 400, got %d %s", w.Code, w.Body.String())
	}
	if w := call("no-such-host", `{"rx_gb": 1, "tx_gb": 2097152}`); w.Code != http.StatusBadRequest {
		t.Errorf("absurd tx_gb should be 400, got %d %s", w.Code, w.Body.String())
	}
	if w := call("no-such-host", `not json`); w.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON should be 400, got %d %s", w.Code, w.Body.String())
	}

	// Unknown host.
	if w := call("no-such-host", `{"rx_gb": 22.6, "tx_gb": 4.8}`); w.Code != http.StatusNotFound {
		t.Errorf("unknown host should be 404, got %d %s", w.Code, w.Body.String())
	}

	// Known host, offline agent: registered via an enroll token, but no
	// live stream is attached, so the push fails and the API must say so
	// instead of reporting success.
	tok := reg.IssueEnrollToken()
	if _, _, err := reg.Register(context.Background(), &agentpb.RegisterRequest{
		EnrollToken: tok,
		AgentId:     "offline-host",
		Hostname:    "offline-host",
	}, ""); err != nil {
		t.Fatalf("register offline host: %v", err)
	}
	if w := call("offline-host", `{"rx_gb": 22.6, "tx_gb": 4.8}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("offline agent should be 503, got %d %s", w.Code, w.Body.String())
	}
}
