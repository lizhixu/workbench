package cert

import (
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"
	"time"
)

// AppDomainRef names one deployed application's bound domain, used by the
// used-by reverse lookup of the cert detail endpoint. Supplied by the router
// layer so the cert package stays decoupled from the apps store.
type AppDomainRef struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Domain string `json:"domain"`
}

// CertDetail extends the plain Certificate JSON with X.509 metadata parsed
// live from the stored chain plus a used-by reverse lookup. Served by
// GET /certs/:id/detail. The private key is never part of this payload;
// ChainPEM contains public certificate material only.
type CertDetail struct {
	*Certificate
	Source             string         `json:"source"` // acme | manual_dns01 | manual_upload
	AccountName        string         `json:"account_name,omitempty"`
	SubjectCN          string         `json:"subject_cn,omitempty"`
	SerialNumber       string         `json:"serial_number,omitempty"`
	SHA256Fingerprint  string         `json:"sha256_fingerprint,omitempty"`
	PublicKeyAlgorithm string         `json:"public_key_algorithm,omitempty"`
	SignatureAlgorithm string         `json:"signature_algorithm,omitempty"`
	ChainLength        int            `json:"chain_length,omitempty"`
	ValidNow           bool           `json:"valid_now"`
	NotYetValid        bool           `json:"not_yet_valid"`
	DaysRemaining      int            `json:"days_remaining"`
	AutoRenewDays      int            `json:"auto_renew_days"`
	PanelBound         bool           `json:"panel_bound"`
	PanelFallback      bool           `json:"panel_fallback"`
	UsedByApps         []AppDomainRef `json:"used_by_apps,omitempty"`
	ChainPEM           string         `json:"chain_pem,omitempty"`
	ParseError         string         `json:"parse_error,omitempty"`
}

// sourceOf derives the issuance provenance: no ACME account means the admin
// uploaded the PEM pair; an account with auto-renew off means it came from a
// two-phase manual DNS-01 order; anything else is a regular ACME issuance.
func (h *Hub) sourceOf(c *Certificate) string {
	switch {
	case c.AccountID == "":
		return "manual_upload"
	case !c.AutoRenew:
		return "manual_dns01"
	default:
		return "acme"
	}
}

