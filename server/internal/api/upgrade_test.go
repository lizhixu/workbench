package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"watchman/proto/agentpb"
	"watchman/server/internal/auth"
	"watchman/server/internal/rpc"
)

func TestUpgradeAgentDynamicURLAndDefaults(t *testing.T) {
	log := slog.Default()
	reg := rpc.NewRegistry("", log)

	// 1. Register a Windows ARM64 host
	req := &agentpb.RegisterRequest{
		EnrollToken:  "test-enroll",
		Hostname:     "win-arm-box",
		Os:           "windows",
		Arch:         "arm64",
		AgentVersion: "0.0.1",
	}
	token := reg.IssueEnrollToken()
	req.EnrollToken = token
	hub, resp, err := reg.Register(context.Background(), req, "")
	if err != nil || !resp.GetOk() {
		t.Fatalf("register failed: %v", err)
	}
	hub.MockConnectForTest()

	authStore, err := auth.NewStore(t.TempDir(), "test-jwt-key")
	if err != nil {
		t.Fatalf("auth store: %v", err)
	}
	adminToken, _, err := authStore.Authenticate("admin", "admin")
	if err != nil {
		t.Fatalf("auth: %v", err)
	}

	router := Router(reg, log, authStore, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	// In background, wait for the UpgradeRequest sent to the hub and respond with progress
	var receivedURL string
	var receivedVersion string
	doneCh := make(chan struct{})

	go func() {
		defer close(doneCh)
		msg, ok := hub.RecvForTest(2 * time.Second)
		if !ok {
			return
		}
		if up := msg.GetUpgrade(); up != nil {
			receivedURL = up.GetUrl()
			receivedVersion = up.GetVersion()
			// Send back progress to satisfy the streaming handler
			hub.DispatchRespForTest("upgrade", &agentpb.AgentMessage{
				Payload: &agentpb.AgentMessage_UpgradeProgress{
					UpgradeProgress: &agentpb.UpgradeProgress{
						Stage:    "restarting",
						Progress: 1.0,
					},
				},
			})
		}
	}()

	// Call POST /api/v1/hosts/:id/upgrade without version (should default to CurrentAgentVersion)
	httpReq := httptest.NewRequest("POST", "/api/v1/hosts/"+resp.GetAgentId()+"/upgrade", strings.NewReader("{}"))
	httpReq.Header.Set("Authorization", "Bearer "+adminToken)
	httpReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, httpReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", w.Code, w.Body.String())
	}

	<-doneCh

	if receivedVersion != CurrentAgentVersion {
		t.Errorf("expected version %s, got %s", CurrentAgentVersion, receivedVersion)
	}
	expectedSuffix := "os=windows&arch=arm64"
	if !strings.Contains(receivedURL, expectedSuffix) {
		t.Errorf("expected URL to contain %s, got %s", expectedSuffix, receivedURL)
	}

	var res map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res["ok"] != true {
		t.Errorf("expected ok=true, got %v", res["ok"])
	}
}

func TestUpgradeAgentAttachesSignature(t *testing.T) {
	log := slog.Default()
	reg := rpc.NewRegistry("", log)

	req := &agentpb.RegisterRequest{
		EnrollToken:  "test-enroll",
		Hostname:     "linux-box",
		Os:           "linux",
		Arch:         "amd64",
		AgentVersion: "0.0.1",
	}
	token := reg.IssueEnrollToken()
	req.EnrollToken = token
	hub, resp, err := reg.Register(context.Background(), req, "")
	if err != nil || !resp.GetOk() {
		t.Fatalf("register failed: %v", err)
	}
	hub.MockConnectForTest()

	authStore, err := auth.NewStore(t.TempDir(), "test-jwt-key")
	if err != nil {
		t.Fatalf("auth store: %v", err)
	}
	adminToken, _, err := authStore.Authenticate("admin", "admin")
	if err != nil {
		t.Fatalf("auth: %v", err)
	}

	// Mock UpgradeSigner
	origSigner := UpgradeSigner
	UpgradeSigner = func(version, sha256 string) []byte {
		return []byte("fake-sig-for-" + version + "-" + sha256)
	}
	defer func() { UpgradeSigner = origSigner }()

	router := Router(reg, log, authStore, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	var receivedSig []byte
	var receivedSha string
	doneCh := make(chan struct{})

	go func() {
		defer close(doneCh)
		msg, ok := hub.RecvForTest(2 * time.Second)
		if !ok {
			return
		}
		if up := msg.GetUpgrade(); up != nil {
			receivedSig = up.GetSignature()
			receivedSha = up.GetSha256()
			hub.DispatchRespForTest("upgrade", &agentpb.AgentMessage{
				Payload: &agentpb.AgentMessage_UpgradeProgress{
					UpgradeProgress: &agentpb.UpgradeProgress{
						Stage:    "restarting",
						Progress: 1.0,
					},
				},
			})
		}
	}()

	httpReq := httptest.NewRequest("POST", "/api/v1/hosts/"+resp.GetAgentId()+"/upgrade", strings.NewReader(`{"sha256":"custom-sha-123"}`))
	httpReq.Header.Set("Authorization", "Bearer "+adminToken)
	httpReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, httpReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", w.Code, w.Body.String())
	}

	<-doneCh

	if receivedSha != "custom-sha-123" {
		t.Errorf("expected sha256 custom-sha-123, got %s", receivedSha)
	}
	expectedSig := "fake-sig-for-" + CurrentAgentVersion + "-custom-sha-123"
	if string(receivedSig) != expectedSig {
		t.Errorf("expected signature %s, got %s", expectedSig, string(receivedSig))
	}
}

