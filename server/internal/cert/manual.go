package cert

import (
	"context"
	"crypto"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DNSRecord is one TXT record the administrator must provision manually at
// their DNS provider before the CA can validate the DNS-01 challenge.
type DNSRecord struct {
	Domain string `json:"domain"` // identifier value, e.g. "example.com" or "*.example.com"
	Host   string `json:"host"`   // "_acme-challenge.example.com"
	Type   string `json:"type"`   // always "TXT"
	Value  string `json:"value"`  // base64url(sha256(keyAuthorization))
}

// pendingAuthz tracks one authorization of a manual DNS-01 order. The
// challenge has been issued by the CA but not yet triggered; triggering
// happens when the administrator confirms the TXT records are in place.
type pendingAuthz struct {
	URL             string `json:"url"`
	IdentifierType  string `json:"identifier_type"`
	IdentifierValue string `json:"identifier_value"`
	ChallengeURL    string `json:"challenge_url"`
	Token           string `json:"token"`
}

// Pending-order lifecycle states.
const (
	ManualOrderAwaitingDNS = "awaiting_dns" // records shown, waiting for the admin to provision them
	ManualOrderVerifying   = "verifying"    // admin confirmed; CA validation in progress
	ManualOrderDone        = "done"         // certificate issued
	// ManualOrderError means the attempt failed. Whether the same order may
	// be confirmed again is tracked by PendingOrder.Terminal: a local
	// pre-check failure (nothing sent to the CA) is retryable, while a CA
	// terminal verdict (authorization invalid/expired) kills the order and
	// the administrator must start a new one.
	ManualOrderError       = "error"
)

// manualOrderTTL bounds how long a pending order is kept. ACME
// authorizations stay pending only for a limited time server-side, so an
// order nobody confirms within the TTL is discarded.
const manualOrderTTL = 2 * time.Hour

// PendingOrder is a manual DNS-01 issuance: the CA order exists, the DNS-01
// challenges are known, and the flow pauses until the administrator adds the
// TXT records at their DNS provider and confirms.
type PendingOrder struct {
	ID          string         `json:"id"`
	AccountID   string         `json:"account_id"`
	Identifiers []string       `json:"identifiers"`
	Records     []DNSRecord    `json:"records"`
	OrderURL    string         `json:"-"`
	FinalizeURL string         `json:"-"`
	Authz       []pendingAuthz `json:"-"`
	Status      string         `json:"status"`
	CertID      string         `json:"cert_id,omitempty"`
	Error       string         `json:"error,omitempty"`
	// Terminal marks an error state that must not be retried on the same
	// order: the CA already reached a terminal verdict (authorization
	// invalid/expired), so the administrator has to start a new order.
	// A non-terminal error (local pre-check, network blip, timeout) is safe
	// to confirm again — the TXT records are unchanged.
	Terminal    bool           `json:"terminal,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	ExpiresAt   time.Time      `json:"expires_at"`
}

// persistedPendingOrder is the on-disk form of a PendingOrder. The ACME
// order URLs and challenge tokens must survive restarts (the confirm phase
// needs them) but must never be exposed over REST, so persistence uses this
// separate shape instead of the json:"-" tags on PendingOrder.
type persistedPendingOrder struct {
	ID          string         `json:"id"`
	AccountID   string         `json:"account_id"`
	Identifiers []string       `json:"identifiers"`
	Records     []DNSRecord    `json:"records"`
	OrderURL    string         `json:"order_url"`
	FinalizeURL string         `json:"finalize_url"`
	Authz       []pendingAuthz `json:"authz"`
	Status      string         `json:"status"`
	CertID      string         `json:"cert_id,omitempty"`
	Error       string         `json:"error,omitempty"`
	Terminal    bool           `json:"terminal,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	ExpiresAt   time.Time      `json:"expires_at"`
}

func (p *PendingOrder) toPersisted() *persistedPendingOrder {
	return &persistedPendingOrder{
		ID: p.ID, AccountID: p.AccountID, Identifiers: p.Identifiers,
		Records: p.Records, OrderURL: p.OrderURL, FinalizeURL: p.FinalizeURL,
		Authz: p.Authz, Status: p.Status, CertID: p.CertID, Error: p.Error,
		Terminal: p.Terminal,
		CreatedAt: p.CreatedAt, ExpiresAt: p.ExpiresAt,
	}
}

func (pp *persistedPendingOrder) toPending() *PendingOrder {
	return &PendingOrder{
		ID: pp.ID, AccountID: pp.AccountID, Identifiers: pp.Identifiers,
		Records: pp.Records, OrderURL: pp.OrderURL, FinalizeURL: pp.FinalizeURL,
		Authz: pp.Authz, Status: pp.Status, CertID: pp.CertID, Error: pp.Error,
		Terminal: pp.Terminal,
		CreatedAt: pp.CreatedAt, ExpiresAt: pp.ExpiresAt,
	}
}

func (h *Hub) pendingDir() string { return filepath.Join(h.dir, "pending") }

func (h *Hub) pendingPath(id string) string { return filepath.Join(h.pendingDir(), id+".json") }

// savePendingLocked persists a pending order atomically (write temp + rename).
// Callers must hold h.mu.
func (h *Hub) savePendingLocked(p *PendingOrder) error {
	if err := os.MkdirAll(h.pendingDir(), 0700); err != nil {
		return err
	}
	b, err := marshalJSON(p.toPersisted())
	if err != nil {
		return err
	}
	tmp := h.pendingPath(p.ID) + fmt.Sprintf(".tmp.%d", time.Now().UnixNano())
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, h.pendingPath(p.ID))
}

