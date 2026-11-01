// Model list probing for the settings page: the operator can pull the
// models an endpoint actually serves and pick one, instead of typing a
// model name blind and finding out it does not exist on the first real
// AI call. This is a read-only diagnostic: it never persists anything and
// never spends a completion, so it stays audit-silent like /ai/test.
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// ListModels returns the model ids served by cfg's endpoint. The endpoint
// kind is detected the same way callLLMWithConfig detects it (Ollama by
// :11434 or /api/generate suffix, everything else as OpenAI-compatible),
// so a list probe and a real call never disagree about which protocol to
// speak. A nil/empty candidate config is the caller's job to merge with
// the saved config first (see Handlers.listModels); ListModels itself
// only requires a non-empty BaseURL.
func (a *Assistant) ListModels(ctx context.Context, cfg Config) ([]string, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("LLM Base URL 不能为空")
	}

	var target string
	var ollama bool
	if strings.Contains(base, ":11434") || strings.HasSuffix(base, "/api/generate") {
		ollama = true
		target = strings.TrimSuffix(base, "/api/generate") + "/api/tags"
	} else {
		target = openAIModelsURL(base)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	applyCustomHeaders(req, cfg)

	// A list call is cheap; 30s is generous without letting a hung
	// endpoint stall the settings page.
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, classifyProbeError(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, classifyProbeStatus(resp.StatusCode, body)
	}

	var models []string
	if ollama {
		var tags struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if err := json.Unmarshal(body, &tags); err != nil {
			return nil, fmt.Errorf("模型列表响应格式无法识别，请手动输入模型名称")
		}
		for _, m := range tags.Models {
			models = append(models, m.Name)
		}
	} else {
		var list struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("模型列表响应格式无法识别，请手动输入模型名称")
		}
		for _, m := range list.Data {
			models = append(models, m.ID)
		}
	}
	return dedupeModels(models), nil
}

// openAIModelsURL mirrors callOpenAI's endpoint derivation: a bare host
// gets /v1/models, a /v1 base gets /models, and a full chat-completions
// URL is trimmed back to its /v1 root first.
func openAIModelsURL(base string) string {
	if strings.HasSuffix(base, "/chat/completions") {
		base = strings.TrimSuffix(base, "/chat/completions")
	}
	if strings.HasSuffix(base, "/v1") {
		return base + "/models"
	}
	return base + "/v1/models"
}

// dedupeModels drops empties and repeats, preserving provider order.
func dedupeModels(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	return out
}

// classifyProbeError turns a transport failure into a Chinese reason the
// settings page can show as-is. Provider error bodies never reach this
// path, so nothing here can leak a key.
func classifyProbeError(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("连接模型端点超时，请检查端点地址与网络")
	}
	return fmt.Errorf("连接模型端点失败：%s", truncateProbeText(err.Error(), 200))
}

// classifyProbeStatus maps a non-200 list response to a Chinese reason.
// 401/403 means the key is wrong, 404 means the endpoint does not serve
// a model list at all (manual input stays available either way).
func classifyProbeStatus(status int, body []byte) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("API Key 无效或无权访问模型列表，请检查密钥")
	case http.StatusNotFound:
		return fmt.Errorf("该端点不支持模型列表查询，请手动输入模型名称")
	}
	detail := ""
	var envelope struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error != nil {
		detail = envelope.Error.Message
	}
	if detail == "" {
		detail = strings.TrimSpace(string(body))
	}
	if detail != "" {
		return fmt.Errorf("查询模型列表失败（HTTP %d）：%s", status, truncateProbeText(detail, 200))
	}
	return fmt.Errorf("查询模型列表失败（HTTP %d）", status)
}

func truncateProbeText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
