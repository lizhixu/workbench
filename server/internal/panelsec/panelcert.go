package panelsec

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"watchman/server/internal/cert"
	"watchman/server/internal/settings"
)

// IssueJob tracks one async panel certificate issuance.
type IssueJob struct {
	ID        string    `json:"id"`
	Mode      string    `json:"mode"`   // "domain" | "ip"
	Target    string    `json:"target"` // domain name or IP literal
	Status    string    `json:"status"` // "running" | "done" | "error"
	CertID    string    `json:"cert_id,omitempty"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// PanelCertIssuer runs one-click panel certificate issuance and binds the
// resulting certificate to the panel automatically:
//   - mode "domain": issues for the bound panel domain (DNS-01 via the cert
//     center, requires dns-mng + an ACME account);
//   - mode "ip": issues a free IP certificate for a public IP literal
//     (HTTP-01 on port 80, e.g. Let's Encrypt IP certificates).
//
// On success the certificate is stored in the cert hub and the panel is
// switched to cert_center mode with SSL enabled.
type PanelCertIssuer struct {
	settings *settings.Store
	certHub  *cert.Hub
	log      *slog.Logger

	mu   sync.Mutex
	jobs map[string]*IssueJob
}

// NewPanelCertIssuer builds a panel certificate issuer.
func NewPanelCertIssuer(st *settings.Store, hub *cert.Hub, log *slog.Logger) *PanelCertIssuer {
	if log == nil {
		log = slog.Default()
	}
	return &PanelCertIssuer{settings: st, certHub: hub, log: log, jobs: map[string]*IssueJob{}}
}

func panelJobID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 0, 16)
	for _, v := range b {
		out = append(out, hexDigits[v>>4], hexDigits[v&0x0f])
	}
	return "piss_" + string(out)
}

// StartIssue validates the request and starts an async issuance job. A
// running job for the same target is reused instead of starting a second
// one (ACME rate limits are strict).
func (p *PanelCertIssuer) StartIssue(mode, ip string) (*IssueJob, error) {
	if p.settings == nil {
		return nil, fmt.Errorf("settings store not available")
	}
	var target string
	switch mode {
	case "domain":
		target = strings.TrimSpace(p.settings.PanelSecurity().PanelDomain)
		if target == "" {
			return nil, fmt.Errorf("尚未绑定面板域名，请先绑定域名")
		}
	case "ip":
		target = strings.TrimSpace(ip)
		if target == "" {
			detected, err := cert.DetectPublicIP()
			if err != nil {
				return nil, err
			}
			target = detected
		}
		parsed := net.ParseIP(target)
		if parsed == nil {
			return nil, fmt.Errorf("无效的 IP 地址: %s", target)
		}
		if !parsed.IsGlobalUnicast() || parsed.IsPrivate() {
			return nil, fmt.Errorf("只能为公网 IP 申请免费证书: %s", target)
		}
	default:
		return nil, fmt.Errorf("未知的签发模式: %s", mode)
	}
	if p.certHub == nil {
		return nil, fmt.Errorf("证书中心不可用")
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, j := range p.jobs {
		if j.Status == "running" && j.Target == target {
			cp := *j
			return &cp, nil
		}
	}
	// Prune finished jobs older than 30 minutes.
	for id, j := range p.jobs {
		if j.Status != "running" && time.Since(j.CreatedAt) > 30*time.Minute {
			delete(p.jobs, id)
		}
	}
	job := &IssueJob{ID: panelJobID(), Mode: mode, Target: target, Status: "running", CreatedAt: time.Now()}
	p.jobs[job.ID] = job
	cp := *job
	go p.run(job.ID, target)
	return &cp, nil
}

// GetJob returns a copy of the job with the given ID.
func (p *PanelCertIssuer) GetJob(id string) (*IssueJob, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	j, ok := p.jobs[id]
	if !ok {
		return nil, false
	}
	cp := *j
	return &cp, true
}

func (p *PanelCertIssuer) finish(id, certID, errMsg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	j, ok := p.jobs[id]
	if !ok {
		return
	}
	if errMsg != "" {
		j.Status = "error"
		j.Error = errMsg
	} else {
		j.Status = "done"
		j.CertID = certID
	}
}

func (p *PanelCertIssuer) run(id, target string) {
	p.log.Info("panel certificate issuance started", "job", id, "target", target)
	c, err := p.certHub.Issue([]string{target}, "")
	if err != nil {
		p.log.Warn("panel certificate issuance failed", "job", id, "target", target, "err", err)
		p.finish(id, "", "签发失败: "+err.Error())
		return
	}
	// Bind the new certificate to the panel and enable HTTPS.
	raw := func(s string) json.RawMessage {
		b, _ := json.Marshal(s)
		return b
	}
	err = p.settings.SetMany(settings.ScopeSystem, "", map[string]json.RawMessage{
		"security.panel_ssl_mode":    raw("cert_center"),
		"security.panel_ssl_cert_id": raw(c.ID),
		"security.panel_ssl_enabled": json.RawMessage("true"),
	})
	if err != nil {
		p.log.Warn("panel certificate issued but panel config update failed", "job", id, "cert", c.ID, "err", err)
		p.finish(id, c.ID, "证书签发成功，但写入面板配置失败: "+err.Error())
		return
	}
	p.log.Info("panel certificate issued and bound", "job", id, "cert", c.ID, "target", target)
	p.finish(id, c.ID, "")
}