func (h *Hub) loadPending(id string) (*PendingOrder, error) {
	b, err := os.ReadFile(h.pendingPath(id))
	if err != nil {
		return nil, err
	}
	var pp persistedPendingOrder
	if err := unmarshalJSON(b, &pp); err != nil {
		return nil, err
	}
	return pp.toPending(), nil
}

// recoverInterruptedPending flips orders that were mid-verification when
// the process stopped back to a retryable error: the in-flight CA round-trip
// is gone, but the TXT records are unchanged, so confirming again simply
// resumes (re-triggering challenges is idempotent; the CA reports the
// current authorization state). Without this a restart would leave orders
// stuck in "verifying" forever.
func (h *Hub) recoverInterruptedPending() {
	entries, err := os.ReadDir(h.pendingDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if !validPendingID(id) {
			continue
		}
		p, err := h.loadPending(id)
		if err != nil || p.Status != ManualOrderVerifying {
			continue
		}
		p.Status = ManualOrderError
		p.Error = "服务重启导致验证中断，可直接重新确认验证"
		p.Terminal = false
		if err := h.savePendingLocked(p); err != nil {
			h.log.Warn("recover interrupted manual order failed", "id", id, "err", err)
			continue
		}
		h.log.Info("recovered interrupted manual dns-01 order", "id", id)
	}
}

// pruneExpiredPending drops pending orders past their TTL.
func (h *Hub) pruneExpiredPending() {
	entries, err := os.ReadDir(h.pendingDir())
	if err != nil {
		return
	}
	now := time.Now()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		p, err := h.loadPending(id)
		if err != nil || now.After(p.ExpiresAt) {
			_ = os.Remove(h.pendingPath(id))
		}
	}
}

// manualClient builds an authenticated ACME client for the given account,
// shared by the start and confirm phases of a manual DNS-01 order.
func (h *Hub) manualClient(accountID string) (*acmeClient, crypto.Signer, *ACMEAccount, error) {
	var acc *ACMEAccount
	if accountID != "" {
		got, ok := h.GetAccount(accountID)
		if !ok {
			return nil, nil, nil, fmt.Errorf("ACME 机构账户 %s 不存在", accountID)
		}
		acc = got
	} else {
		got, err := h.GetDefaultAccount()
		if err != nil {
			return nil, nil, nil, err
		}
		acc = got
	}
	accountKey, err := h.loadOrCreateAccountKey(acc.ID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("account key: %w", err)
	}
	dirURL := acc.DirectoryURL
	if dirURL == "" {
		dirURL = DefaultDirectoryURL
	}
	client := &acmeClient{
		directoryURL: dirURL,
		email:        acc.Email,
		eabKeyID:     acc.EABKeyID,
		eabHMACKey:   acc.EABHMACKey,
		http:         &http.Client{Timeout: 30 * time.Second},
		log:          h.log,
	}
	if err := client.ensureAccount(accountKey); err != nil {
		return nil, nil, nil, fmt.Errorf("acme account (%s): %w", acc.Name, err)
	}
	return client, accountKey, acc, nil
}

