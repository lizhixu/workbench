package cert

import (
	"fmt"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
)

// ACMEProviderPreset represents well-known CA presets.
type ACMEProviderPreset struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	DirectoryURL string `json:"directory_url"`
	RequiresEAB  bool   `json:"requires_eab"`
	// RequiresContact marks CAs that reject newAccount without a contact
	// address (RFC 8555 §7.3 makes it optional, but Google Trust Services
	// answers 400 malformed: "Accounts must have at least one contact.").
	RequiresContact bool   `json:"requires_contact"`
	Description     string `json:"description"`
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
		ID:              "google",
		Name:            "Google Trust Services (GTS)",
		DirectoryURL:    "https://dv.acme-v02.api.pki.goog/directory",
		RequiresEAB:     true,
		RequiresContact: true,
		Description:     "谷歌公共 CA，签发速度极快且 OCSP 稳定，需在 Google Cloud 获取 EAB KID 和 HMAC Key，且必须填写联系邮箱。",
	},
	{
		ID:              "google_staging",
		Name:            "Google Trust Services (测试环境)",
		DirectoryURL:    "https://dv.acme-v02.test-api.pki.goog/directory",
		RequiresEAB:     true,
		RequiresContact: true,
		Description:     "Google Cloud 测试环境，需通过 gcloud publicca 生成测试 EAB 凭据，且必须填写联系邮箱。",
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

// requiresContactHosts lists ACME directory hosts whose CA rejects
// newAccount requests without a contact field. Google Trust Services answers
// 400 malformed ("Accounts must have at least one contact.") instead of
// honouring the RFC 8555 §7.3 optional-contact rule.
var requiresContactHosts = []string{"pki.goog"}

// RequiresContact reports whether the CA behind directoryURL demands a
// contact address. It matches on host (not on the preset ID) so a manually
// typed GTS directory is enforced the same way as the preset.
func RequiresContact(directoryURL string) bool {
	u, err := url.Parse(strings.TrimSpace(directoryURL))
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	for _, suffix := range requiresContactHosts {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// ValidateAccountContact enforces the CA's contact policy before any request
// leaves the server, so the operator sees an actionable message instead of
// the raw CA error ("Accounts must have at least one contact."). The address
// format check applies to every CA (a malformed address is rejected at save
// time even when the CA would accept an empty contact); the public-suffix
// check only applies to CAs that require a contact.
func ValidateAccountContact(directoryURL, email string) error {
	email = strings.TrimSpace(email)
	requiresContact := RequiresContact(directoryURL)
	if email == "" {
		if requiresContact {
			return fmt.Errorf("该 ACME 机构要求至少填写一个联系邮箱（Google Trust Services 不接受空 contact），请补齐「联系邮箱」后重试")
		}
		return nil
	}
	addr, err := mail.ParseAddress(email)
	if err != nil {
		return fmt.Errorf("联系邮箱 %q 格式不正确：%v", email, err)
	}
	// Public CAs reject addresses whose domain is not publicly resolvable
	// (e.g. admin@watchman.local) with urn:ietf:params:acme:error:invalidContact.
	// Only enforce this for CAs that require a contact: a private CA may
	// legitimately use an internal mail domain.
	if requiresContact {
		// Email domains are case-insensitive, but publicsuffix matches the
		// literal labels: "EXAMPLE.ORG" is reported as non-ICANN, which would
		// wrongly reject a valid address.
		domain := strings.ToLower(addr.Address[strings.LastIndex(addr.Address, "@")+1:])
		suffix, icann := publicsuffix.PublicSuffix(domain)
		// A bare public suffix (e.g. "com") is not a registrable mail
		// domain either: it has no label of its own before the suffix.
		if !icann || suffix == domain {
			return fmt.Errorf("联系邮箱 %q 的域名 %q 不是可公开解析的注册域名，CA 会以 invalidContact 拒绝，请填写真实邮箱", email, domain)
		}
	}
	return nil
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
