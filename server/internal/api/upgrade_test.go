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

func TestUpgradeHostDTOStateOnError(t *testing.T) {
	log := slog.Default()
	reg := rpc.NewRegistry("", log)

	req := &agentpb.RegisterRequest{
		EnrollToken:  "test-enroll",
		Hostname:     "error-box",
		Os:           "linux",
		Arch:         "amd64",
		AgentVersion: "0.0.1",
	}
	token := reg.IssueEnrollToken()
	req.EnrollToken = token
	_, resp, err := reg.Register(context.Background(), req, "")
	if err != nil || !resp.GetOk() {
		t.Fatalf("register failed: %v", err)
	}

	authStore, err := auth.NewStore(t.TempDir(), "test-jwt-key")
	if err != nil {
		t.Fatalf("auth store: %v", err)
	}
	adminToken, _, err := authStore.Authenticate("admin", "admin")
	if err != nil {
		t.Fatalf("auth: %v", err)
	}

	router := Router(reg, log, authStore, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	// 1. Mark in-progress
	reg.SetAgentUpgrading(resp.GetAgentId(), "v1.0.0")
	reg.SetAgentUpgradeProgress(resp.GetAgentId(), "downloading", "")

	httpReq := httptest.NewRequest("GET", "/api/v1/hosts/"+resp.GetAgentId(), nil)
	httpReq.Header.Set("Authorization", "Bearer "+adminToken)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httpReq)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var res map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	data := res["data"].(map[string]any)
	if data["upgrading"] != true {
		t.Errorf("expected upgrading=true while downloading, got %v", data["upgrading"])
	}
	if data["upgrade_stage"] != "downloading" {
		t.Errorf("expected stage=downloading, got %v", data["upgrade_stage"])
	}
	if data["agent_outdated"] != true {
		t.Errorf("expected agent_outdated=true for version 0.0.1, got %v", data["agent_outdated"])
	}
	if data["agent_latest_version"] != CurrentAgentVersion {
		t.Errorf("expected agent_latest_version=%s, got %v", CurrentAgentVersion, data["agent_latest_version"])
	}

	// 2. Mark error
	reg.SetAgentUpgradeProgress(resp.GetAgentId(), "error", "open binary: text file busy")
	w2 := httptest.NewRecorder()
	httpReq2 := httptest.NewRequest("GET", "/api/v1/hosts/"+resp.GetAgentId(), nil)
	httpReq2.Header.Set("Authorization", "Bearer "+adminToken)
	router.ServeHTTP(w2, httpReq2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w2.Code)
	}
	var res2 map[string]any
	_ = json.Unmarshal(w2.Body.Bytes(), &res2)
	data2 := res2["data"].(map[string]any)
	if data2["upgrading"] == true {
		t.Errorf("expected upgrading=false when stage is error, got %v", data2["upgrading"])
	}
	if data2["upgrade_stage"] != "error" {
		t.Errorf("expected stage=error, got %v", data2["upgrade_stage"])
	}
	if data2["upgrade_error"] != "open binary: text file busy" {
		t.Errorf("expected upgrade_error to match, got %v", data2["upgrade_error"])
	}
}

func TestSystemUpgrade_RejectsCorruptBinary(t *testing.T) {
	log := slog.Default()
	reg := rpc.NewRegistry("", log)

	authStore, err := auth.NewStore(t.TempDir(), "test-jwt-key")
	if err != nil {
		t.Fatalf("auth store: %v", err)
	}
	adminToken, _, err := authStore.Authenticate("admin", "admin")
	if err != nil {
		t.Fatalf("auth: %v", err)
	}

	router := Router(reg, log, authStore, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	// Send base64 corrupt data
	body := `{"data":"bm90IGFuIGV4ZWN1dGFibGU="}` // "not an executable"
	httpReq := httptest.NewRequest("POST", "/api/v1/system/upgrade", strings.NewReader(body))
	httpReq.Header.Set("Authorization", "Bearer "+adminToken)
	httpReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, httpReq)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for corrupt binary, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "安全校验失败") && !strings.Contains(w.Body.String(), "低于安全下限") {
		t.Errorf("expected security validation failure in response, got %s", w.Body.String())
	}
}

func TestSystemUpgrade_RejectsShaMismatch(t *testing.T) {
	log := slog.Default()
	reg := rpc.NewRegistry("", log)

	authStore, err := auth.NewStore(t.TempDir(), "test-jwt-key")
	if err != nil {
		t.Fatalf("auth store: %v", err)
	}
	adminToken, _, err := authStore.Authenticate("admin", "admin")
	if err != nil {
		t.Fatalf("auth: %v", err)
	}

	router := Router(reg, log, authStore, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	body := `{"data":"bm90IGFuIGV4ZWN1dGFibGU=","sha256":"1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff"}`
	httpReq := httptest.NewRequest("POST", "/api/v1/system/upgrade", strings.NewReader(body))
	httpReq.Header.Set("Authorization", "Bearer "+adminToken)
	httpReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, httpReq)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for sha mismatch, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "SHA-256 校验失败") {
		t.Errorf("expected SHA-256 mismatch in response, got %s", w.Body.String())
	}
}
