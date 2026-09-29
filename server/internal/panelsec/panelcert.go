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
	ID        string `json:"id"`
	Mode      string `json:"mode"`                // "domain" | "ip"
	Target    string `json:"target"`              // domain name or IP literal
	Challenge string `json:"challenge,omitempty"` // "" (auto) | "http-01" | "dns-01" | "dns-01-manual"
	Status    string `json:"status"`              // "running" | "awaiting_dns" | "done" | "error"
	CertID    string `json:"cert_id,omitempty"`
	Error     string `json:"error,omitempty"`
	// PendingID and DNSRecords are set for manual DNS-01 ("dns-01-manual"):
	// the administrator provisions the TXT records, then confirms via
	// ConfirmIssue.
	PendingID  string           `json:"pending_id,omitempty"`
	DNSRecords []cert.DNSRecord `json:"dns_records,omitempty"`
	// Terminal mirrors the underlying manual order: when true the CA already
	// reached a final verdict and the job must not be confirmed again — start
	// a new issuance instead.
	Terminal   bool             `json:"terminal,omitempty"`
	CreatedAt  time.Time        `json:"created_at"`
}

// PanelCertIssuer runs one-click panel certificate issuance and binds the
// resulting certificate to the panel automatically:
//   - mode "domain": issues for the bound panel domain (HTTP-01 preferred,
//     DNS-01 via the cert center's dns-mng as fallback; requires an ACME account);
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
func (p *PanelCertIssuer) StartIssue(mode, ip, challenge string) (*IssueJob, error) {
	if p.settings == nil {
		return nil, fmt.Errorf("settings store not available")
	}
	ch := cert.ChallengePreference(strings.TrimSpace(challenge))
	if !ch.Valid() {
		return nil, fmt.Errorf("未知的验证方式: %q", challenge)
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
	// Manual DNS-01 is a two-phase flow: create the CA order now (network
	// I/O, done outside the job lock), show the TXT records to the
	// administrator, and wait for ConfirmIssue — no validation is triggered
	// yet. IP identifiers cannot use DNS-01 (RFC 8738), so manual mode is
	// domain-only.
	var manualOrder *cert.PendingOrder
	if ch == cert.ChallengeDNS01Manual {
		if mode != "domain" {
			return nil, fmt.Errorf("DNS-01 手动验证仅支持域名，IP 证书请使用 HTTP-01")
		}
		order, err := p.certHub.StartManualDNSOrder([]string{target}, "")
		if err != nil {
			return nil, err
		}
		manualOrder = order
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, j := range p.jobs {
		if (j.Status == "running" || j.Status == "awaiting_dns") && j.Target == target {
			cp := *j
			return &cp, nil
		}
	}
	// Prune finished jobs older than 30 minutes.
	for id, j := range p.jobs {
		if j.Status != "running" && j.Status != "awaiting_dns" && time.Since(j.CreatedAt) > 30*time.Minute {
			delete(p.jobs, id)
		}
	}
	job := &IssueJob{ID: panelJobID(), Mode: mode, Target: target, Challenge: string(ch), Status: "running", CreatedAt: time.Now()}
	if manualOrder != nil {
		job.Status = "awaiting_dns"
		job.PendingID = manualOrder.ID
		job.DNSRecords = manualOrder.Records
		p.jobs[job.ID] = job
		cp := *job
		return &cp, nil
	}
	p.jobs[job.ID] = job
	cp := *job
	go p.run(job.ID, target, ch)
	return &cp, nil
}

// ConfirmIssue starts CA validation for a manual DNS-01 job ("awaiting_dns").
// The administrator must have provisioned the TXT records first. On success
// the certificate is bound to the panel and HTTPS is enabled, like the
// automatic flow. A job that failed verification may be confirmed again,
// unless the underlying order reached a terminal CA verdict (Terminal=true),
// in which case a new issuance must be started.
func (p *PanelCertIssuer) ConfirmIssue(id string) (*IssueJob, error) {
	p.mu.Lock()
	j, ok := p.jobs[id]
	if !ok {
		p.mu.Unlock()
		return nil, fmt.Errorf("签发任务不存在或已过期")
	}
	if j.Challenge != string(cert.ChallengeDNS01Manual) {
		p.mu.Unlock()
		return nil, fmt.Errorf("该任务不是手动 DNS-01 验证，无需确认")
	}
	if j.Status != "awaiting_dns" && j.Status != "error" {
		p.mu.Unlock()
		return nil, fmt.Errorf("任务当前状态为 %s，无法确认验证", j.Status)
	}
	if j.Status == "error" && j.Terminal {
		p.mu.Unlock()
		return nil, fmt.Errorf("CA 已对该订单作出终态判定（验证不通过），请重新发起签发")
	}
	j.Status = "running"
	j.Error = ""
	cp := *j
	pendingID := j.PendingID
	p.mu.Unlock()

	go p.runManualConfirm(id, pendingID)
	return &cp, nil
}

func (p *PanelCertIssuer) runManualConfirm(id, pendingID string) {
	p.log.Info("panel manual dns-01 confirmation started", "job", id, "pending", pendingID)
	c, err := p.certHub.ConfirmManualDNSOrder(pendingID)
	if err != nil {
		p.log.Warn("panel manual dns-01 confirmation failed", "job", id, "err", err)
		p.finish(id, "", "验证失败: "+err.Error())
		// Surface whether the underlying order may be retried, so the UI can
		// hide the re-confirm button for terminal CA verdicts.
		if order, gerr := p.certHub.GetPendingOrder(pendingID); gerr == nil {
			p.mu.Lock()
			if j, ok := p.jobs[id]; ok {
				j.Terminal = order.Terminal
			}
			p.mu.Unlock()
		}
		return
	}
	if err := p.bindPanelCertificate(c.ID); err != nil {
		p.log.Warn("panel certificate issued but panel config update failed", "job", id, "cert", c.ID, "err", err)
		p.finish(id, c.ID, "证书签发成功，但写入面板配置失败: "+err.Error())
		return
	}
	p.log.Info("panel certificate issued and bound", "job", id, "cert", c.ID)
	p.finish(id, c.ID, "")
}

// CancelIssue discards a manual DNS-01 job that is no longer needed (e.g.
// the administrator gave up adding the TXT records). The underlying ACME
// pending order is cancelled as well; the CA-side order is simply left to
// expire.
func (p *PanelCertIssuer) CancelIssue(id string) error {
	p.mu.Lock()
	j, ok := p.jobs[id]
	if !ok {
		p.mu.Unlock()
		return fmt.Errorf("签发任务不存在或已过期")
	}
	if j.Challenge != string(cert.ChallengeDNS01Manual) {
		p.mu.Unlock()
		return fmt.Errorf("该任务不是手动 DNS-01 验证，无需取消")
	}
	if j.Status == "running" {
		p.mu.Unlock()
		return fmt.Errorf("任务正在验证中，请稍候再取消")
	}
	pendingID := j.PendingID
	delete(p.jobs, id)
	p.mu.Unlock()
	if pendingID != "" {
		if err := p.certHub.CancelManualDNSOrder(pendingID); err != nil {
			p.log.Warn("panel job cancelled but pending order cleanup failed", "job", id, "err", err)
		}
	}
	return nil
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

func (p *PanelCertIssuer) run(id, target string, ch cert.ChallengePreference) {
	p.log.Info("panel certificate issuance started", "job", id, "target", target, "challenge", string(ch))
	c, err := p.certHub.IssueWithChallenge([]string{target}, "", ch)
	if err != nil {
		p.log.Warn("panel certificate issuance failed", "job", id, "target", target, "err", err)
		p.finish(id, "", "签发失败: "+err.Error())
		return
	}
	// Bind the new certificate to the panel and enable HTTPS.
	if err := p.bindPanelCertificate(c.ID); err != nil {
		p.log.Warn("panel certificate issued but panel config update failed", "job", id, "cert", c.ID, "err", err)
		p.finish(id, c.ID, "证书签发成功，但写入面板配置失败: "+err.Error())
		return
	}
	p.log.Info("panel certificate issued and bound", "job", id, "cert", c.ID, "target", target)
	p.finish(id, c.ID, "")
}

// bindPanelCertificate switches the panel to cert_center mode using the
// given certificate and enables HTTPS.
func (p *PanelCertIssuer) bindPanelCertificate(certID string) error {
	raw := func(s string) json.RawMessage {
		b, _ := json.Marshal(s)
		return b
	}
	return p.settings.SetMany(settings.ScopeSystem, "", map[string]json.RawMessage{
		"security.panel_ssl_mode":    raw("cert_center"),
		"security.panel_ssl_cert_id": raw(certID),
		"security.panel_ssl_enabled": json.RawMessage("true"),
	})
}
