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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Config is the dns-mng integration settings.
type Config struct {
	Enabled   bool      `json:"enabled"`
	BaseURL   string    `json:"base_url"`
	Username  string    `json:"username"`
	Password  string    `json:"password"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Certificate lifecycle status. Empty is treated as issued for records written
// before the field existed.
const (
	CertStatusPending = "pending" // 签发中：点击申请即建档的占位记录，等待 ACME 完成
	CertStatusIssued  = "issued"  // 已签发
	CertStatusFailed  = "failed"  // 签发失败
)

// ErrCertificateBeingIssued is returned by Hub.Delete when the target is a
// pending issuance placeholder. Refusing the delete is a temporary conflict
// (HTTP 409), not a missing record (HTTP 404); handlers map it with errors.Is.
var ErrCertificateBeingIssued = errors.New("certificate is being issued and cannot be deleted yet")

// Certificate is one issued certificate with its PEM material.
type Certificate struct {
	ID        string              `json:"id"`
	AccountID string              `json:"account_id,omitempty"` // ID of the ACMEAccount that issued this cert
	Challenge ChallengePreference `json:"challenge,omitempty"`  // preferred ACME challenge for issuance/renewal (auto/http-01/dns-01); manual DNS-01 is per-issuance only and is never stored
	Domains   []string            `json:"domains"`              // SANs, e.g. ["*.example.com", "example.com"]
	Status    string              `json:"status,omitempty"`     // pending | issued | failed；空值按 issued 兼容存量
	CertPEM   string              `json:"-"`
	KeyPEM    string              `json:"-"`
	NotBefore time.Time           `json:"not_before"`
	NotAfter  time.Time           `json:"not_after"`
	Issuer    string              `json:"issuer,omitempty"`
	AutoRenew bool                `json:"auto_renew"`
	CreatedAt time.Time           `json:"created_at"`
	Renewing  bool                `json:"renewing,omitempty"`
	LastError string              `json:"last_error,omitempty"`
	Remark    string              `json:"remark,omitempty"` // admin-managed note (≤200 runes), edited via PUT /certs/:id/remark
	LastLogAt time.Time           `json:"last_log_at,omitempty"`
}

// DefaultDirectoryURL is the Let's Encrypt production ACME directory.
const DefaultDirectoryURL = "https://acme-v02.api.letsencrypt.org/directory"

// Hub manages dns-mng settings, multiple ACME accounts, certificates and renewals.
type Hub struct {
	mu sync.RWMutex
	// acmeMu serializes the actual ACME network/order/challenge work across
	// issuance, renewal, and manual DNS-01 flows. Handler locks cannot protect
	// background goroutines, so this lock lives at the Hub execution boundary.
	acmeMu   sync.Mutex
	dir      string
	log      *slog.Logger
	cfg      Config
	accounts map[string]*ACMEAccount
	certs    map[string]*Certificate
	tasks    map[string]*IssueTask
	liveLogs sync.Map // id (string) -> *certLogger
	// deletedCerts records ids removed through Delete. Issue/Import and the
	// manual flow call storeResult with a brand-new id, so a missing certs[id]
	// alone cannot distinguish "new certificate" from "deleted while a renewal
	// was in flight"; this tombstone lets a late storeResult discard (and clean
	// up) a result for a row the operator already deleted.
	deletedCerts map[string]struct{}
	// renewBeforeDays supplies the configured "days before expiry" for
	// automatic renewal (settings: certs.auto_renew_days). Nil means the
	// built-in default (30 days). Injected by the server entrypoint so the
	// cert package stays decoupled from the settings store.
	renewBeforeDays func() int
	// onRenewed, when set, is invoked after a certificate lands in the store
	// — new ACME issuance, renewal, manual DNS-01 confirmation and import
	// all funnel there. Injected by the wiring layer (router) so a re-issued
	// certificate is re-distributed to the nginx hosts serving bound app
	// domains; see SetOnRenewed for the execution contract.
	onRenewed func(*Certificate)
}

// SetRenewBeforeDaysProvider injects the auto-renew lead-time source.
// A nil provider restores the built-in default.
func (h *Hub) SetRenewBeforeDaysProvider(fn func() int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.renewBeforeDays = fn
}

// SetOnRenewed injects the post-store callback fired whenever a certificate
// is stored (renewal, fresh issuance covering already-bound domains, manual
// DNS-01 confirmation, import). The callback runs on its own goroutine with
// a PEM-carrying copy; it must never be required for correctness of the
// issuance flow itself — panics are recovered and logged, a slow callback
// cannot block Issue/Renew/Import (which may run inside synchronous HTTP
// handlers such as the proxy bind's auto-issue fallback). A nil callback
// disables the hook.
func (h *Hub) SetOnRenewed(fn func(*Certificate)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onRenewed = fn
}

// notifyRenewed hands the freshly stored certificate (PEMs included) to the
// onRenewed hook. Best-effort by design: no callback, a missing certificate
// or a panicking hook are all tolerated silently.
func (h *Hub) notifyRenewed(id string) {
	if ll, ok := h.liveLogs.Load(id); ok {
		if clog, ok2 := ll.(*certLogger); ok2 {
			clog.Logf("反代回推", "已触发应用反代 Nginx 证书分发与热重载通知 (cert=%s)", id)
		}
	}
	h.mu.RLock()
	fn := h.onRenewed
	h.mu.RUnlock()
	if fn == nil {
		return
	}
	c, ok := h.Get(id)
	if !ok {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				h.log.Warn("证书回推回调 panic，已忽略", "cert", id, "panic", r)
			}
		}()
		fn(c)
	}()
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
		tasks:    make(map[string]*IssueTask),
	}
	h.deletedCerts = make(map[string]struct{})
	if err := os.MkdirAll(h.dir, 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(h.logsDir(), 0700); err != nil {
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
	if err := h.loadTasks(); err != nil {
		return nil, err
	}
	// A restart must not leave manual DNS-01 orders stuck in "verifying":
	// flip them back to a retryable error so the administrator can confirm
	// again (or cancel) instead of staring at a dead task.
	h.recoverInterruptedPending()
	return h, nil
}

func (h *Hub) configPath() string        { return filepath.Join(h.dir, "hub.json") }
func (h *Hub) accountsPath() string      { return filepath.Join(h.dir, "accounts.json") }
func (h *Hub) certIndexPath() string     { return filepath.Join(h.dir, "index.json") }
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
	//
	// Also migrate obsolete ACME directory URLs (see legacyDirectoryURLs):
	// an account saved against a non-existent endpoint keeps failing TLS
	// verification until the stored URL is corrected.
	migrated := false
	for _, acc := range h.accounts {
		if acc.Email == "admin@watchman.local" || acc.Email == "admin@example.com" {
			acc.Email = ""
			migrated = true
		}
		if fixed := NormalizeDirectoryURL(acc.DirectoryURL); fixed != acc.DirectoryURL {
			acc.DirectoryURL = fixed
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
	// Never persist an obsolete directory endpoint (see legacyDirectoryURLs):
	// the UI prefilled one in older builds and clients may still send it.
	acc.DirectoryURL = NormalizeDirectoryURL(acc.DirectoryURL)
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
	needSave := false
	for _, c := range list {
		certB, errCert := os.ReadFile(h.certFile(c.ID))
		keyB, errKey := os.ReadFile(h.keyFile(c.ID))
		if errCert == nil && errKey == nil {
			c.CertPEM = string(certB)
			c.KeyPEM = string(keyB)
		} else {
			c.LastError = "certificate material missing on disk"
		}
		// 重启对账：残留的「签发中」占位记录不可能还在推进（签发协程已随
		// 进程消失），置为失败，避免前端永远显示签发中。与 loadTasks 把
		// running 任务对账为 failed 同一语义。
		if c.Status == CertStatusPending {
			c.Status = CertStatusFailed
			c.LastError = "服务重启，签发中断"
			needSave = true
		}
		h.certs[c.ID] = c
	}
	if needSave {
		if err := h.saveCertsLocked(); err != nil {
			h.log.Warn("persist interrupted pending certs failed", "err", err)
		}
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

// FindByDomain returns the best certificate covering domain. With several
// certificates covering the same name (multiple CA vendors, wildcard plus
// exact) the pick must be deterministic — cover probe, panel SNI fallback and
// legacy binding resolution all share this function, and a map-iteration
// order would let two calls disagree. Ranking: exact SAN beats wildcard;
// among wildcards more labels win; then the longest remaining validity;
// finally the certificate ID keeps the order stable across restarts.
func (h *Hub) FindByDomain(domain string) (*Certificate, bool) {
	return h.findByDomain(domain, false)
}

// FindUsableByDomain is FindByDomain restricted to certificates that can
// actually serve traffic right now: private key present and validity window
// covering now. The reverse-proxy bind resolves its automatic certificate
// through this function, so a renewal-in-progress or expired record can never
// be materialized into an app binding while a sibling covering certificate is
// still good.
func (h *Hub) FindUsableByDomain(domain string) (*Certificate, bool) {
	return h.findByDomain(domain, true)
}

// CertUnusableReason explains why a covering certificate cannot serve now;
// empty means usable.
func CertUnusableReason(c *Certificate, now time.Time) string {
	if !now.Before(c.NotAfter) {
		return "证书已过期"
	}
	if now.Before(c.NotBefore) {
		return "证书尚未生效"
	}
	if c.KeyPEM == "" {
		return "证书缺少私钥材料"
	}
	return ""
}

func (h *Hub) findByDomain(domain string, usableOnly bool) (*Certificate, bool) {
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	now := time.Now()
	h.mu.RLock()
	defer h.mu.RUnlock()
	var best *Certificate
	bestRank := certMatchRank{}
	for _, c := range h.certs {
		// 占位/失败的证书记录不参与域名匹配：它们没有可用材料，绝不能在
		// cover 探测、used-by 反查或 SNI 回退中遮蔽同域名的真实证书。
		if c.Status == CertStatusPending || c.Status == CertStatusFailed {
			continue
		}
		if usableOnly && CertUnusableReason(c, now) != "" {
			continue
		}
		// A certificate's rank is governed by its best covering SAN, not the
		// first one found (a cert may carry both `*.example.com` and an exact
		// entry for the very domain being resolved).
		found := false
		certBest := certMatchRank{}
		for _, san := range c.Domains {
			if !DomainCovers(san, domain) {
				continue
			}
			if r := rankCertMatch(san, c); !found || r.betterThan(certBest) {
				found, certBest = true, r
			}
		}
		if found && (best == nil || certBest.betterThan(bestRank)) {
			best, bestRank = c, certBest
		}
	}
	if best == nil {
		return nil, false
	}
	cp := *best
	return &cp, true
}

// certMatchRank scores how well one SAN entry covers a domain: exact matches
// outrank wildcards, and deeper wildcards (more labels) outrank shallower
// ones (`*.a.example.com` > `*.example.com`).
type certMatchRank struct {
	exact    bool
	labels   int
	notAfter time.Time
	id       string
}

func rankCertMatch(san string, c *Certificate) certMatchRank {
	exact := !strings.HasPrefix(san, "*.")
	labels := strings.Count(san, ".")
	if !exact {
		labels = strings.Count(strings.TrimPrefix(san, "*."), ".")
	}
	return certMatchRank{exact: exact, labels: labels, notAfter: c.NotAfter, id: c.ID}
}

func (r certMatchRank) betterThan(o certMatchRank) bool {
	if r.exact != o.exact {
		return r.exact
	}
	if r.labels != o.labels {
		return r.labels > o.labels
	}
	if !r.notAfter.Equal(o.notAfter) {
		return r.notAfter.After(o.notAfter)
	}
	return r.id > o.id
}

// Delete removes one certificate from the hub.
func (h *Hub) Delete(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.certs[id]
	if !ok {
		return fmt.Errorf("certificate %s not found", id)
	}
	// Backstop for the handler's pending guard: a pending row has a live
	// issuance task whose storeResult would resurrect it on success.
	if c.Status == CertStatusPending {
		return fmt.Errorf("%w: %s", ErrCertificateBeingIssued, id)
	}
	delete(h.certs, id)
	// Mark the id as terminally deleted so a late storeResult (e.g. a renewal
	// that was in flight when the operator deleted this issued certificate)
	// discards its result instead of resurrecting the row.
	h.deletedCerts[id] = struct{}{}
	_ = os.Remove(h.certFile(id))
	_ = os.Remove(h.keyFile(id))
	_ = os.Remove(h.certLogFile(id))
	return h.saveCertsLocked()
}

// maxRemarkRunes caps the admin-managed certificate note.
const maxRemarkRunes = 200

// UpdateRemark edits the admin-managed note of one certificate. The remark is
// metadata only — it never affects issuance, renewal or serving.
func (h *Hub) UpdateRemark(id, remark string) (*Certificate, error) {
	remark = strings.TrimSpace(remark)
	if utf8.RuneCountInString(remark) > maxRemarkRunes {
		return nil, fmt.Errorf("备注最长 %d 字", maxRemarkRunes)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.certs[id]
	if !ok {
		return nil, fmt.Errorf("certificate %s not found", id)
	}
	c.Remark = remark
	if err := h.saveCertsLocked(); err != nil {
		return nil, err
	}
	cp := *c
	return &cp, nil
}

// effectiveChallenge returns the challenge mechanism a renewal should
// use: the preference stored on the record, falling back to auto when
// unset, invalid, or manual (manual TXT provisioning cannot run
// unattended in the renewal loop).
func (c *Certificate) effectiveChallenge() ChallengePreference {
	if c == nil {
		return ChallengeAuto
	}
	switch c.Challenge {
	case ChallengeAuto, ChallengeHTTP01, ChallengeDNS01:
		return c.Challenge
	default:
		return ChallengeAuto
	}
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
	// Only a still-registered record may be rebuilt. If the operator deleted
	// this id while an issuance/renewal was in flight, storeResult must not
	// resurrect the row, and must not leave the PEM files it just wrote as
	// orphans. Brand-new ids from Issue/Import/manual/test injection are
	// absent from certs but also absent from deletedCerts, so they still
	// create a fresh record as before.
	if _, exists := h.certs[id]; !exists {
		if _, deleted := h.deletedCerts[id]; deleted {
			_ = os.Remove(h.certFile(id))
			_ = os.Remove(h.keyFile(id))
			return fmt.Errorf("证书 %s 已被删除，丢弃本次签发结果", id)
		}
	}
	// A renewal rebuilds the record in place (same ID); the admin-managed
	// remark and the stored challenge preference predate it and must
	// survive the re-issue.
	var remark string
	var challenge ChallengePreference
	if prev, ok := h.certs[id]; ok {
		remark = prev.Remark
		challenge = prev.Challenge
	}
	h.certs[id] = &Certificate{
		ID:        id,
		AccountID: accountID,
		Challenge: challenge,
		Domains:   domains,
		Status:    CertStatusIssued,
		CertPEM:   string(certPEM),
		KeyPEM:    string(keyPEM),
		NotBefore: notBefore,
		NotAfter:  notAfter,
		Issuer:    issuer,
		AutoRenew: autoRenew,
		CreatedAt: time.Now(),
		Remark:    remark,
		LastLogAt: time.Now(),
	}
	return h.saveCertsLocked()
}

// StoreCertForTest injects a certificate directly into the hub storage.
// Used by tests across packages (e.g. apps proxy tests).
func (h *Hub) StoreCertForTest(id string, domains []string, certPEM, keyPEM string) error {
	return h.storeResult(id, "", domains, []byte(certPEM), []byte(keyPEM), time.Now(), time.Now().Add(90*24*time.Hour), "test", false)
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

// createPendingCert registers the placeholder certificate record the moment an
// issuance is accepted, so the UI shows a「签发中」row immediately instead of
// only after the ACME flow succeeds. The record carries no key material and a
// zero validity window, so it is never usable for serving traffic; the issuing
// goroutine later fills it in place (storeResult) or marks it failed.
func (h *Hub) createPendingCert(id, accountID string, domains []string, ch ChallengePreference) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Manual DNS-01 is per-issuance only and can never run unattended;
	// never persist it as the record's challenge preference.
	if ch == ChallengeDNS01Manual {
		ch = ChallengeAuto
	}
	h.certs[id] = &Certificate{
		ID:        id,
		AccountID: accountID,
		Challenge: ch,
		Domains:   append([]string(nil), domains...),
		Status:    CertStatusPending,
		AutoRenew: true,
		CreatedAt: time.Now(),
	}
	return h.saveCertsLocked()
}

// markIssueFailed flips a pending placeholder to failed, recording the cause so
// the row shows「签发失败」with an inspectable log. Best-effort persistence.
func (h *Hub) markIssueFailed(id, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Only a pending placeholder may become failed. A late failure callback
	// must never downgrade a record that already reached CertStatusIssued
	// (an empty status is a legacy issued record, not a placeholder).
	if c, ok := h.certs[id]; ok && c.Status == CertStatusPending {
		c.Status = CertStatusFailed
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
func (h *Hub) issueACME(acc *ACMEAccount, identifiers []string, ch ChallengePreference, clog *certLogger) (certPEM, keyPEM []byte, notBefore, notAfter time.Time, issuer string, err error) {
	if clog != nil {
		clog.Logf("准备账户", "使用 ACME 账户 %q (%s), 目标标识符: %s", acc.Name, acc.Email, strings.Join(identifiers, ", "))
		clog.Logf("挑战选择", "首选验证机制: %s", string(ch))
	}
	if !ch.Valid() {
		if clog != nil {
			clog.Warnf("挑战选择", "未知的验证方式: %q", string(ch))
		}
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("未知的验证方式: %q", string(ch))
	}
	if ch == ChallengeDNS01Manual {
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("DNS-01 手动验证需分两步完成（先获取解析记录，管理员添加解析后再确认），请使用 StartManualDNSOrder/ConfirmManualDNSOrder")
	}
	// RFC 8555 §8.4: a wildcard authorization only offers dns-01. Detect it up
	// front so the operator gets an actionable message instead of a mid-flow
	// "no usable challenge" after the order has already been created.
	wildcard, hasWildcard := firstWildcard(identifiers)
	if hasWildcard && ch == ChallengeHTTP01 {
		msg := fmt.Sprintf("泛域名 %q 只能使用 DNS-01 验证（CA 不会为 *. 前缀下发 http-01 挑战），请把验证方式改为「自动」或「DNS-01」，不能选择 HTTP-01", wildcard)
		if clog != nil {
			clog.Warn("挑战选择", msg)
		}
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("%s", msg)
	}
	solvers := h.buildSolvers()
	defer solvers.stop()
	solvers, err = solvers.applyPreference(ch)
	if err != nil {
		if clog != nil {
			clog.Warnf("挑战选择", "挑战求解器筛选失败: %v", err)
		}
		return nil, nil, time.Time{}, time.Time{}, "", err
	}
	if hasWildcard && solvers.dns == nil {
		msg := fmt.Sprintf("泛域名 %q 必须使用 DNS-01 验证，但当前没有可用的 DNS-01 求解器：请在证书中心配置并启用 dns-mng，或改用「DNS-01（手动解析）」手工添加 TXT 记录", wildcard)
		if clog != nil {
			clog.Warn("挑战选择", msg)
		}
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("%s", msg)
	}
	if !solvers.hasAny() {
		msg := "无法完成域名验证：HTTP-01 需要临时监听 80 端口（当前不可用），DNS-01 需要在证书中心配置并启用 dns-mng"
		if clog != nil {
			clog.Warn("挑战选择", msg)
		}
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("%s", msg)
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
		clog:         clog,
	}

	accountKey, err := h.loadOrCreateAccountKey(acc.ID)
	if err != nil {
		if clog != nil {
			clog.Warnf("准备账户", "加载/创建账户密钥失败: %v", err)
		}
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

	clog := newCertLogger("issue_"+randomHex(4), h.log)
	h.acmeMu.Lock()
	defer h.acmeMu.Unlock()
	certPEM, keyPEM, notBefore, notAfter, issuer, err := h.issueACME(acc, clean, ch, clog)
	if err != nil {
		return nil, err
	}
	id := "crt_" + randomHex(8)
	if err := h.storeResult(id, acc.ID, clean, certPEM, keyPEM, notBefore, notAfter, issuer, true); err != nil {
		return nil, err
	}
	// Persist the challenge preference on the new record (storeResult only
	// preserves it from a previous record, and there is none here).
	if ch != ChallengeAuto && ch != ChallengeDNS01Manual {
		h.mu.Lock()
		if c, ok := h.certs[id]; ok {
			c.Challenge = ch
			_ = h.saveCertsLocked()
		}
		h.mu.Unlock()
	}
	clog.Logf("完成入库", "证书入库成功: ID=%s, 包含域名: %s", id, strings.Join(clean, ", "))
	h.notifyRenewed(id)
	_ = h.SaveCertLog(id, clog.String())
	c, _ := h.Get(id)
	return c, nil
}

// StartIssueTask creates a background ACME issuance task and returns its tracking record.
func (h *Hub) StartIssueTask(identifiers []string, accountID string, ch ChallengePreference) (*IssueTask, *Certificate, error) {
	clean := make([]string, 0, len(identifiers))
	for _, d := range identifiers {
		d = strings.TrimSpace(strings.ToLower(d))
		if d != "" {
			clean = append(clean, d)
		}
	}
	if len(clean) == 0 {
		return nil, nil, fmt.Errorf("至少需要一个域名或 IP")
	}

	var acc *ACMEAccount
	if accountID != "" {
		got, ok := h.GetAccount(accountID)
		if !ok {
			return nil, nil, fmt.Errorf("ACME 机构账户 %s 不存在", accountID)
		}
		acc = got
	} else {
		got, err := h.GetDefaultAccount()
		if err != nil {
			return nil, nil, err
		}
		acc = got
	}

	// 提前预生成证书 ID 与任务 ID，并在协程启动前立即落一条「签发中」占位
	// 记录，让用户点击申请后列表立刻可见。
	certID := "crt_" + randomHex(8)
	if err := h.createPendingCert(certID, acc.ID, clean, ch); err != nil {
		return nil, nil, fmt.Errorf("创建证书占位记录失败: %w", err)
	}

	task, err := h.CreateIssueTaskWithError(clean, acc.ID, string(ch))
	if err != nil {
		return nil, nil, fmt.Errorf("保存签发任务失败: %w", err)
	}

	// 将任务与证书互相绑定（IssueTask.CertID 指向预生成的 certID）。
	_ = h.UpdateIssueTaskWithError(task.ID, func(t *IssueTask) {
		t.CertID = certID
	})
	task.CertID = certID

	clog := newCertLogger(task.ID, h.log)
	h.RegisterLiveLog(task.ID, clog)
	// 同时让证书 ID 也能实时消费（日志弹窗可直接用证书 ID 或任务 ID 查看）
	h.RegisterLiveLog(certID, clog)

	go func() {
		defer h.UnregisterLiveLog(task.ID)
		defer h.UnregisterLiveLog(certID)
		h.acmeMu.Lock()
		defer h.acmeMu.Unlock()
		clog.Logf("准备账户", "启动证书签发任务 [%s], 域名/IP: %s, 挑战机制: %s", task.ID, strings.Join(clean, ", "), string(ch))
		certPEM, keyPEM, notBefore, notAfter, issuer, err := h.issueACME(acc, clean, ch, clog)
		if err != nil {
			clog.Warnf("完成入库", "证书签发失败: %v", err)
			h.markIssueFailed(certID, err.Error())
			_ = h.SaveCertLog(certID, clog.String())
			if updateErr := h.UpdateIssueTaskWithError(task.ID, func(t *IssueTask) {
				t.Status = TaskFailed
				t.Error = err.Error()
				t.Log = clog.String()
			}); updateErr != nil {
				h.log.Error("persist failed cert issue task state", "id", task.ID, "err", updateErr)
			}
			return
		}
		clog.Logf("完成入库", "证书获取完成，正在入库: ID=%s", certID)
		if err := h.storeResult(certID, acc.ID, clean, certPEM, keyPEM, notBefore, notAfter, issuer, true); err != nil {
			clog.Warnf("完成入库", "证书存储失败: %v", err)
			h.markIssueFailed(certID, err.Error())
			_ = h.SaveCertLog(certID, clog.String())
			if updateErr := h.UpdateIssueTaskWithError(task.ID, func(t *IssueTask) {
				t.Status = TaskFailed
				t.CertID = certID
				t.Error = err.Error()
				t.Log = clog.String()
			}); updateErr != nil {
				h.log.Error("persist failed cert issue task state", "id", task.ID, "err", updateErr)
			}
			return
		}
		clog.Logf("完成入库", "证书已成功入库: %s", certID)
		h.notifyRenewed(certID)
		if err := h.SaveCertLog(certID, clog.String()); err != nil {
			h.log.Warn("persist cert issue log failed", "id", certID, "err", err)
		}
		if updateErr := h.UpdateIssueTaskWithError(task.ID, func(t *IssueTask) {
			t.Status = TaskSuccess
			t.CertID = certID
			t.Log = clog.String()
		}); updateErr != nil {
			h.log.Error("persist successful cert issue task state failed", "id", task.ID, "err", updateErr)
		}
	}()

	certCopy, _ := h.Get(certID)
	return task, certCopy, nil
}

// StartReissueTask re-issues an existing certificate under a new
// configuration (ACME account, domains, challenge preference) and stores
// the result back into the same record (same ID, same bindings). The old
// material keeps serving until the new issuance lands: a failed reissue
// leaves the record untouched apart from LastError (markError) — the
// domains on the record always describe the stored material, never an
// issuance that did not happen, so a failed reissue can never make
// FindByDomain/SNI match a domain the stored certificate does not cover.
// The account and challenge changes are saved on the record up front
// (they describe how the record is managed, not the material); a manual
// challenge is never persisted — it cannot run unattended.
func (h *Hub) StartReissueTask(id string, identifiers []string, accountID string, ch ChallengePreference) (*IssueTask, *Certificate, error) {
	clean := make([]string, 0, len(identifiers))
	for _, d := range identifiers {
		d = strings.TrimSpace(strings.ToLower(d))
		if d != "" {
			clean = append(clean, d)
		}
	}
	if len(clean) == 0 {
		return nil, nil, fmt.Errorf("至少需要一个域名或 IP")
	}

	var acc *ACMEAccount
	if accountID != "" {
		got, ok := h.GetAccount(accountID)
		if !ok {
			return nil, nil, fmt.Errorf("ACME 机构账户 %s 不存在", accountID)
		}
		acc = got
	} else {
		got, err := h.GetDefaultAccount()
		if err != nil {
			return nil, nil, err
		}
		acc = got
	}

	h.mu.Lock()
	cur, ok := h.certs[id]
	if !ok {
		h.mu.Unlock()
		return nil, nil, fmt.Errorf("certificate %s not found", id)
	}
	if cur.Status == CertStatusPending {
		h.mu.Unlock()
		return nil, nil, fmt.Errorf("%w: %s", ErrCertificateBeingIssued, id)
	}
	if cur.Renewing {
		h.mu.Unlock()
		return nil, nil, fmt.Errorf("该证书正在续期中，请等待续期完成后再重新签发")
	}
	// The edit does not touch auto-renew: preserve the record's setting.
	autoRenew := cur.AutoRenew
	cur.Renewing = true
	cur.LastError = ""
	cur.AccountID = acc.ID
	if ch != ChallengeDNS01Manual {
		cur.Challenge = ch
	}
	_ = h.saveCertsLocked()
	h.mu.Unlock()

	task, err := h.CreateIssueTaskWithError(clean, acc.ID, string(ch))
	if err != nil {
		// Roll the in-flight marker back so the record is not stuck.
		h.mu.Lock()
		if c, ok := h.certs[id]; ok {
			c.Renewing = false
			_ = h.saveCertsLocked()
		}
		h.mu.Unlock()
		return nil, nil, fmt.Errorf("保存签发任务失败: %w", err)
	}

	// Bind the task to the existing record (not a fresh id).
	_ = h.UpdateIssueTaskWithError(task.ID, func(t *IssueTask) {
		t.CertID = id
	})
	task.CertID = id

	clog := newCertLogger(task.ID, h.log)
	h.RegisterLiveLog(task.ID, clog)
	h.RegisterLiveLog(id, clog)

	go func() {
		defer h.UnregisterLiveLog(task.ID)
		defer h.UnregisterLiveLog(id)
		h.acmeMu.Lock()
		defer h.acmeMu.Unlock()
		clog.Logf("准备账户", "启动证书重新签发任务 [%s], 域名/IP: %s, 挑战机制: %s", task.ID, strings.Join(clean, ", "), string(ch))
		certPEM, keyPEM, notBefore, notAfter, issuer, err := h.issueACME(acc, clean, ch, clog)
		if err != nil {
			clog.Warnf("完成入库", "证书重新签发失败: %v", err)
			h.markError(id, err.Error())
			_ = h.SaveCertLog(id, clog.String())
			if updateErr := h.UpdateIssueTaskWithError(task.ID, func(t *IssueTask) {
				t.Status = TaskFailed
				t.Error = err.Error()
				t.Log = clog.String()
			}); updateErr != nil {
				h.log.Error("persist failed cert reissue task state", "id", task.ID, "err", updateErr)
			}
			return
		}
		clog.Logf("完成入库", "证书获取完成，正在入库: ID=%s", id)
		if err := h.storeResult(id, acc.ID, clean, certPEM, keyPEM, notBefore, notAfter, issuer, autoRenew); err != nil {
			clog.Warnf("完成入库", "证书存储失败: %v", err)
			h.markError(id, err.Error())
			_ = h.SaveCertLog(id, clog.String())
			if updateErr := h.UpdateIssueTaskWithError(task.ID, func(t *IssueTask) {
				t.Status = TaskFailed
				t.CertID = id
				t.Error = err.Error()
				t.Log = clog.String()
			}); updateErr != nil {
				h.log.Error("persist failed cert reissue task state", "id", task.ID, "err", updateErr)
			}
			return
		}
		clog.Logf("完成入库", "证书已成功入库: %s", id)
		h.notifyRenewed(id)
		if err := h.SaveCertLog(id, clog.String()); err != nil {
			h.log.Warn("persist cert reissue log failed", "id", id, "err", err)
		}
		if updateErr := h.UpdateIssueTaskWithError(task.ID, func(t *IssueTask) {
			t.Status = TaskSuccess
			t.CertID = id
			t.Log = clog.String()
		}); updateErr != nil {
			h.log.Error("persist successful cert reissue task state failed", "id", task.ID, "err", updateErr)
		}
	}()

	certCopy, _ := h.Get(id)
	return task, certCopy, nil
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
	h.notifyRenewed(id)
	c, _ := h.Get(id)
	return c, nil
}

// Renew re-issues an existing certificate using its original ACME account.
func (h *Hub) Renew(id string) (*Certificate, error) {
	c, ok := h.Get(id)
	if !ok {
		return nil, fmt.Errorf("certificate %s not found", id)
	}
	// Decide both guards inside one h.mu critical section, against the
	// current h.certs[id] rather than the stale value copy above. Two
	// concurrent Renews otherwise both read Renewing=false from their
	// copies and pass; acmeMu only serializes the ACME work and never
	// re-checks, so the same domain would be issued twice and storeResult
	// would overwrite the row twice. Failed rows still pass (retry path).
	h.mu.Lock()
	cur, ok := h.certs[id]
	if !ok {
		h.mu.Unlock()
		return nil, fmt.Errorf("certificate %s not found", id)
	}
	if cur.Status == CertStatusPending {
		h.mu.Unlock()
		return nil, fmt.Errorf("该证书正在签发中，请等待签发完成后再申请")
	}
	if cur.Renewing {
		h.mu.Unlock()
		return nil, fmt.Errorf("该证书正在续期中")
	}
	cur.Renewing = true
	cur.LastError = ""
	_ = h.saveCertsLocked()
	h.mu.Unlock()

	clog := newCertLogger(id, h.log)
	h.RegisterLiveLog(id, clog)
	defer h.UnregisterLiveLog(id)
	h.acmeMu.Lock()
	defer h.acmeMu.Unlock()

	clog.Logf("准备账户", "开始证书续期: %s (%s)", id, strings.Join(c.Domains, ", "))

	var acc *ACMEAccount
	if c.AccountID != "" {
		if got, ok := h.GetAccount(c.AccountID); ok {
			acc = got
		}
	}
	if acc == nil {
		got, err := h.GetDefaultAccount()
		if err != nil {
			clog.Warnf("准备账户", "获取默认 ACME 账户失败: %v", err)
			_ = h.SaveCertLog(id, clog.String())
			h.markError(id, err.Error())
			return nil, err
		}
		acc = got
	}

	certPEM, keyPEM, notBefore, notAfter, issuer, err := h.issueACME(acc, c.Domains, c.effectiveChallenge(), clog)
	if err != nil {
		clog.Warnf("完成入库", "证书续期失败: %v", err)
		_ = h.SaveCertLog(id, clog.String())
		h.markError(id, err.Error())
		return nil, err
	}
	if err := h.storeResult(id, acc.ID, c.Domains, certPEM, keyPEM, notBefore, notAfter, issuer, true); err != nil {
		clog.Warnf("完成入库", "证书更新存储失败: %v", err)
		_ = h.SaveCertLog(id, clog.String())
		h.markError(id, err.Error())
		return nil, err
	}
	clog.Logf("完成入库", "证书续期成功，更新完成: ID=%s, 有效期至 %s", id, notAfter.Format("2006-01-02 15:04:05"))
	h.notifyRenewed(id)
	_ = h.SaveCertLog(id, clog.String())
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
		// Only genuinely issued certificates are renewal candidates. A
		// pending placeholder has no validity window (zero NotAfter) and a
		// failed placeholder never gains one, so without this guard both
		// would qualify forever: the pending row would double-issue once
		// the in-flight task finishes, and the failed row would retry
		// every 6h indefinitely. Empty status is a legacy issued record
		// (predates the Status field) and stays renewable.
		if c.Status == CertStatusPending || c.Status == CertStatusFailed {
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
