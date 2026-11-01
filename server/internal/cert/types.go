package cert

import (
	"strings"
	"time"
)

// ACMEProviderPreset represents well-known CA presets.
type ACMEProviderPreset struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	DirectoryURL string `json:"directory_url"`
	RequiresEAB  bool   `json:"requires_eab"`
	Description  string `json:"description"`
}

// Presets lists well-known ACME CAs.
var Presets = []ACMEProviderPreset{
	{
		ID:           "letsencrypt",
		Name:         "Let's Encrypt",
		DirectoryURL: "https://acme-v02.api.letsencrypt.org/directory",
		RequiresEAB:  false,
		Description:  "全球主流免费非盈利 CA，无需 EAB 凭证，只需提供邮箱即可。",
	},
	{
		ID:           "letsencrypt_staging",
		Name:         "Let's Encrypt (测试环境)",
		DirectoryURL: "https://acme-staging-v02.api.letsencrypt.org/directory",
		RequiresEAB:  false,
		Description:  "Let's Encrypt 调试测试环境，无频次限制，颁发未受信任证书。",
	},
	{
		ID:           "google",
		Name:         "Google Trust Services (GTS)",
		DirectoryURL: "https://dv.acme-v02.api.pki.goog/directory",
		RequiresEAB:  true,
		Description:  "谷歌公共 CA，签发速度极快且 OCSP 稳定，需在 Google Cloud 获取 EAB KID 和 HMAC Key。",
	},
	{
		ID:           "google_staging",
		Name:         "Google Trust Services (测试环境)",
		DirectoryURL: "https://dv.acme-v02.test-api.pki.goog/directory",
		RequiresEAB:  true,
		Description:  "Google Cloud 测试环境，需通过 gcloud publicca 生成测试 EAB 凭据。",
	},
	{
		ID:           "zerossl",
		Name:         "ZeroSSL",
		DirectoryURL: "https://acme.zerossl.com/v2/DV90",
		RequiresEAB:  true,
		Description:  "知名商业 CA，支持免费 90 天证书，需在 ZeroSSL 开发者控制台生成 EAB 凭证。",
	},
	{
		ID:           "sslcom",
		Name:         "SSL.com",
		DirectoryURL: "https://acme.ssl.com/sslcom-dv-ecc",
		RequiresEAB:  true,
		Description:  "老牌商业 CA，提供 ACME DV 自动化接口，需在控制台获取 EAB 密钥。",
	},
	{
		ID:           "litessl",
		Name:         "LiteSSL / 自定义 CA",
		DirectoryURL: "",
		RequiresEAB:  false,
		Description:  "适用于企业自建 Step-CA、Vault、Pebble 或 LiteSSL 等标准 RFC 8555 兼容服务。",
	},
}

// legacyDirectoryURLs maps ACME directory endpoints that earlier builds
// shipped (or prefilled in the UI) onto their official values.
//
// Google Trust Services serves its ACME directory at
// dv.acme-v02.api.pki.goog (staging: dv.acme-v02.test-api.pki.goog). The
// earlier short hosts dv.acme.pki.goog / dv.acme.staging.pki.goog are not
// ACME endpoints at all: they resolve to a generic Google frontend, which
// answers the TLS handshake with its default multi-SAN *.google.com
// certificate, so the request dies with
// "x509: certificate is valid for *.google.com ... not dv.acme.pki.goog".
var legacyDirectoryURLs = map[string]string{
	"https://dv.acme.pki.goog/directory":         "https://dv.acme-v02.api.pki.goog/directory",
	"https://dv.acme.staging.pki.goog/directory": "https://dv.acme-v02.test-api.pki.goog/directory",
}

// NormalizeDirectoryURL rewrites a known-obsolete ACME directory URL to the
// official one. Any other value (custom CA, Step-CA, Pebble, ...) is returned
// unchanged, and a trailing slash is ignored when matching.
func NormalizeDirectoryURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return raw
	}
	if fixed, ok := legacyDirectoryURLs[strings.TrimSuffix(trimmed, "/")]; ok {
		return fixed
	}
	return trimmed
}

// ACMEAccount defines an authorized ACME identity at a Certificate Authority.
type ACMEAccount struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	ProviderID   string    `json:"provider_id"` // "letsencrypt" | "zerossl" | "google" | "sslcom" | "custom"
	DirectoryURL string    `json:"directory_url"`
	Email        string    `json:"email"`
	// External Account Binding (EAB), required by ZeroSSL, Google, SSL.com.
	EABKeyID   string    `json:"eab_key_id,omitempty"`
	EABHMACKey string    `json:"eab_hmac_key,omitempty"` // stored 0600 on server, masked in public views
	IsDefault  bool      `json:"is_default"`
	CreatedAt  time.Time `json:"created_at"`
}

// Redacted returns a copy with the EAB HMAC key cleared for safe public viewing.
func (a *ACMEAccount) Redacted() *ACMEAccount {
	if a == nil {
		return nil
	}
	cp := *a
	if cp.EABHMACKey != "" {
		cp.EABHMACKey = "••••••••"
	}
	return &cp
}
