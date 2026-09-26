package alert

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Platform identifies the target webhook receiver service.
type Platform string

const (
	PlatformDingTalk Platform = "dingtalk"
	PlatformWeCom    Platform = "wecom"
	PlatformFeishu   Platform = "feishu"
	PlatformGeneric  Platform = "generic"
)

// DetectPlatform identifies the webhook target from its endpoint URL.
func DetectPlatform(rawURL string) Platform {
	lower := strings.ToLower(rawURL)
	switch {
	case strings.Contains(lower, "dingtalk.com"):
		return PlatformDingTalk
	case strings.Contains(lower, "weixin.qq.com") || strings.Contains(lower, "work.weixin.qq.com"):
		return PlatformWeCom
	case strings.Contains(lower, "feishu.cn") || strings.Contains(lower, "larksuite.com"):
		return PlatformFeishu
	default:
		return PlatformGeneric
	}
}

// SendWebhook dispatches an alert notification event to the configured Webhook.
// It automatically formats the payload for DingTalk, WeChat Work, Feishu, or
// generic endpoints, including cryptographic signatures where required.
func SendWebhook(ctx context.Context, cfg WebhookConfig, event *Event) (int, string, error) {
	if !cfg.Enabled && event.RuleID != "__test__" {
		return 0, "", fmt.Errorf("webhook 未启用")
	}
	targetURL := strings.TrimSpace(cfg.URL)
	if targetURL == "" {
		return 0, "", fmt.Errorf("webhook URL 不能为空")
	}

	platform := DetectPlatform(targetURL)
	secret := strings.TrimSpace(cfg.Secret)

	var (
		payloadBytes []byte
		finalURL     = targetURL
		headers      = make(map[string]string)
	)

	headers["Content-Type"] = "application/json"

	switch platform {
	case PlatformDingTalk:
		finalURL, payloadBytes = buildDingTalkPayload(targetURL, secret, event)

	case PlatformWeCom:
		payloadBytes = buildWeComPayload(event)

	case PlatformFeishu:
		payloadBytes = buildFeishuPayload(secret, event)

	default: // PlatformGeneric
		if secret != "" {
			headers["X-Webhook-Secret"] = secret
		}
		raw := map[string]any{
			"event":     event,
			"timestamp": time.Now().Format(time.RFC3339),
			"version":   "watchman-v1",
		}
		payloadBytes, _ = json.Marshal(raw)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", finalURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return 0, "", fmt.Errorf("创建 HTTP 请求失败: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("网络请求失败: %w", err)
	}
	defer resp.Body.Close()

	bodyData, _ := io.ReadAll(resp.Body)
	respStr := strings.TrimSpace(string(bodyData))

	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, respStr, fmt.Errorf("对端返回 HTTP %d: %s", resp.StatusCode, respStr)
	}

	// For DingTalk: verify errcode in JSON body
	if platform == PlatformDingTalk {
		var dtResp struct {
			ErrCode int    `json:"errcode"`
			ErrMsg  string `json:"errmsg"`
		}
		if err := json.Unmarshal(bodyData, &dtResp); err == nil && dtResp.ErrCode != 0 {
			return resp.StatusCode, respStr, fmt.Errorf("钉钉机器人返回错误 [%d]: %s", dtResp.ErrCode, dtResp.ErrMsg)
		}
	}

	// For WeCom: verify errcode in JSON body
	if platform == PlatformWeCom {
		var wcResp struct {
			ErrCode int    `json:"errcode"`
			ErrMsg  string `json:"errmsg"`
		}
		if err := json.Unmarshal(bodyData, &wcResp); err == nil && wcResp.ErrCode != 0 {
			return resp.StatusCode, respStr, fmt.Errorf("企业微信机器人返回错误 [%d]: %s", wcResp.ErrCode, wcResp.ErrMsg)
		}
	}

	// For Feishu: verify StatusCode/code
	if platform == PlatformFeishu {
		var fsResp struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		if err := json.Unmarshal(bodyData, &fsResp); err == nil && fsResp.Code != 0 {
			return resp.StatusCode, respStr, fmt.Errorf("飞书机器人返回错误 [%d]: %s", fsResp.Code, fsResp.Msg)
		}
	}

	return resp.StatusCode, respStr, nil
}

