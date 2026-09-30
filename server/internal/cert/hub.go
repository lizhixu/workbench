// Package cert implements the SSL certificate hub: it obtains and renews
// certificates via ACME (Let's Encrypt, ZeroSSL, Google Trust Services, SSL.com, LiteSSL),
// delegating the DNS-01 challenge to an external dns-mng instance (D:\codes\ai-api\dns-mng)
// through its HTTP API.
package cert

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Config is the dns-mng integration settings.
type Config struct {
	Enabled   bool      `json:"enabled"`
	BaseURL   string    `json:"base_url"`
	Username  string    `json:"username"`
	Password  string    `json:"password"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Certificate is one issued certificate with its PEM material.
type Certificate struct {
	ID        string    `json:"id"`
	AccountID string    `json:"account_id,omitempty"` // ID of the ACMEAccount that issued this cert
	Domains   []string  `json:"domains"`             // SANs, e.g. ["*.example.com", "example.com"]
	CertPEM   string    `json:"-"`
	KeyPEM    string    `json:"-"`
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
	Issuer    string    `json:"issuer,omitempty"`
	AutoRenew bool      `json:"auto_renew"`
	CreatedAt time.Time `json:"created_at"`
	Renewing  bool      `json:"renewing,omitempty"`
	LastError string    `json:"last_error,omitempty"`
}

// DefaultDirectoryURL is the Let's Encrypt production ACME directory.
const DefaultDirectoryURL = "https://acme-v02.api.letsencrypt.org/directory"

// Hub manages dns-mng settings, multiple ACME accounts, certificates and renewals.
type Hub struct {
	mu       sync.RWMutex
	dir      string
	log      *slog.Logger
	cfg      Config
	accounts map[string]*ACMEAccount
	certs    map[string]*Certificate
	// renewBeforeDays supplies the configured "days before expiry" for
	// automatic renewal (settings: certs.auto_renew_days). Nil means the
	// built-in default (30 days). Injected by the server entrypoint so the
	// cert package stays decoupled from the settings store.
	renewBeforeDays func() int
}

// SetRenewBeforeDaysProvider injects the auto-renew lead-time source.
// A nil provider restores the built-in default.
func (h *Hub) SetRenewBeforeDaysProvider(fn func() int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.renewBeforeDays = fn
}

func (h *Hub) renewBeforeDaysOrDefault() int {
	h.mu.RLock()
	fn := h.renewBeforeDays
	h.mu.RUnlock()
	if fn == nil {
		return defaultRenewBeforeDays
	}
	if d := fn(); d >= 1 && d <= 90 {
		return d
	}
	return defaultRenewBeforeDays
}

// NewHub loads or initializes the hub under dataDir.
func NewHub(dataDir string, log *slog.Logger) (*Hub, error) {
	if log == nil {
		log = slog.Default()
	}
	h := &Hub{
		dir:      filepath.Join(dataDir, "certs"),
		log:      log,
		accounts: make(map[string]*ACMEAccount),
		certs:    make(map[string]*Certificate),
	}
	if err := os.MkdirAll(h.dir, 0700); err != nil {
		return nil, err
	}
	if err := h.loadConfig(); err != nil {
		return nil, err
	}
	if err := h.loadAccounts(); err != nil {
		return nil, err
	}
	if err := h.loadCerts(); err != nil {
		return nil, err
	}
	// A restart must not leave manual DNS-01 orders stuck in "verifying":
	// flip them back to a retryable error so the administrator can confirm
	// again (or cancel) instead of staring at a dead task.
	h.recoverInterruptedPending()
	return h, nil
}

func (h *Hub) configPath() string   { return filepath.Join(h.dir, "hub.json") }
func (h *Hub) accountsPath() string { return filepath.Join(h.dir, "accounts.json") }
func (h *Hub) certIndexPath() string { return filepath.Join(h.dir, "index.json") }
func (h *Hub) certFile(id string) string { return filepath.Join(h.dir, id+".crt") }
func (h *Hub) keyFile(id string) string  { return filepath.Join(h.dir, id+".key") }

func (h *Hub) loadConfig() error {
	b, err := os.ReadFile(h.configPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read cert hub config: %w", err)
	}
	return unmarshalJSON(b, &h.cfg)
}

func (h *Hub) saveConfigLocked() error {
	b, err := marshalJSON(h.cfg)
	if err != nil {
		return err
	}
	tmp := h.configPath() + fmt.Sprintf(".tmp.%d", time.Now().UnixNano())
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, h.configPath())
}

// GetConfig returns a copy of the dns-mng integration settings.
func (h *Hub) GetConfig() Config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cfg
}

// UpdateConfig replaces the settings and persists them.
func (h *Hub) UpdateConfig(cfg Config) error {
	cfg.UpdatedAt = time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg = cfg
	return h.saveConfigLocked()
}

// ---- ACME Accounts Storage ----

func (h *Hub) loadAccounts() error {
	b, err := os.ReadFile(h.accountsPath())
	if err != nil {
		if os.IsNotExist(err) {
			// Auto-seed default Let's Encrypt account
			def := &ACMEAccount{
				ID:           "acc_letsencrypt",
				Name:         "Let's Encrypt (默认)",
				ProviderID:   "letsencrypt",
				DirectoryURL: DefaultDirectoryURL,
				Email:        "",
				IsDefault:    true,
				CreatedAt:    time.Now(),
			}
			h.accounts[def.ID] = def
			_ = h.saveAccountsLocked()
			return nil
		}
		return fmt.Errorf("read acme accounts: %w", err)
	}
	var list []*ACMEAccount
	if err := unmarshalJSON(b, &list); err != nil {
		h.log.Warn("acme accounts corrupted, starting fresh", "err", err)
		return nil
	}
	for _, acc := range list {
		h.accounts[acc.ID] = acc
	}
	if len(h.accounts) == 0 {
		def := &ACMEAccount{
			ID:           "acc_letsencrypt",
			Name:         "Let's Encrypt (默认)",
			ProviderID:   "letsencrypt",
			DirectoryURL: DefaultDirectoryURL,
			Email:        "",
			IsDefault:    true,
			CreatedAt:    time.Now(),
		}
		h.accounts[def.ID] = def
		_ = h.saveAccountsLocked()
	}
	// Migrate legacy placeholder emails: earlier versions seeded
	// admin@watchman.local (and the UI prefilled admin@example.com);
	// neither has a valid public suffix, so Let's Encrypt rejects them
	// with invalidContact. Empty means "omit contact", which RFC 8555 allows.
	migrated := false
	for _, acc := range h.accounts {
		if acc.Email == "admin@watchman.local" || acc.Email == "admin@example.com" {
			acc.Email = ""
			migrated = true
		}
	}
	if migrated {
		_ = h.saveAccountsLocked()
	}
	return nil
}

func (h *Hub) saveAccountsLocked() error {
	list := make([]*ACMEAccount, 0, len(h.accounts))
	for _, acc := range h.accounts {
		list = append(list, acc)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	b, err := marshalJSON(list)
	if err != nil {
		return err
	}
	tmp := h.accountsPath() + fmt.Sprintf(".tmp.%d", time.Now().UnixNano())
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, h.accountsPath())
}

// ListAccounts returns all configured ACME accounts (redacted HMAC keys).
func (h *Hub) ListAccounts() []*ACMEAccount {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*ACMEAccount, 0, len(h.accounts))
	for _, a := range h.accounts {
		out = append(out, a.Redacted())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// GetAccount returns one account with full credentials for server-side signing.
func (h *Hub) GetAccount(id string) (*ACMEAccount, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	a, ok := h.accounts[id]
	if !ok {
		return nil, false
	}
	cp := *a
	return &cp, true
}

// PutAccount creates or replaces an ACME account.
func (h *Hub) PutAccount(acc *ACMEAccount) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if acc.ID == "" {
		acc.ID = "acc_" + randomHex(6)
	}
	if acc.CreatedAt.IsZero() {
		acc.CreatedAt = time.Now()
	}
	// If set as default, clear previous defaults
	if acc.IsDefault {
		for _, a := range h.accounts {
			a.IsDefault = false
		}
	}
	h.accounts[acc.ID] = acc
	return h.saveAccountsLocked()
}

// DeleteAccount deletes an ACME account.
func (h *Hub) DeleteAccount(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.accounts[id]; !ok {
		return fmt.Errorf("account %s not found", id)
	}
	delete(h.accounts, id)
	_ = os.Remove(filepath.Join(h.dir, "acme-account-"+id+".key"))
	return h.saveAccountsLocked()
}

// GetDefaultAccount returns the default ACME account or the first available.
func (h *Hub) GetDefaultAccount() (*ACMEAccount, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, a := range h.accounts {
		if a.IsDefault {
			cp := *a
			return &cp, nil
		}
	}
	for _, a := range h.accounts {
		cp := *a
		return &cp, nil
	}
	return nil, fmt.Errorf("no ACME accounts configured")
}

// ---- Certificates Storage ----

func (h *Hub) loadCerts() error {
	b, err := os.ReadFile(h.certIndexPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read cert index: %w", err)
	}
	var list []*Certificate
	if err := unmarshalJSON(b, &list); err != nil {
		h.log.Warn("cert index corrupted, starting empty", "err", err)
		return nil
	}
	for _, c := range list {
		certB, errCert := os.ReadFile(h.certFile(c.ID))
		keyB, errKey := os.ReadFile(h.keyFile(c.ID))
		if errCert == nil && errKey == nil {
			c.CertPEM = string(certB)
			c.KeyPEM = string(keyB)
		} else {
			c.LastError = "certificate material missing on disk"
		}
		h.certs[c.ID] = c
	}
	return nil
}

func (h *Hub) saveCertsLocked() error {
	list := make([]*Certificate, 0, len(h.certs))
	for _, c := range h.certs {
		cp := *c
		cp.CertPEM, cp.KeyPEM = "", ""
		list = append(list, &cp)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	b, err := marshalJSON(list)
	if err != nil {
		return err
	}
	tmp := h.certIndexPath() + fmt.Sprintf(".tmp.%d", time.Now().UnixNano())
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, h.certIndexPath())
}

// List returns all certificates.
func (h *Hub) List() []*Certificate {
	h.mu.RLock()
	defer h.mu.RUnlock()
	list := make([]*Certificate, 0, len(h.certs))
	for _, c := range h.certs {
		cp := *c
		cp.CertPEM, cp.KeyPEM = "", ""
		list = append(list, &cp)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	return list
}

// Get returns one certificate including PEM material.
func (h *Hub) Get(id string) (*Certificate, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c, ok := h.certs[id]
	if !ok {
		return nil, false
	}
	cp := *c
	return &cp, true
}

// FindByDomain returns the first certificate covering domain.
func (h *Hub) FindByDomain(domain string) (*Certificate, bool) {
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.certs {
		for _, san := range c.Domains {
			if san == domain || (strings.HasPrefix(san, "*.") && strings.HasSuffix(domain, strings.TrimPrefix(san, "*"))) {
				cp := *c
				return &cp, true
			}
		}
	}
	return nil, false
}

// Delete removes one certificate from the hub.
func (h *Hub) Delete(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.certs[id]; !ok {
		return fmt.Errorf("certificate %s not found", id)
	}
	delete(h.certs, id)
	_ = os.Remove(h.certFile(id))
	_ = os.Remove(h.keyFile(id))
	return h.saveCertsLocked()
}

func (h *Hub) storeResult(id string, accountID string, domains []string, certPEM, keyPEM []byte, notBefore, notAfter time.Time, issuer string, autoRenew bool) error {
	if err := os.WriteFile(h.certFile(id), certPEM, 0600); err != nil {
		return err
	}
	if err := os.WriteFile(h.keyFile(id), keyPEM, 0600); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.certs[id] = &Certificate{
		ID:        id,
		AccountID: accountID,
		Domains:   domains,
		CertPEM:   string(certPEM),
		KeyPEM:    string(keyPEM),
		NotBefore: notBefore,
		NotAfter:  notAfter,
		Issuer:    issuer,
		AutoRenew: autoRenew,
		CreatedAt: time.Now(),
	}
	return h.saveCertsLocked()
}

func (h *Hub) markError(id, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.certs[id]; ok {
		c.LastError = msg
		c.Renewing = false
		_ = h.saveCertsLocked()
	}
}

// issueACME runs the ACME flow using the specified account. Challenge
// validation is compatible across mechanisms: plain DNS names prefer HTTP-01
// (temporary :80 listener, no external dependency) and fall back to DNS-01
// via dns-mng when HTTP-01 is unavailable or fails; wildcards require
// DNS-01; IP identifiers (RFC 8738) require HTTP-01. An explicit non-auto
// challenge preference restricts the flow to that mechanism.
func (h *Hub) issueACME(acc *ACMEAccount, identifiers []string, ch ChallengePreference) (certPEM, keyPEM []byte, notBefore, notAfter time.Time, issuer string, err error) {
	if !ch.Valid() {
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("未知的验证方式: %q", string(ch))
	}
	if ch == ChallengeDNS01Manual {
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("DNS-01 手动验证需分两步完成（先获取解析记录，管理员添加解析后再确认），请使用 StartManualDNSOrder/ConfirmManualDNSOrder")
	}
	solvers := h.buildSolvers()
	defer solvers.stop()
	solvers, err = solvers.applyPreference(ch)
	if err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", err
	}
	if !solvers.hasAny() {
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf(
			"无法完成域名验证：HTTP-01 需要临时监听 80 端口（当前不可用），DNS-01 需要在证书中心配置并启用 dns-mng")
	}
	dirURL := acc.DirectoryURL
	if dirURL == "" {
		dirURL = DefaultDirectoryURL
	}

	acmeClient := &acmeClient{
		directoryURL: dirURL,
		email:        acc.Email,
		eabKeyID:     acc.EABKeyID,
		eabHMACKey:   acc.EABHMACKey,
		http:         &http.Client{Timeout: 30 * time.Second},
		log:          h.log,
	}

	accountKey, err := h.loadOrCreateAccountKey(acc.ID)
	if err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("account key: %w", err)
	}
	if err := acmeClient.ensureAccount(accountKey); err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("acme account (%s): %w", acc.Name, err)
	}

	certPEM, keyPEM, notBefore, notAfter, issuer, err = acmeClient.obtainCertificate(accountKey, identifiers, solvers)
	if err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", err
	}
	if issuer == "" {
		issuer = acc.Name
	}
	return certPEM, keyPEM, notBefore, notAfter, issuer, nil
}

// Issue requests a new certificate using a specific or default ACME account.
// identifiers accepts DNS names and/or IP literals (RFC 8738); the challenge
// is picked per identifier type automatically (HTTP-01 preferred, DNS-01
// fallback). Use IssueWithChallenge to pin the challenge mechanism.
func (h *Hub) Issue(identifiers []string, accountID string) (*Certificate, error) {
	return h.IssueWithChallenge(identifiers, accountID, ChallengeAuto)
}

// IssueWithChallenge is Issue with an explicit challenge preference
// (ChallengeAuto, ChallengeHTTP01 or ChallengeDNS01).
func (h *Hub) IssueWithChallenge(identifiers []string, accountID string, ch ChallengePreference) (*Certificate, error) {
	clean := make([]string, 0, len(identifiers))
	for _, d := range identifiers {
		d = strings.TrimSpace(strings.ToLower(d))
		if d != "" {
			clean = append(clean, d)
		}
	}
	if len(clean) == 0 {
		return nil, fmt.Errorf("至少需要一个域名或 IP")
	}

	var acc *ACMEAccount
	if accountID != "" {
		got, ok := h.GetAccount(accountID)
		if !ok {
			return nil, fmt.Errorf("ACME 机构账户 %s 不存在", accountID)
		}
		acc = got
	} else {
		got, err := h.GetDefaultAccount()
		if err != nil {
			return nil, err
		}
		acc = got
	}

	certPEM, keyPEM, notBefore, notAfter, issuer, err := h.issueACME(acc, clean, ch)
	if err != nil {
		return nil, err
	}
	id := "crt_" + randomHex(8)
	if err := h.storeResult(id, acc.ID, clean, certPEM, keyPEM, notBefore, notAfter, issuer, true); err != nil {
		return nil, err
	}
	c, _ := h.Get(id)
	return c, nil
}

// Import stores a user-supplied certificate (issued through another channel,
// e.g. manually uploaded PEM) in the hub so it shows up in the issued list
// and can be selected wherever the panel or apps need a certificate. The PEM
// pair is validated (parseable leaf, key matches the certificate) before
// storing. Imported certificates do not auto-renew via ACME (AutoRenew is
// false); an explicit Renew on one attempts a fresh ACME issuance for the
// same identifiers.
func (h *Hub) Import(certPEM, keyPEM []byte) (*Certificate, error) {
	certPEM = bytes.TrimSpace(certPEM)
	keyPEM = bytes.TrimSpace(keyPEM)
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return nil, fmt.Errorf("证书 PEM 与私钥 PEM 均不能为空")
	}
	keyPair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("证书与私钥不匹配或 PEM 格式错误: %w", err)
	}
	if len(keyPair.Certificate) == 0 {
		return nil, fmt.Errorf("证书 PEM 中没有有效的证书")
	}
	leaf, err := x509.ParseCertificate(keyPair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("解析证书失败: %w", err)
	}
	domains := make([]string, 0, len(leaf.DNSNames)+len(leaf.IPAddresses))
	domains = append(domains, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		domains = append(domains, ip.String())
	}
	if len(domains) == 0 && leaf.Subject.CommonName != "" {
		domains = append(domains, leaf.Subject.CommonName)
	}
	issuer := leaf.Issuer.CommonName
	if issuer == "" {
		issuer = leaf.Issuer.String()
	}
	id := "crt_" + randomHex(8)
	// Manually managed: never auto-renew an imported certificate via ACME.
	if err := h.storeResult(id, "", domains, certPEM, keyPEM, leaf.NotBefore, leaf.NotAfter, issuer, false); err != nil {
		return nil, err
	}
	c, _ := h.Get(id)
	return c, nil
}

// Renew re-issues an existing certificate using its original ACME account.
func (h *Hub) Renew(id string) (*Certificate, error) {
	c, ok := h.Get(id)
	if !ok {
		return nil, fmt.Errorf("certificate %s not found", id)
	}
	h.mu.Lock()
	if c.Renewing {
		h.mu.Unlock()
		return nil, fmt.Errorf("该证书正在续期中")
	}
	if cur, ok := h.certs[id]; ok {
		cur.Renewing = true
		cur.LastError = ""
		_ = h.saveCertsLocked()
	}
	h.mu.Unlock()

	var acc *ACMEAccount
	if c.AccountID != "" {
		if got, ok := h.GetAccount(c.AccountID); ok {
			acc = got
		}
	}
	if acc == nil {
		got, err := h.GetDefaultAccount()
		if err != nil {
			h.markError(id, err.Error())
			return nil, err
		}
		acc = got
	}

	certPEM, keyPEM, notBefore, notAfter, issuer, err := h.issueACME(acc, c.Domains, ChallengeAuto)
	if err != nil {
		h.markError(id, err.Error())
		return nil, err
	}
	if err := h.storeResult(id, acc.ID, c.Domains, certPEM, keyPEM, notBefore, notAfter, issuer, true); err != nil {
		h.markError(id, err.Error())
		return nil, err
	}
	updated, _ := h.Get(id)
	return updated, nil
}

// RunRenewalLoop periodically renews expiring certificates.
func (h *Hub) RunRenewalLoop(stop <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				h.renewExpiring()
			}
		}
	}()
}

// defaultRenewBeforeDays is the built-in auto-renew lead time (days) used
// when no provider is injected. Mirrors settings.DefaultCertRenewDays.
const defaultRenewBeforeDays = 30

func (h *Hub) renewExpiring() {
	renewBefore := time.Duration(h.renewBeforeDaysOrDefault()) * 24 * time.Hour
	for _, c := range h.List() {
		if !c.AutoRenew || c.Renewing {
			continue
		}
		// Renewal threshold scales with validity: renewBefore days for classic
		// 90-day certificates, but short-lived certificates (e.g. Let's
		// Encrypt IP certificates, ~6 days) renew at 1/3 of validity.
		threshold := renewBefore
		if validity := c.NotAfter.Sub(c.NotBefore); validity > 0 {
			if third := validity / 3; third < threshold {
				threshold = third
			}
		}
		if time.Until(c.NotAfter) > threshold {
			continue
		}
		h.log.Info("renewing certificate", "id", c.ID, "domains", c.Domains, "not_after", c.NotAfter)
		if _, err := h.Renew(c.ID); err != nil {
			h.log.Warn("certificate renewal failed", "id", c.ID, "err", err)
		}
	}
}

// loadOrCreateAccountKey maintains a private key per ACME account.
func (h *Hub) loadOrCreateAccountKey(accountID string) (crypto.Signer, error) {
	keyName := "acme-account.key"
	if accountID != "" {
		keyName = "acme-account-" + accountID + ".key"
	}
	path := filepath.Join(h.dir, keyName)
	if b, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(b)
		if block != nil {
			if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
				return key, nil
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0600); err != nil {
		return nil, err
	}
	return key, nil
}
