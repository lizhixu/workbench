package alert

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDetectPlatform(t *testing.T) {
	cases := []struct {
		url      string
		expected Platform
	}{
		{"https://oapi.dingtalk.com/robot/send?access_token=xxx", PlatformDingTalk},
		{"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx", PlatformWeCom},
		{"https://work.weixin.qq.com/api/webhook/send", PlatformWeCom},
		{"https://open.feishu.cn/open-apis/bot/v2/hook/xxx", PlatformFeishu},
		{"https://open.larksuite.com/open-apis/bot/v2/hook/xxx", PlatformFeishu},
		{"https://api.example.com/alerts/webhook", PlatformGeneric},
		{"http://127.0.0.1:8080/hook", PlatformGeneric},
	}
	for _, c := range cases {
		if got := DetectPlatform(c.url); got != c.expected {
			t.Errorf("DetectPlatform(%q) = %s, want %s", c.url, got, c.expected)
		}
	}
}

func TestDingTalkPayloadAndSigning(t *testing.T) {
	secret := "SEC_test_secret_123456"
	rawURL := "https://oapi.dingtalk.com/robot/send?access_token=abc"
	event := &Event{
		ID:       "evt-1",
		RuleName: "CPU 使用率过高",
		Severity: SeverityCritical,
		Hostname: "prod-node-01",
		Message:  "CPU 占用 95.8% 持续 60 秒",
		FiredAt:  time.Now(),
	}

	finalURL, bodyBytes := buildDingTalkPayload(rawURL, secret, event)

	// Verify query params exist: timestamp and sign
	u, err := url.Parse(finalURL)
	if err != nil {
		t.Fatalf("parse final url failed: %v", err)
	}
	ts := u.Query().Get("timestamp")
	sign := u.Query().Get("sign")
	if ts == "" || sign == "" {
		t.Fatalf("expected timestamp and sign in query params, got url: %s", finalURL)
	}

	// Verify HMAC signature calculation
	stringToSign := ts + "\n" + secret
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(stringToSign))
	expectedSign := base64.StdEncoding.EncodeToString(h.Sum(nil))
	if sign != expectedSign {
		t.Errorf("signature mismatch: got %q, want %q", sign, expectedSign)
	}

	// Verify JSON body
	var body struct {
		MsgType  string `json:"msgtype"`
		Markdown struct {
			Title string `json:"title"`
			Text  string `json:"text"`
		} `json:"markdown"`
	}
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		t.Fatalf("unmarshal dingtalk body failed: %v", err)
	}
	if body.MsgType != "markdown" {
		t.Errorf("msgtype = %s, want markdown", body.MsgType)
	}
	if !strings.Contains(body.Markdown.Text, "prod-node-01") {
		t.Errorf("markdown text does not contain hostname: %s", body.Markdown.Text)
	}
	if !strings.Contains(body.Markdown.Text, "CPU 使用率过高") {
		t.Errorf("markdown text does not contain rule name: %s", body.Markdown.Text)
	}
}

func TestSendWebhookGeneric(t *testing.T) {
	var receivedHeaders http.Header
	var receivedBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		var err error
		receivedBody, err = ioReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"received": true}`))
	}))
	defer server.Close()

	cfg := WebhookConfig{
		URL:     server.URL,
		Secret:  "custom-token-secret-777",
		Enabled: true,
	}
	event := &Event{
		ID:       "evt-generic",
		RuleName: "主机离线",
		Severity: SeverityWarning,
		Hostname: "test-box",
		Message:  "主机心跳超时",
		FiredAt:  time.Now(),
	}

	status, resp, err := SendWebhook(context.Background(), cfg, event)
	if err != nil {
		t.Fatalf("SendWebhook failed: %v", err)
	}
	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
	if !strings.Contains(resp, "received") {
		t.Errorf("response = %s, want received", resp)
	}
	if receivedHeaders.Get("X-Webhook-Secret") != "custom-token-secret-777" {
		t.Errorf("secret header = %q, want custom-token-secret-777", receivedHeaders.Get("X-Webhook-Secret"))
	}
	if !strings.Contains(string(receivedBody), "test-box") {
		t.Errorf("body does not contain test-box: %s", string(receivedBody))
	}
}

func ioReadAll(r ioReader) ([]byte, error) {
	buf := new(strings.Builder)
	b := make([]byte, 1024)
	for {
		n, err := r.Read(b)
		if n > 0 {
			buf.Write(b[:n])
		}
		if err != nil {
			break
		}
	}
	return []byte(buf.String()), nil
}

type ioReader interface {
	Read(p []byte) (n int, err error)
}