// accountNameOf resolves the display name of the issuing ACME account.
func (h *Hub) accountNameOf(c *Certificate) string {
	if c.AccountID == "" {
		return ""
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if acc, ok := h.accounts[c.AccountID]; ok {
		return acc.Name
	}
	return ""
}

// CertView is the list-endpoint view: the persisted Certificate plus the
// server-derived source/account fields, so the UI never re-derives the
// provenance inference rules client-side.
type CertView struct {
	*Certificate
	Source      string `json:"source"`
	AccountName string `json:"account_name,omitempty"`
}

// ListView returns List() enriched with the derived source/account fields.
func (h *Hub) ListView() []*CertView {
	list := h.List()
	out := make([]*CertView, 0, len(list))
	for _, c := range list {
		out = append(out, &CertView{Certificate: c, Source: h.sourceOf(c), AccountName: h.accountNameOf(c)})
	}
	return out
}

// Detail returns the enriched certificate view for the detail dialog.
// panelCertID/panelDomain come from the panel security settings (settings
// keys security.panel_ssl_cert_id / security.panel_domain); apps lists all
// deployed applications with their bound domains. A leaf that fails to parse
// degrades to metadata + parse_error instead of failing the whole request.
func (h *Hub) Detail(id, panelCertID, panelDomain string, apps []AppDomainRef) (*CertDetail, bool) {
	c, ok := h.Get(id)
	if !ok {
		return nil, false
	}
	c.KeyPEM = "" // the private key never crosses the REST boundary
	d := &CertDetail{Certificate: c, AutoRenewDays: h.renewBeforeDaysOrDefault()}
	d.Source = h.sourceOf(c)
	d.AccountName = h.accountNameOf(c)

	if chain := strings.TrimSpace(c.CertPEM); chain != "" {
		d.ChainPEM = c.CertPEM
		if err := d.parseChain(chain); err != nil {
			d.ParseError = err.Error()
		}
	} else {
		d.ParseError = "证书材料缺失，无法解析"
	}

	now := time.Now()
	d.NotYetValid = now.Before(c.NotBefore)
	d.ValidNow = !d.NotYetValid && now.Before(c.NotAfter)
	// Same truncation as panelsec.CertSummary so the two surfaces agree.
	d.DaysRemaining = int(time.Until(c.NotAfter).Hours() / 24)

	if panelCertID == c.ID {
		d.PanelBound = true
	} else if panelCertID == "" && panelDomain != "" {
		// Mirror the panel's fallback: no explicit binding, the SNI pick for
		// the panel domain lands on this certificate.
		if m, ok := h.FindByDomain(panelDomain); ok && m.ID == c.ID {
			d.PanelFallback = true
		}
	}
	for _, a := range apps {
		if strings.TrimSpace(a.Domain) == "" {
			continue
		}
		if m, ok := h.FindByDomain(a.Domain); ok && m.ID == c.ID {
			d.UsedByApps = append(d.UsedByApps, a)
		}
	}
	return d, true
}

// parseChain fills the leaf-certificate fields and the chain length from a
// PEM bundle (leaf first, intermediates after). It counts every decodable
// CERTIFICATE block even when a non-leaf block is corrupt.
func (d *CertDetail) parseChain(chain string) error {
	var leaf *x509.Certificate
	rest := []byte(chain)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		pc, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			if leaf == nil {
				return fmt.Errorf("解析证书失败: %v", err)
			}
			continue
		}
		if leaf == nil {
			leaf = pc
		}
		d.ChainLength++
	}
	if leaf == nil {
		return fmt.Errorf("未在 PEM 中找到证书块")
	}
	d.SubjectCN = leaf.Subject.CommonName
	if d.SubjectCN == "" {
		d.SubjectCN = leaf.Subject.String()
	}
	d.SerialNumber = colonHex(leaf.SerialNumber.Bytes())
	sum := sha256.Sum256(leaf.Raw)
	d.SHA256Fingerprint = colonHex(sum[:])
	d.PublicKeyAlgorithm = publicKeyDescription(leaf.PublicKeyAlgorithm, leaf.PublicKey)
	d.SignatureAlgorithm = leaf.SignatureAlgorithm.String()
	return nil
}

// colonHex renders bytes as the openssl-style uppercase hex with colons
// (AA:BB:CC) used for serial numbers and fingerprints.
func colonHex(b []byte) string {
	parts := make([]string, len(b))
	for i, v := range b {
		parts[i] = fmt.Sprintf("%02X", v)
	}
	return strings.Join(parts, ":")
}

// publicKeyDescription renders the key algorithm with its size/curve,
// e.g. "RSA 2048", "ECDSA P-256", "Ed25519".
func publicKeyDescription(algo x509.PublicKeyAlgorithm, key any) string {
	switch algo {
	case x509.RSA:
		if k, ok := key.(*rsa.PublicKey); ok {
			return fmt.Sprintf("RSA %d", k.N.BitLen())
		}
		return "RSA"
	case x509.ECDSA:
		if k, ok := key.(*ecdsa.PublicKey); ok {
			if name := k.Curve.Params().Name; name != "" {
				return "ECDSA " + name
			}
		}
		return "ECDSA"
	case x509.Ed25519:
		return "Ed25519"
	default:
		return algo.String()
	}
}

// DomainCovers reports whether the SAN entry san (exact or wildcard) matches
// the normalized domain. Shared by FindByDomain, the used-by lookup and the
// apps-side renewal re-push so every surface applies the same matching rule.
func DomainCovers(san, domain string) bool {
	return san == domain || (strings.HasPrefix(san, "*.") && strings.HasSuffix(domain, strings.TrimPrefix(san, "*")))
}
