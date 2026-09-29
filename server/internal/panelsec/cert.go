package panelsec

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"watchman/server/internal/cert"
	"watchman/server/internal/settings"
)

// CertSummary describes metadata of an inspected SSL certificate.
type CertSummary struct {
	Subject       string    `json:"subject"`
	Domains       []string  `json:"domains"`
	Issuer        string    `json:"issuer"`
	NotBefore     time.Time `json:"not_before"`
	NotAfter      time.Time `json:"not_after"`
	DaysRemaining int       `json:"days_remaining"`
	Valid         bool      `json:"valid"`
}

// CertProvider resolves the active panel TLS certificate on demand from
// settings and the certificate hub, caching the parsed certificate in memory.
type CertProvider struct {
	settings *settings.Store
	certHub  *cert.Hub
	log      *slog.Logger

	mu           sync.RWMutex
	cacheKey     string
	cachedCert   *tls.Certificate
	fallbackCert *tls.Certificate
}

// NewCertProvider creates a dynamic certificate provider.
func NewCertProvider(st *settings.Store, hub *cert.Hub, fallback *tls.Certificate, log *slog.Logger) *CertProvider {
	if log == nil {
		log = slog.Default()
	}
	return &CertProvider{
		settings:     st,
		certHub:      hub,
		fallbackCert: fallback,
		log:          log,
	}
}

// GetCertificate implements the tls.Config.GetCertificate callback.
func (cp *CertProvider) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if cp.settings == nil {
		if cp.fallbackCert != nil {
			return cp.fallbackCert, nil
		}
		return nil, errors.New("no certificate configured")
	}

	sec := cp.settings.PanelSecurity()
	if !sec.SSLEnabled {
		if cp.fallbackCert != nil {
			return cp.fallbackCert, nil
		}
		return nil, errors.New("panel SSL is disabled")
	}

	var key string
	var certPEM, keyPEM string

	if sec.SSLMode == "custom" {
		certPEM = sec.SSLCertPEM
		keyPEM = sec.SSLKeyPEM
		if certPEM == "" || keyPEM == "" {
			if cp.fallbackCert != nil {
				return cp.fallbackCert, nil
			}
			return nil, errors.New("custom certificate or private key is empty")
		}
		key = "custom:" + certPEM + ":" + keyPEM
	} else {
		// cert_center mode
		if cp.certHub == nil {
			if cp.fallbackCert != nil {
				return cp.fallbackCert, nil
			}
			return nil, errors.New("certificate center hub unavailable")
		}

		var c *cert.Certificate
		if sec.SSLCertID != "" {
			c, _ = cp.certHub.Get(sec.SSLCertID)
		}
		if c == nil && hello.ServerName != "" {
			c, _ = cp.certHub.FindByDomain(hello.ServerName)
		}
		if c == nil && sec.PanelDomain != "" {
			c, _ = cp.certHub.FindByDomain(sec.PanelDomain)
		}
		if c == nil {
			list := cp.certHub.List()
			if len(list) > 0 {
				c, _ = cp.certHub.Get(list[0].ID)
			}
		}

		if c == nil || c.CertPEM == "" || c.KeyPEM == "" {
			if cp.fallbackCert != nil {
				return cp.fallbackCert, nil
			}
			return nil, fmt.Errorf("no certificate found for server %q", hello.ServerName)
		}

		certPEM = c.CertPEM
		keyPEM = c.KeyPEM
		key = "cert_center:" + c.ID + ":" + certPEM + ":" + keyPEM
	}

	cp.mu.RLock()
	if cp.cacheKey == key && cp.cachedCert != nil {
		c := cp.cachedCert
		cp.mu.RUnlock()
		return c, nil
	}
	cp.mu.RUnlock()

	parsed, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		cp.log.Error("failed to parse panel TLS certificate", "err", err)
		if cp.fallbackCert != nil {
			return cp.fallbackCert, nil
		}
		return nil, fmt.Errorf("invalid certificate pair: %w", err)
	}

	cp.mu.Lock()
	cp.cacheKey = key
	cp.cachedCert = &parsed
	cp.mu.Unlock()

	return &parsed, nil
}

// InspectActiveCert inspects and summarizes the currently configured certificate.
func (cp *CertProvider) InspectActiveCert() (*CertSummary, error) {
	if cp.settings == nil {
		return nil, errors.New("settings store not available")
	}
	sec := cp.settings.PanelSecurity()
	if !sec.SSLEnabled {
		return nil, nil
	}

	var certPEM string
	if sec.SSLMode == "custom" {
		certPEM = sec.SSLCertPEM
	} else if cp.certHub != nil {
		var c *cert.Certificate
		if sec.SSLCertID != "" {
			c, _ = cp.certHub.Get(sec.SSLCertID)
		}
		if c == nil && sec.PanelDomain != "" {
			c, _ = cp.certHub.FindByDomain(sec.PanelDomain)
		}
		if c == nil {
			list := cp.certHub.List()
			if len(list) > 0 {
				c, _ = cp.certHub.Get(list[0].ID)
			}
		}
		if c != nil {
			certPEM = c.CertPEM
		}
	}

	if certPEM == "" {
		return nil, errors.New("certificate content not found")
	}

	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return nil, errors.New("invalid PEM certificate")
	}
	x509Cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse x509 certificate: %w", err)
	}

	now := time.Now()
	remaining := int(time.Until(x509Cert.NotAfter).Hours() / 24)
	valid := now.After(x509Cert.NotBefore) && now.Before(x509Cert.NotAfter)

	domains := x509Cert.DNSNames
	if len(domains) == 0 && x509Cert.Subject.CommonName != "" {
		domains = []string{x509Cert.Subject.CommonName}
	}

	return &CertSummary{
		Subject:       x509Cert.Subject.CommonName,
		Domains:       domains,
		Issuer:        x509Cert.Issuer.CommonName,
		NotBefore:     x509Cert.NotBefore,
		NotAfter:      x509Cert.NotAfter,
		DaysRemaining: remaining,
		Valid:         valid,
	}, nil
}
