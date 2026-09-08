package cert

import "time"

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
		DirectoryURL: "https://dv.acme.pki.goog/directory",
		RequiresEAB:  true,
		Description:  "谷歌公共 CA，签发速度极快且 OCSP 稳定，需在 Google Cloud 获取 EAB KID 和 HMAC Key。",
	},
	{
		ID:           "google_staging",
		Name:         "Google Trust Services (测试环境)",
		DirectoryURL: "https://dv.acme.staging.pki.goog/directory",
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