// validPendingID rejects anything that is not a generated pending-order ID,
// so a crafted :id can never escape the pending directory via pendingPath.
func validPendingID(id string) bool {
	if !strings.HasPrefix(id, "mord_") || len(id) != len("mord_")+16 {
		return false
	}
	for _, r := range id[len("mord_"):] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// StartManualDNSOrder creates an ACME order for a manual DNS-01 issuance and
// returns the TXT records the administrator must provision. It stops before
// triggering any challenge: the order stays pending until ConfirmManualDNSOrder
// is called. IP literals are rejected — RFC 8738 restricts them to HTTP-01.
//
// Network I/O (account setup, order creation, authorization fetch) runs
// without holding the hub lock; only the final persist takes it briefly.
func (h *Hub) StartManualDNSOrder(identifiers []string, accountID string) (*PendingOrder, error) {
	clean := make([]string, 0, len(identifiers))
	for _, d := range identifiers {
		d = strings.TrimSpace(strings.ToLower(d))
		if d == "" {
			continue
		}
		if identifierType(d) == "ip" {
			return nil, fmt.Errorf("IP 地址 %q 仅支持 HTTP-01 验证，不能使用 DNS-01 手动验证", d)
		}
		clean = append(clean, d)
	}
	if len(clean) == 0 {
		return nil, fmt.Errorf("至少需要一个域名")
	}

	client, accountKey, acc, err := h.manualClient(accountID)
	if err != nil {
		return nil, err
	}
	orderURL, order, err := client.createOrder(accountKey, clean)
	if err != nil {
		return nil, err
	}
	thumbprint, err := keyThumbprint(accountKey)
	if err != nil {
		return nil, err
	}

	p := &PendingOrder{
		ID:          "mord_" + randomHex(8),
		AccountID:   acc.ID,
		Identifiers: clean,
		OrderURL:    orderURL,
		FinalizeURL: order.Finalize,
		Status:      ManualOrderAwaitingDNS,
		CreatedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(manualOrderTTL),
	}
	for _, authzURL := range order.Authorizations {
		authz, err := client.fetchAuthorization(authzURL, accountKey)
		if err != nil {
			return nil, err
		}
		if authz.Status == "valid" {
			continue // already validated (e.g. reused authorization); no record needed
		}
		var dnsCh *authzChallenge
		for i := range authz.Challenges {
			if authz.Challenges[i].Type == "dns-01" {
				dnsCh = &authz.Challenges[i]
				break
			}
		}
		if dnsCh == nil {
			return nil, fmt.Errorf("CA 未为 %q 提供 dns-01 验证方式", authz.Identifier.Value)
		}
		keyAuthz := dnsCh.Token + "." + thumbprint
		rec := DNSRecord{
			Domain: authz.Identifier.Value,
			Host:   dns01RecordHost(authz.Identifier.Value),
			Type:   "TXT",
			Value:  dns01TXTValue(keyAuthz),
		}
		p.Records = append(p.Records, rec)
		p.Authz = append(p.Authz, pendingAuthz{
			URL:             authzURL,
			IdentifierType:  authz.Identifier.Type,
			IdentifierValue: authz.Identifier.Value,
			ChallengeURL:    dnsCh.URL,
			Token:           dnsCh.Token,
		})
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneExpiredPending()
	if err := h.savePendingLocked(p); err != nil {
		return nil, err
	}
	h.log.Info("manual dns-01 order created", "id", p.ID, "identifiers", clean, "records", len(p.Records))
	cp := *p
	return &cp, nil
}

// GetPendingOrder returns a copy of the pending manual DNS-01 order.
func (h *Hub) GetPendingOrder(id string) (*PendingOrder, error) {
	if !validPendingID(id) {
		return nil, fmt.Errorf("手动签发任务不存在或已过期")
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	p, err := h.loadPending(id)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("手动签发任务不存在或已过期")
		}
		return nil, err
	}
	if time.Now().After(p.ExpiresAt) {
		return nil, fmt.Errorf("手动签发任务已过期，请重新发起")
	}
	cp := *p
	return &cp, nil
}

// checkDNSTXT is a best-effort sanity check before triggering the CA:
// verify the expected TXT value is visible for the record host. Triggering
// with missing records burns the authorization (the CA marks it invalid and
// the whole order must be recreated), so a clear local error here saves the
// administrator from a wasted round-trip.
func checkDNSTXT(host, want string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	txts, err := net.DefaultResolver.LookupTXT(ctx, host)
	if err != nil {
		return fmt.Errorf("查询 %s 的 TXT 记录失败: %v（请确认已添加解析并等待生效）", host, err)
	}
	for _, t := range txts {
		if t == want {
			return nil
		}
	}
	return fmt.Errorf("未在 %s 查到期望的 TXT 记录值（请确认已添加解析并等待其生效）", host)
}

// dnsTXTLookup is the hook ConfirmManualDNSOrder uses for the pre-trigger
// DNS sanity check. It is a variable so tests can stub it (the fake test CA
// has no real DNS).
var dnsTXTLookup = checkDNSTXT

// ConfirmManualDNSOrder triggers the DNS-01 challenges of a pending order,
// waits for the CA to validate them, finalizes the order and stores the
// certificate. It may be retried from a non-terminal error state (records are
// unchanged, so the administrator does not need to re-provision them); a
// terminal error means the CA already gave a final verdict on this order and
// a fresh order must be started instead.
func (h *Hub) ConfirmManualDNSOrder(id string) (*Certificate, error) {
	if !validPendingID(id) {
		return nil, fmt.Errorf("手动签发任务不存在或已过期")
	}
	h.mu.Lock()
	p, err := h.loadPending(id)
	if err != nil {
		h.mu.Unlock()
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("手动签发任务不存在或已过期")
		}
		return nil, err
	}
	if time.Now().After(p.ExpiresAt) {
		h.mu.Unlock()
		return nil, fmt.Errorf("手动签发任务已过期，请重新发起")
	}
	if p.Status != ManualOrderAwaitingDNS && p.Status != ManualOrderError {
		h.mu.Unlock()
		return nil, fmt.Errorf("任务当前状态为 %s，无法确认验证", manualOrderStatusLabel(p.Status))
	}
	if p.Status == ManualOrderError && p.Terminal {
		h.mu.Unlock()
		return nil, fmt.Errorf("CA 已对该订单作出终态判定（验证不通过），请重新发起签发")
	}
	p.Status = ManualOrderVerifying
	p.Error = ""
	p.Terminal = false
	if err := h.savePendingLocked(p); err != nil {
		h.mu.Unlock()
		return nil, err
	}
	h.mu.Unlock()

	// fail records the error on the pending order. terminal=false means the
	// same order may be confirmed again (nothing was sent to the CA, or the
	// failure was transient); terminal=true means the CA already reached a
	// final verdict on this order and it must not be retried.
	fail := func(msg string, err error, terminal bool) (*Certificate, error) {
		full := msg
		if err != nil {
			full = msg + ": " + err.Error()
		}
		h.mu.Lock()
		if cur, lerr := h.loadPending(id); lerr == nil {
			cur.Status = ManualOrderError
			cur.Error = full
			cur.Terminal = terminal
			_ = h.savePendingLocked(cur)
		}
		h.mu.Unlock()
		h.log.Warn("manual dns-01 order failed", "id", id, "err", full, "terminal", terminal)
		return nil, fmt.Errorf("%s", full)
	}

	// Sanity check first: every expected TXT value must be visible before
	// any challenge is triggered.
	wantByDomain := make(map[string]string, len(p.Records))
	for _, r := range p.Records {
		wantByDomain[r.Domain] = r.Value
	}
	for _, az := range p.Authz {
		host := dns01RecordHost(az.IdentifierValue)
		if err := dnsTXTLookup(host, wantByDomain[az.IdentifierValue]); err != nil {
			return fail("DNS 预检查未通过（尚未通知 CA，不会消耗验证机会）", err, false)
		}
	}

	client, accountKey, acc, err := h.manualClient(p.AccountID)
	if err != nil {
		return fail("ACME 账户准备失败", err, false)
	}

	// Trigger every challenge, then wait for the CA to validate.
	// A longer deadline is used here than in the automatic flow because the
	// records were provisioned by hand and DNS propagation takes longer.
	for _, az := range p.Authz {
		if err := client.triggerChallenge(az.ChallengeURL, accountKey); err != nil {
			return fail(fmt.Sprintf("通知 CA 验证 %q 失败", az.IdentifierValue), err, false)
		}
		if err := client.pollAuthorizationWithTimeout(az.URL, accountKey, 10*time.Minute); err != nil {
			// An invalid/expired authorization is a terminal CA verdict: the
			// order is dead and must be recreated. A timeout is transient —
			// the authorization may still flip to valid, so retrying is safe.
			terminal := strings.Contains(err.Error(), "authorization invalid") ||
				strings.Contains(err.Error(), "authorization expired")
			msg := fmt.Sprintf("CA 验证 %q 未通过（请检查 TXT 解析是否已全球生效）", az.IdentifierValue)
			if terminal {
				msg = fmt.Sprintf("CA 已判定 %q 验证失败，该订单不可再用，请重新发起签发", az.IdentifierValue)
			}
			return fail(msg, err, terminal)
		}
	}

	// Re-fetch the order, wait until ready, finalize, download.
	body, err := client.postAsGet(p.OrderURL, client.kid, accountKey)
	if err != nil {
		return fail("查询订单状态失败", err, false)
	}
	var order acmeOrder
	if err := json.Unmarshal(body, &order); err != nil {
		return fail("解析订单状态失败", err, false)
	}
	if order.Finalize == "" {
		order.Finalize = p.FinalizeURL
	}
	if err := client.waitOrderReady(p.OrderURL, &order, accountKey); err != nil {
		// The order went invalid while waiting: terminal, start over.
		return fail("等待订单就绪失败", err, true)
	}
	certPEM, keyPEM, notBefore, notAfter, issuer, err := client.finalizeOrder(p.OrderURL, &order, accountKey, p.Identifiers)
	if err != nil {
		// Finalize state is uncertain after a transport error; the safe
		// choice is a fresh order rather than guessing.
		return fail("签发证书失败", err, true)
	}
	if issuer == "" {
		issuer = acc.Name
	}
	certID := "crt_" + randomHex(8)
	if err := h.storeResult(certID, acc.ID, p.Identifiers, certPEM, keyPEM, notBefore, notAfter, issuer); err != nil {
		// The certificate was issued but could not be stored locally; the
		// order is consumed, so a fresh order is required.
		return fail("保存证书失败", err, true)
	}

	h.mu.Lock()
	if cur, lerr := h.loadPending(id); lerr == nil {
		cur.Status = ManualOrderDone
		cur.Error = ""
		cur.CertID = certID
		_ = h.savePendingLocked(cur)
	}
	h.mu.Unlock()
	h.log.Info("manual dns-01 order completed", "id", id, "cert", certID)
	c, _ := h.Get(certID)
	return c, nil
}

// CancelManualDNSOrder discards a pending order that is no longer needed.
// The CA-side order simply expires; already-provisioned TXT records can be
// removed by the administrator at their DNS provider.
func (h *Hub) CancelManualDNSOrder(id string) error {
	if !validPendingID(id) {
		return fmt.Errorf("手动签发任务不存在或已过期")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	p, err := h.loadPending(id)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("手动签发任务不存在或已过期")
		}
		return err
	}
	if p.Status == ManualOrderVerifying {
		return fmt.Errorf("任务正在验证中，请稍候")
	}
	return os.Remove(h.pendingPath(id))
}

func manualOrderStatusLabel(status string) string {
	switch status {
	case ManualOrderAwaitingDNS:
		return "等待添加 DNS 解析"
	case ManualOrderVerifying:
		return "CA 验证中"
	case ManualOrderDone:
		return "已完成"
	case ManualOrderError:
		return "验证失败"
	default:
		return status
	}
}