// buildDingTalkPayload signs the URL if secret is provided, and formats Markdown.
func buildDingTalkPayload(rawURL, secret string, e *Event) (string, []byte) {
	finalURL := rawURL
	if secret != "" {
		timestamp := fmt.Sprintf("%d", time.Now().UnixMilli())
		stringToSign := fmt.Sprintf("%s\n%s", timestamp, secret)
		h := hmac.New(sha256.New, []byte(secret))
		h.Write([]byte(stringToSign))
		sign := url.QueryEscape(base64.StdEncoding.EncodeToString(h.Sum(nil)))

		sep := "&"
		if !strings.Contains(finalURL, "?") {
			sep = "?"
		}
		finalURL = fmt.Sprintf("%s%stimestamp=%s&sign=%s", finalURL, sep, timestamp, sign)
	}

	sevIcon := "🔔"
	sevText := "INFO 信息"
	switch e.Severity {
	case SeverityCritical:
		sevIcon = "🚨"
		sevText = "CRITICAL 严重"
	case SeverityWarning:
		sevIcon = "⚠️"
		sevText = "WARNING 警告"
	}

	hostname := e.Hostname
	if hostname == "" {
		hostname = e.HostID
	}
	if hostname == "" {
		hostname = "系统全局"
	}

	firedTime := e.FiredAt.Format("2006-01-02 15:04:05")
	if e.FiredAt.IsZero() {
		firedTime = time.Now().Format("2006-01-02 15:04:05")
	}

	var sb strings.Builder
	title := fmt.Sprintf("[%s] %s: %s", sevIcon, e.RuleName, hostname)
	sb.WriteString(fmt.Sprintf("### %s %s\n\n", sevIcon, e.RuleName))
	sb.WriteString(fmt.Sprintf("- **告警级别**: %s\n", sevText))
	sb.WriteString(fmt.Sprintf("- **关联主机**: `%s`\n", hostname))
	sb.WriteString(fmt.Sprintf("- **详情说明**: %s\n", e.Message))
	sb.WriteString(fmt.Sprintf("- **触发时间**: %s\n", firedTime))

	if e.AIInterpretation != "" {
		sb.WriteString(fmt.Sprintf("\n> 💡 **AI 智能分析与排障建议**:\n> %s\n", strings.ReplaceAll(e.AIInterpretation, "\n", "\n> ")))
	}

	body, _ := json.Marshal(map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]string{
			"title": title,
			"text":  sb.String(),
		},
	})
	return finalURL, body
}

// buildWeComPayload formats a WeChat Work robot Markdown payload.
func buildWeComPayload(e *Event) []byte {
	sevColor := "comment"
	sevText := "信息 (Info)"
	switch e.Severity {
	case SeverityCritical:
		sevColor = "warning"
		sevText = "严重 (Critical)"
	case SeverityWarning:
		sevColor = "warning"
		sevText = "警告 (Warning)"
	}

	hostname := e.Hostname
	if hostname == "" {
		hostname = e.HostID
	}
	if hostname == "" {
		hostname = "系统全局"
	}

	firedTime := e.FiredAt.Format("2006-01-02 15:04:05")
	if e.FiredAt.IsZero() {
		firedTime = time.Now().Format("2006-01-02 15:04:05")
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("### 🛡️ Watchman 告警通知\n"))
	sb.WriteString(fmt.Sprintf("> 规则名称: <font color=\"%s\">%s</font>\n", sevColor, e.RuleName))
	sb.WriteString(fmt.Sprintf("> 告警级别: <font color=\"%s\">%s</font>\n", sevColor, sevText))
	sb.WriteString(fmt.Sprintf("> 关联主机: <font color=\"info\">%s</font>\n", hostname))
	sb.WriteString(fmt.Sprintf("> 告警详情: %s\n", e.Message))
	sb.WriteString(fmt.Sprintf("> 触发时间: %s\n", firedTime))

	if e.AIInterpretation != "" {
		sb.WriteString(fmt.Sprintf("\n💡 **AI 智能建议**:\n%s\n", e.AIInterpretation))
	}

	body, _ := json.Marshal(map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]string{
			"content": sb.String(),
		},
	})
	return body
}

// buildFeishuPayload formats Feishu custom robot payload.
func buildFeishuPayload(secret string, e *Event) []byte {
	hostname := e.Hostname
	if hostname == "" {
		hostname = e.HostID
	}
	if hostname == "" {
		hostname = "系统全局"
	}

	firedTime := e.FiredAt.Format("2006-01-02 15:04:05")
	if e.FiredAt.IsZero() {
		firedTime = time.Now().Format("2006-01-02 15:04:05")
	}

	text := fmt.Sprintf("🛡️ Watchman 告警通知\n------------------------\n【规则名称】%s\n【告警级别】%s\n【关联主机】%s\n【告警详情】%s\n【触发时间】%s",
		e.RuleName, e.Severity, hostname, e.Message, firedTime)
	if e.AIInterpretation != "" {
		text += fmt.Sprintf("\n【AI建议】%s", e.AIInterpretation)
	}

	msg := map[string]any{
		"msg_type": "text",
		"content": map[string]string{
			"text": text,
		},
	}

	if secret != "" {
		timestamp := time.Now().Unix()
		stringToSign := fmt.Sprintf("%d\n%s", timestamp, secret)
		h := hmac.New(sha256.New, []byte(stringToSign))
		sign := base64.StdEncoding.EncodeToString(h.Sum(nil))
		msg["timestamp"] = fmt.Sprintf("%d", timestamp)
		msg["sign"] = sign
	}

	body, _ := json.Marshal(msg)
	return body
}
