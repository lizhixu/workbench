// Package cert implements the SSL certificate hub: it obtains and renews
// certificates via ACME (Let's Encrypt style), delegating the DNS-01 challenge
// to an external dns-mng instance (D:\codes\ai-api\dns-mng sibling project)
// through its HTTP API. Certificates are persisted under the data dir and can
// be distributed to managed gateway hosts together with generated Nginx
// reverse-proxy configuration (application domain -> app upstream).
package cert

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
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
)

// Config is the dns-mng integration settings, edited from the UI settings.
type Config struct {
	// Enabled turns the whole certificate hub on.
	Enabled bool `json:"enabled"`
	// BaseURL is the dns-mng service address, e.g. "http://10.0.0.2:8080".
	BaseURL string `json:"base_url"`
	// Username/Password are the dns-mng HTTP Basic Auth credentials for its
	// /api/acme/dns01/present|cleanup endpoints.
	Username string `json:"username"`
	Password string `json:"password"`
	// DirectoryURL is the ACME directory endpoint. Empty = Let's Encrypt prod.
	DirectoryURL string `json:"directory_url"`
	// Email is the ACME account contact.
	Email     string    `json:"email"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Certificate is one issued certificate with its PEM material.
type Certificate struct {
	ID        string    `json:"id"`
	Domains   []string  `json:"domains"` // SANs, e.g. ["*.example.com", "example.com"]
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

// ErrDisabled is returned when the hub is not configured yet.
var ErrDisabled = errors.New("certificate hub disabled: dns-mng not configured")

// Hub manages the dns-mng config, certificates and renewal scheduling.
type Hub struct {
	mu    sync.RWMutex
	dir   string
	log   *slog.Logger
	cfg   Config
	certs map[string]*Certificate
}

// NewHub loads or initializes the hub under dataDir.
func NewHub(dataDir string, log *slog.Logger) (*Hub, error) {
	if log == nil {
		log = slog.Default()
	}
	h := &Hub{
		dir:   filepath.Join(dataDir, "certs"),
		log:   log,
		certs: make(map[string]*Certificate),
	}
	if err := os.MkdirAll(h.dir, 0700); err != nil {
		return nil, err
	}
	if err := h.loadConfig(); err != nil {
		return nil, err
	}
	if err := h.loadCerts(); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *Hub) configPath() string { return filepath.Join(h.dir, "hub.json") }

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
		// PEM material lives beside the index in per-cert files; a missing
		// file leaves the record visible but unusable (re-issue fixes it).
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

func (h *Hub) certIndexPath() string     { return filepath.Join(h.dir, "index.json") }
func (h *Hub) certFile(id string) string { return filepath.Join(h.dir, id+".crt") }
func (h *Hub) keyFile(id string) string  { return filepath.Join(h.dir, id+".key") }

func (h *Hub) saveCertsLocked() error {
	list := make([]*Certificate, 0, len(h.certs))
	for _, c := range h.certs {
		cp := *c
		cp.CertPEM, cp.KeyPEM = "", "" // never persist PEM in the index
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

// List returns all certificates (without PEM material).
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

// Get returns one certificate including PEM material (server-side use).
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

// FindByDomain returns the first certificate covering the given domain.
func (h *Hub) FindByDomain(domain string) (*Certificate, bool) {
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.certs {
		for _, san := range c.Domains {
			if san == domain || strings.HasPrefix(san, "*.") && strings.HasSuffix(domain, strings.TrimPrefix(san, "*")) {
				cp := *c
				return &cp, true
			}
		}
	}
	return nil, false
}

// Delete removes one certificate from the hub (records only).
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

// storeResult persists a newly issued certificate.
func (h *Hub) storeResult(id string, domains []string, certPEM, keyPEM []byte, notBefore, notAfter time.Time, issuer string) error {
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
		Domains:   domains,
		CertPEM:   string(certPEM),
		KeyPEM:    string(keyPEM),
		NotBefore: notBefore,
		NotAfter:  notAfter,
		Issuer:    issuer,
		AutoRenew: true,
		CreatedAt: time.Now(),
	}
	return h.saveCertsLocked()
}

// markError records a failure on an existing certificate record.
func (h *Hub) markError(id, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.certs[id]; ok {
		c.LastError = msg
		c.Renewing = false
		_ = h.saveCertsLocked()
	}
}

// issueDNS01 runs the full ACME DNS-01 flow against dns-mng:
// register/reuse account key -> order -> authorizations -> TXT via dns-mng ->
// finalize -> download. It returns the issued certificate PEM chain + key.
func (h *Hub) issueDNS01(domains []string) (certPEM, keyPEM []byte, notBefore, notAfter time.Time, issuer string, err error) {
	cfg := h.GetConfig()
	if !cfg.Enabled || cfg.BaseURL == "" {
		return nil, nil, time.Time{}, time.Time{}, "", ErrDisabled
	}
	dirURL := cfg.DirectoryURL
	if dirURL == "" {
		dirURL = DefaultDirectoryURL
	}

	acmeClient := &acmeClient{
		directoryURL: dirURL,
		email:        cfg.Email,
		http:         &http.Client{Timeout: 30 * time.Second},
		log:          h.log,
	}

	// Account key: persisted so renewals reuse the same ACME account.
	accountKey, err := h.loadOrCreateAccountKey()
	if err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("account key: %w", err)
	}
	if err := acmeClient.ensureAccount(accountKey); err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("acme account: %w", err)
	}

	// Order + authorize each domain with DNS-01 via dns-mng.
	challenger := &dnsMngChallenge{baseURL: cfg.BaseURL, username: cfg.Username, password: cfg.Password, http: acmeClient.http}
	certPEM, keyPEM, notBefore, notAfter, issuer, err = acmeClient.obtainCertificate(accountKey, domains, challenger)
	if err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", err
	}
	return certPEM, keyPEM, notBefore, notAfter, issuer, nil
}

// Issue requests a new certificate for the domains. The call blocks (DNS-01
// propagation + ACME validation take tens of seconds); callers run it in a
// goroutine and poll.
func (h *Hub) Issue(domains []string) (*Certificate, error) {
	clean := make([]string, 0, len(domains))
	for _, d := range domains {
		d = strings.TrimSpace(strings.ToLower(d))
		if d != "" {
			clean = append(clean, d)
		}
	}
	if len(clean) == 0 {
		return nil, fmt.Errorf("至少需要一个域名")
	}
	certPEM, keyPEM, notBefore, notAfter, issuer, err := h.issueDNS01(clean)
	if err != nil {
		return nil, err
	}
	id := "crt_" + randomHex(8)
	if err := h.storeResult(id, clean, certPEM, keyPEM, notBefore, notAfter, issuer); err != nil {
		return nil, err
	}
	c, _ := h.Get(id)
	return c, nil
}

// Renew re-issues a certificate. When it succeeds the record is replaced in
// place; the old material is overwritten.
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

	certPEM, keyPEM, notBefore, notAfter, issuer, err := h.issueDNS01(c.Domains)
	if err != nil {
		h.markError(id, err.Error())
		return nil, err
	}
	if err := h.storeResult(id, c.Domains, certPEM, keyPEM, notBefore, notAfter, issuer); err != nil {
		h.markError(id, err.Error())
		return nil, err
	}
	updated, _ := h.Get(id)
	return updated, nil
}

// RunRenewalLoop periodically renews certificates expiring within the window.
// It returns immediately after launching the loop goroutine.
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

const renewBefore = 30 * 24 * time.Hour

func (h *Hub) renewExpiring() {
	for _, c := range h.List() {
		if !c.AutoRenew || c.Renewing {
			continue
		}
		if time.Until(c.NotAfter) > renewBefore {
			continue
		}
		h.log.Info("renewing certificate", "id", c.ID, "domains", c.Domains, "not_after", c.NotAfter)
		if _, err := h.Renew(c.ID); err != nil {
			h.log.Warn("certificate renewal failed", "id", c.ID, "err", err)
		}
	}
}

// loadOrCreateAccountKey keeps one ECDSA account key for all orders.
func (h *Hub) loadOrCreateAccountKey() (crypto.Signer, error) {
	path := filepath.Join(h.dir, "acme-account.key")
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
