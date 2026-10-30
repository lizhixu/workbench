package cert

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// marshalJSON/unmarshalJSON are tiny indirections so the hub file does not
// import encoding/json twice with different needs; kept here for symmetry.
func marshalJSON(v any) ([]byte, error)   { return json.MarshalIndent(v, "", "  ") }
func unmarshalJSON(b []byte, v any) error { return json.Unmarshal(b, v) }

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 0, n*2)
	for _, v := range b {
		out = append(out, hexDigits[v>>4], hexDigits[v&0x0f])
	}
	return string(out)
}

// identifierType classifies an ACME identifier per RFC 8738: IP literals use
// the "ip" type, everything else is a "dns" name.
func identifierType(v string) string {
	if net.ParseIP(strings.TrimSpace(v)) != nil {
		return "ip"
	}
	return "dns"
}

// acmeError extracts the error detail from an RFC 8555 problem JSON response.
func acmeError(action string, status int, body []byte) error {
	var prob struct {
		Type   string `json:"type"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(body, &prob) == nil && prob.Detail != "" {
		if prob.Type != "" {
			return fmt.Errorf("%s: status %d (%s): %s", action, status, prob.Type, prob.Detail)
		}
		return fmt.Errorf("%s: status %d: %s", action, status, prob.Detail)
	}
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 300 {
		snippet = snippet[:300]
	}
	if snippet != "" {
		return fmt.Errorf("%s: status %d %s", action, status, snippet)
	}
	return fmt.Errorf("%s: status %d", action, status)
}

// http01Challenge serves ACME HTTP-01 responses for the duration of an
// issuance. Public CAs only validate http-01 on port 80, so the listener is
// bound briefly (challenge lifetime) and released afterwards; it is only
// needed for "ip" identifiers (e.g. Let's Encrypt IP certificates), which
// cannot use DNS-01 without control of reverse DNS.
type http01Challenge struct {
	mu     sync.Mutex
	values map[string]string // token -> keyAuthorization

	// addr is the listen address; defaults to ":80" (the only port public
	// CAs validate http-01 on). Tests may override it.
	addr string

	ln  net.Listener
	srv *http.Server
}

func (h *http01Challenge) start() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ln != nil {
		return nil
	}
	addr := h.addr
	if addr == "" {
		addr = ":80"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("http-01 验证需要临时监听 80 端口: %w", err)
	}
	h.values = make(map[string]string)
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/acme-challenge/", func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.URL.Path, "/.well-known/acme-challenge/")
		h.mu.Lock()
		v, ok := h.values[token]
		h.mu.Unlock()
		if !ok || token == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, v)
	})
	// Capture the server in a local: the Serve goroutine must not read h.srv,
	// which stop() may nil concurrently (e.g. when a pinned challenge
	// preference fails fast right after the listener was probed).
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	h.srv = srv
	h.ln = ln
	go func() {
		_ = srv.Serve(ln) // closed by stop()
	}()
	return nil
}

func (h *http01Challenge) present(token, keyAuthz string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.values == nil {
		h.values = make(map[string]string)
	}
	h.values[token] = keyAuthz
}

func (h *http01Challenge) stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.srv != nil {
		_ = h.srv.Close()
		h.srv = nil
	}
	h.ln = nil
	h.values = nil
}

// dnsMngChallenge implements the DNS-01 challenge by delegating TXT record
// creation/removal to a dns-mng instance (sibling project) which already
// knows which provider account owns each domain. It satisfies challengeSolver.
type dnsMngChallenge struct {
	baseURL            string
	username, password string
	http               *http.Client
	log                *slog.Logger
}

func (d *dnsMngChallenge) call(path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(d.baseURL, "/")+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(d.username, d.password)
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("dns-mng %s: %d %s", path, resp.StatusCode, string(snippet))
	}
	return nil
}

// presentTXT creates the _acme-challenge TXT record for fqdn.
func (d *dnsMngChallenge) presentTXT(fqdn, value string) error {
	return d.call("/api/acme/dns01/present", map[string]any{
		"fqdn":  fqdn,
		"value": value,
		"ttl":   300,
	})
}

// cleanupTXT removes the TXT record after validation.
func (d *dnsMngChallenge) cleanupTXT(fqdn, value string) error {
	return d.call("/api/acme/dns01/cleanup", map[string]any{
		"fqdn":  fqdn,
		"value": value,
	})
}

// acmeClient is a minimal RFC 8555 client supporting the DNS-01 and HTTP-01
// flows: account creation, new-order, authorization challenges, finalize,
// download. DNS identifiers use DNS-01 (delegated to dns-mng); IP
// identifiers (RFC 8738) use HTTP-01 served on port 80.
type acmeClient struct {
	directoryURL string
	email        string
	eabKeyID     string
	eabHMACKey   string
	http         *http.Client
	log          *slog.Logger
	clog         *certLogger

	// directory cache
	dir map[string]string
	kid string
}

func (a *acmeClient) logStage(stage, msg string) {
	if a.clog != nil {
		a.clog.Log(stage, msg)
	}
}

func (a *acmeClient) logStagef(stage, format string, args ...any) {
	if a.clog != nil {
		a.clog.Logf(stage, format, args...)
	}
}

func (a *acmeClient) warnStage(stage, msg string) {
	if a.clog != nil {
		a.clog.Warn(stage, msg)
	}
}

func (a *acmeClient) warnStagef(stage, format string, args ...any) {
	if a.clog != nil {
		a.clog.Warnf(stage, format, args...)
	}
}

type acmeAccount struct {
	Status string `json:"status"`
	Kid    string `json:"-"`
	Orders string `json:"orders"`
}

func (a *acmeClient) directory() (map[string]string, error) {
	if a.dir != nil {
		return a.dir, nil
	}
	// The directory itself is a plain GET, no signing.
	resp, err := a.http.Get(a.directoryURL)
	if err != nil {
		return nil, fmt.Errorf("acme directory: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("acme directory read: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("acme directory: status %d", resp.StatusCode)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("acme directory parse: %w", err)
	}
	// The directory may contain non-string values (e.g. the "meta" object);
	// only the endpoint URLs (strings) are needed.
	dir := make(map[string]string, len(raw))
	for k, v := range raw {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			dir[k] = s
		}
	}
	for _, key := range []string{"newNonce", "newAccount", "newOrder"} {
		if dir[key] == "" {
			return nil, fmt.Errorf("acme directory: missing %q endpoint", key)
		}
	}
	a.dir = dir
	return dir, nil
}

// ensureAccount registers or locates the ACME account for the account key.
func (a *acmeClient) ensureAccount(accountKey crypto.Signer) error {
	a.logStagef("准备账户", "正在获取 ACME 机构 Directory: %s", a.directoryURL)
	dir, err := a.directory()
	if err != nil {
		a.warnStagef("准备账户", "获取 ACME Directory 失败: %v", err)
		return err
	}
	a.logStagef("准备账户", "获取 Directory 成功 (newAccount=%s, newOrder=%s)", dir["newAccount"], dir["newOrder"])

	thumbprint, err := keyThumbprint(accountKey)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"termsOfServiceAgreed": true,
	}
	// RFC 8555: contact is optional. Let's Encrypt rejects addresses whose
	// domain lacks a valid public suffix (e.g. the historically seeded
	// admin@watchman.local), so omit the field entirely when no usable
	// email is configured instead of sending a placeholder.
	if email := strings.TrimSpace(a.email); email != "" {
		payload["contact"] = []string{"mailto:" + email}
	}
	if a.eabKeyID != "" && a.eabHMACKey != "" {
		a.logStagef("准备账户", "已配置 EAB (External Account Binding) 外部绑定: KeyID=%s", a.eabKeyID)
		eabJWS, err := computeEAB(dir["newAccount"], a.eabKeyID, a.eabHMACKey, accountKey.Public())
		if err != nil {
			a.warnStagef("准备账户", "计算 EAB 签名失败: %v", err)
			return fmt.Errorf("compute EAB: %w", err)
		}
		payload["externalAccountBinding"] = eabJWS
	}
	a.logStagef("准备账户", "向 CA 注册或加载账户 (Email=%s)...", a.email)
	body, location, status, err := a.jwsRequest(dir["newAccount"], payload, "", accountKey, true)
	if err != nil {
		a.warnStagef("准备账户", "ACME 账户请求失败: %v", err)
		return err
	}
	if status/100 != 2 {
		acmeErr := acmeError("newAccount", status, body)
		a.warnStagef("准备账户", "ACME 账户请求被 CA 拒绝: %v", acmeErr)
		return acmeErr
	}
	if location == "" {
		return fmt.Errorf("newAccount: missing Location")
	}
	a.kid = location
	_ = thumbprint
	a.logStagef("准备账户", "ACME 账户就绪 (kid=%s)", a.kid)
	return nil
}

// obtainCertificate runs order -> authorizations -> finalize -> cert.
// identifiers may mix DNS names and IP literals. Each authorization is
// validated with the usable challenge solvers in preference order (HTTP-01
// first for plain DNS names, DNS-01 for wildcards, HTTP-01 only for IP
// literals). When a solver fails at the CA, the order is retried once with
// the remaining solvers, because a failed challenge poisons its authorization
// and a fresh order is required to fall back.
func (a *acmeClient) obtainCertificate(accountKey crypto.Signer, identifiers []string, solvers *solverSet) (certPEM, keyPEM []byte, notBefore, notAfter time.Time, issuer string, err error) {
	excluded := ""
	for {
		certPEM, keyPEM, notBefore, notAfter, issuer, err =
			a.attemptOrder(accountKey, identifiers, solvers.excluding(excluded))
		if err == nil {
			return certPEM, keyPEM, notBefore, notAfter, issuer, nil
		}
		var cfe *challengeFailedError
		if !errors.As(err, &cfe) || excluded != "" {
			return nil, nil, time.Time{}, time.Time{}, "", err
		}
		excluded = cfe.solverType
		a.log.Warn("order attempt failed, retrying with remaining challenge solvers",
			"failed_solver", cfe.solverType, "identifier", cfe.identifier, "err", cfe.err)
		a.warnStagef("挑战选择", "验证方式 %s 失败，回退尝试剩余可用挑战方式...", cfe.solverType)
	}
}

// acmeOrder is the subset of an RFC 8555 order object the client uses.
type acmeOrder struct {
	Status         string          `json:"status"`
	Authorizations []string        `json:"authorizations"`
	Finalize       string          `json:"finalize"`
	Certificate    string          `json:"certificate"`
	Error          json.RawMessage `json:"error,omitempty"`
}

// authzChallenge is one challenge object offered by the CA for an authorization.
type authzChallenge struct {
	Type  string `json:"type"`
	URL   string `json:"url"`
	Token string `json:"token"`
}

// authorization is the subset of an RFC 8555 authorization object the client uses.
type authorization struct {
	Status     string `json:"status"`
	Identifier struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"identifier"`
	Challenges []authzChallenge `json:"challenges"`
}

// createOrder submits a new order and returns its URL and object.
func (a *acmeClient) createOrder(accountKey crypto.Signer, identifiers []string) (string, *acmeOrder, error) {
	a.logStagef("创建订单", "向 CA 提交新证书订单: 包含 %d 个标识符 (%s)", len(identifiers), strings.Join(identifiers, ", "))
	dir, err := a.directory()
	if err != nil {
		return "", nil, err
	}

	// Identifiers for the order.
	idents := make([]map[string]string, 0, len(identifiers))
	needHTTP01 := false
	for _, d := range identifiers {
		t := identifierType(d)
		if t == "ip" {
			needHTTP01 = true
		}
		idents = append(idents, map[string]string{"type": t, "value": d})
	}

	// 1. New order. The order URL is the Location response header, used to
	// poll status below.
	orderReq := map[string]any{
		"identifiers": idents,
	}
	if needHTTP01 {
		// RFC 8738 / ACME profile draft: Let's Encrypt (and CAs supporting short-lived IP certs)
		// require "profile": "shortlived" for IP address identifiers (see https://letsencrypt.org/docs/profiles/).
		orderReq["profile"] = "shortlived"
	}

	body, orderURL, status, err := a.jwsRequest(dir["newOrder"], orderReq, a.kid, accountKey, false)
	if err != nil {
		a.warnStagef("创建订单", "提交订单请求失败: %v", err)
		return "", nil, fmt.Errorf("newOrder: %w", err)
	}
	// If CA rejected the profile parameter (e.g. non-Let's Encrypt CA that doesn't support the profile draft),
	// fall back to order without profile.
	if status/100 != 2 && needHTTP01 && strings.Contains(strings.ToLower(string(body)), "unrecognized") && strings.Contains(strings.ToLower(string(body)), "profile") {
		delete(orderReq, "profile")
		body, orderURL, status, err = a.jwsRequest(dir["newOrder"], orderReq, a.kid, accountKey, false)
		if err != nil {
			a.warnStagef("创建订单", "回退提交订单失败: %v", err)
			return "", nil, fmt.Errorf("newOrder: %w", err)
		}
	}
	if status/100 != 2 {
		acmeErr := acmeError("newOrder", status, body)
		a.warnStagef("创建订单", "CA 拒绝创建订单: %v", acmeErr)
		return "", nil, acmeErr
	}
	var order acmeOrder
	if err := json.Unmarshal(body, &order); err != nil {
		return "", nil, fmt.Errorf("newOrder parse: %w", err)
	}
	if order.Finalize == "" || orderURL == "" {
		return "", nil, fmt.Errorf("order missing finalize/location URL")
	}
	a.logStagef("创建订单", "CA 订单创建成功: URL=%s, 待授权数量=%d", orderURL, len(order.Authorizations))
	return orderURL, &order, nil
}

// fetchAuthorization POST-as-GETs one authorization object.
func (a *acmeClient) fetchAuthorization(authzURL string, accountKey crypto.Signer) (*authorization, error) {
	body, err := a.postAsGet(authzURL, a.kid, accountKey)
	if err != nil {
		return nil, fmt.Errorf("authz fetch: %w", err)
	}
	var authz authorization
	if err := json.Unmarshal(body, &authz); err != nil {
		return nil, fmt.Errorf("authz parse: %w", err)
	}
	return &authz, nil
}

// triggerChallenge asks the CA to validate the challenge (RFC 8555 §7.5.1:
// an empty JSON object acknowledges the challenge).
func (a *acmeClient) triggerChallenge(challengeURL string, accountKey crypto.Signer) error {
	_, _, _, err := a.jwsRequest(challengeURL, map[string]any{}, a.kid, accountKey, false)
	return err
}

// waitOrderReady polls the order until its status becomes "ready".
func (a *acmeClient) waitOrderReady(orderURL string, order *acmeOrder, accountKey crypto.Signer) error {
	a.logStagef("提交Finalize", "等待 CA 订单进入就绪状态 (URL=%s, 当前状态=%s)...", orderURL, order.Status)
	deadline := time.Now().Add(5 * time.Minute)
	lastStatus := order.Status
	for order.Status != "ready" {
		if time.Now().After(deadline) {
			a.warnStagef("提交Finalize", "等待订单就绪超时: %s", order.Status)
			return fmt.Errorf("order timeout: %s", order.Status)
		}
		time.Sleep(3 * time.Second)
		body, _, status, err := a.jwsRequest(orderURL, nil, a.kid, accountKey, false)
		if err != nil {
			return err
		}
		if status/100 != 2 {
			return acmeError("poll order", status, body)
		}
		if err := json.Unmarshal(body, order); err != nil {
			return err
		}
		if order.Status != lastStatus {
			a.logStagef("提交Finalize", "订单状态更新: %s", order.Status)
			lastStatus = order.Status
		}
		if order.Status == "invalid" {
			errMsg := "order invalid"
			if len(order.Error) > 0 {
				errMsg = fmt.Sprintf("order invalid: %s", string(order.Error))
			}
			a.warnStagef("提交Finalize", "订单已失效: %s", errMsg)
			return fmt.Errorf("%s", errMsg)
		}
	}
	a.logStage("提交Finalize", "CA 订单已就绪 (ready)，准备提交 CSR")
	return nil
}

// finalizeOrder submits the CSR, waits for the order to become valid and
// downloads the certificate chain.
func (a *acmeClient) finalizeOrder(orderURL string, order *acmeOrder, accountKey crypto.Signer, identifiers []string) (certPEM, keyPEM []byte, notBefore, notAfter time.Time, issuer string, err error) {
	a.logStage("生成CSR", "正在生成 ECDSA P-256 私钥与 PKCS#10 证书签名请求 (CSR)...")
	csrKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		a.warnStagef("生成CSR", "生成 ECDSA 私钥失败: %v", err)
		return nil, nil, time.Time{}, time.Time{}, "", err
	}
	csrDer, err := makeCSR(csrKey, identifiers)
	if err != nil {
		a.warnStagef("生成CSR", "构造 CSR 失败: %v", err)
		return nil, nil, time.Time{}, time.Time{}, "", err
	}
	csrB64 := base64.RawURLEncoding.EncodeToString(csrDer)
	a.logStagef("生成CSR", "CSR 构造成功，包含标识符: %s", strings.Join(identifiers, ", "))

	a.logStagef("提交Finalize", "向 CA 提交 Finalize 请求: %s", order.Finalize)
	body, _, status, err := a.jwsRequest(order.Finalize, map[string]any{"csr": csrB64}, a.kid, accountKey, false)
	if err != nil {
		a.warnStagef("提交Finalize", "Finalize 请求网络错误: %v", err)
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("finalize: %w", err)
	}
	if status/100 != 2 {
		acmeErr := acmeError("finalize", status, body)
		a.warnStagef("提交Finalize", "Finalize 请求被 CA 拒绝: %v", acmeErr)
		return nil, nil, time.Time{}, time.Time{}, "", acmeErr
	}
	var finOrder struct {
		Status      string          `json:"status"`
		Certificate string          `json:"certificate"`
		Error       json.RawMessage `json:"error,omitempty"`
	}
	_ = json.Unmarshal(body, &finOrder)

	a.logStagef("提交Finalize", "Finalize 提交成功，轮询等待证书签发 (当前状态=%s)...", finOrder.Status)
	deadline := time.Now().Add(3 * time.Minute)
	certURL := finOrder.Certificate
	lastStatus := finOrder.Status
	for {
		if time.Now().After(deadline) {
			a.warnStagef("提交Finalize", "等待证书签发超时: %s", finOrder.Status)
			return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("finalize timeout: %s", finOrder.Status)
		}
		if certURL == "" {
			body, _, status, err := a.jwsRequest(orderURL, nil, a.kid, accountKey, false)
			if err != nil {
				return nil, nil, time.Time{}, time.Time{}, "", err
			}
			if status/100 != 2 {
				return nil, nil, time.Time{}, time.Time{}, "", acmeError("poll finalize", status, body)
			}
			_ = json.Unmarshal(body, &finOrder)
			certURL = finOrder.Certificate
		}
		if finOrder.Status != lastStatus {
			a.logStagef("提交Finalize", "订单签发状态更新: %s", finOrder.Status)
			lastStatus = finOrder.Status
		}
		if certURL != "" && finOrder.Status == "valid" {
			a.logStagef("提交Finalize", "CA 签发完毕，证书下载地址已就绪: %s", certURL)
			break
		}
		if finOrder.Status == "invalid" {
			errMsg := "finalize invalid"
			if len(finOrder.Error) > 0 {
				errMsg = fmt.Sprintf("finalize invalid: %s", string(finOrder.Error))
			}
			a.warnStagef("提交Finalize", "CA 签发失败: %s", errMsg)
			return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("%s", errMsg)
		}
		time.Sleep(3 * time.Second)
	}

	a.logStagef("下载证书", "从 CA 下载证书链 (URL=%s)...", certURL)
	respBody, err := a.postAsGet(certURL, a.kid, accountKey)
	if err != nil {
		a.warnStagef("下载证书", "下载证书链失败: %v", err)
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("download cert: %w", err)
	}

	block, rest := pem.Decode(respBody)
	if block == nil || block.Type != "CERTIFICATE" {
		a.warnStage("下载证书", "下载内容非有效 PEM 证书格式")
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("cert download: not PEM")
	}
	first, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		a.warnStagef("下载证书", "解析证书公钥失败: %v", err)
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("cert parse: %w", err)
	}

	pemBytes := pem.EncodeToMemory(block)
	if len(rest) > 0 {
		pemBytes = append(pemBytes, rest...)
	}
	keyDer, err := x509.MarshalECPrivateKey(csrKey)
	if err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", err
	}
	keyPem := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDer})

	a.logStagef("下载证书", "证书链解析成功: 主体=%s, 颁发者=%s, 有效期=%s ~ %s",
		first.Subject.CommonName, first.Issuer.CommonName,
		first.NotBefore.Format("2006-01-02 15:04:05"), first.NotAfter.Format("2006-01-02 15:04:05"))
	return pemBytes, keyPem, first.NotBefore, first.NotAfter, first.Issuer.CommonName, nil
}

// attemptOrder runs a single order -> authorizations -> finalize -> cert
// pass using the given solvers.
func (a *acmeClient) attemptOrder(accountKey crypto.Signer, identifiers []string, solvers *solverSet) (certPEM, keyPEM []byte, notBefore, notAfter time.Time, issuer string, err error) {
	orderURL, order, err := a.createOrder(accountKey, identifiers)
	if err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", err
	}

	// 2. Satisfy each authorization, picking (and falling back between) the
	// usable challenge solvers. The solvers were probed when built and stay
	// up for the whole order.
	for _, authzURL := range order.Authorizations {
		if err := a.satisfyAuthorization(authzURL, accountKey, solvers); err != nil {
			return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("authorization: %w", err)
		}
	}

	// 3. Poll order until ready.
	if err := a.waitOrderReady(orderURL, order, accountKey); err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", err
	}

	// 4-5. CSR, finalize, download.
	return a.finalizeOrder(orderURL, order, accountKey, identifiers)
}

// challengeRef is one challenge object offered by the CA for an authorization.
type challengeRef struct {
	url   string
	token string
}

// satisfyAuthorization completes one authorization, trying the usable
// challenge solvers in preference order (see solverSet.pickSolvers). A solver
// whose Present fails is skipped before the CA is contacted; the first
// solver that presents successfully is submitted to the CA. A CA-side
// validation failure is returned as *challengeFailedError so the caller can
// retry the order with the remaining solvers (a failed challenge poisons its
// authorization, so fallback requires a fresh order).
func (a *acmeClient) satisfyAuthorization(authzURL string, accountKey crypto.Signer, solvers *solverSet) error {
	a.logStagef("挑战选择", "正在获取授权详情: %s", authzURL)
	authz, err := a.fetchAuthorization(authzURL, accountKey)
	if err != nil {
		a.warnStagef("挑战选择", "获取授权详情失败: %v", err)
		return err
	}
	if authz.Status == "valid" {
		a.logStagef("挑战选择", "域名/IP %q 此前已验证通过 (status=valid)，无需重复挑战", authz.Identifier.Value)
		return nil
	}

	thumbprint, err := keyThumbprint(accountKey)
	if err != nil {
		return err
	}

	offered := make(map[string]challengeRef, len(authz.Challenges))
	offeredTypes := make(map[string]bool, len(authz.Challenges))
	for _, ch := range authz.Challenges {
		if _, ok := offered[ch.Type]; !ok {
			offered[ch.Type] = challengeRef{url: ch.URL, token: ch.Token}
			offeredTypes[ch.Type] = true
		}
	}
	candidates := solvers.pickSolvers(authz.Identifier.Type, authz.Identifier.Value, offeredTypes)
	if len(candidates) == 0 {
		errMsg := fmt.Sprintf("no usable challenge for %s %q (CA offered: %s; available here: %s)",
			authz.Identifier.Type, authz.Identifier.Value,
			strings.Join(sortedKeys(offeredTypes), ", "),
			strings.Join(solvers.availableTypes(), ", "))
		a.warnStage("挑战选择", errMsg)
		return fmt.Errorf("%s", errMsg)
	}

	a.logStagef("挑战选择", "域名/IP %q: CA 提供挑战 [%s], 本地选用 [%s]",
		authz.Identifier.Value,
		strings.Join(sortedKeys(offeredTypes), ", "),
		candidates[0].Type())

	var presentErrs []string
	for _, sv := range candidates {
		ref := offered[sv.Type()]
		if ref.token == "" {
			continue
		}
		keyAuthz := ref.token + "." + thumbprint

		switch sv.Type() {
		case "http-01":
			a.logStagef("挑战呈现", "HTTP-01 验证: 在本地 80 端口注册端点 /.well-known/acme-challenge/%s", ref.token)
		case "dns-01":
			fqdn := dns01RecordHost(authz.Identifier.Value)
			txt := dns01TXTValue(keyAuthz)
			a.logStagef("挑战呈现", "DNS-01 验证: 请求 dns-mng 添加 TXT 记录 %s = %s 并等待生效 (20秒)...", fqdn, txt)
		}

		cleanup, err := sv.Present(authz.Identifier.Value, ref.token, keyAuthz)
		if err != nil {
			// Present failed before the CA was involved: safe to try the next solver.
			a.warnStagef("挑战呈现", "挑战方式 %s 呈现失败: %v", sv.Type(), err)
			presentErrs = append(presentErrs, fmt.Sprintf("%s: %v", sv.Type(), err))
			continue
		}
		a.logStagef("挑战呈现", "%s 挑战呈现就绪", strings.ToUpper(sv.Type()))

		a.logStagef("CA验证", "通知 CA 开始校验 %s 挑战: %s", strings.ToUpper(sv.Type()), ref.url)
		if err := a.triggerChallenge(ref.url, accountKey); err != nil {
			cleanup()
			a.warnStagef("CA验证", "通知 CA 触发校验失败: %v", err)
			return &challengeFailedError{solverType: sv.Type(), identifier: authz.Identifier.Value, err: fmt.Errorf("challenge notify: %w", err)}
		}
		err = a.pollAuthorization(authzURL, accountKey)
		cleanup()
		a.logStagef("清理挑战", "已清理 %s 挑战临时凭据 (%s)", strings.ToUpper(sv.Type()), authz.Identifier.Value)
		if err != nil {
			a.warnStagef("CA验证", "CA 校验 %s 未通过: %v", authz.Identifier.Value, err)
			return &challengeFailedError{solverType: sv.Type(), identifier: authz.Identifier.Value, err: err}
		}
		a.logStagef("CA验证", "域名/IP %q 验证成功，授权已生效", authz.Identifier.Value)
		return nil
	}
	return fmt.Errorf("all challenge solvers failed to present for %q: %s",
		authz.Identifier.Value, strings.Join(presentErrs, "; "))
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pollAuthorization waits until the authorization becomes valid.
func (a *acmeClient) pollAuthorization(authzURL string, accountKey crypto.Signer) error {
	return a.pollAuthorizationWithTimeout(authzURL, accountKey, 3*time.Minute)
}

// pollAuthorizationWithTimeout is pollAuthorization with a custom deadline.
// The manual DNS-01 flow uses a longer deadline because the administrator
// adds the TXT records by hand and CA-side DNS propagation takes longer.
func (a *acmeClient) pollAuthorizationWithTimeout(authzURL string, accountKey crypto.Signer, timeout time.Duration) error {
	a.logStagef("CA验证", "轮询 CA 授权状态 (URL=%s, 最长等待=%s)...", authzURL, timeout)
	deadline := time.Now().Add(timeout)
	lastStatus := ""
	for {
		if time.Now().After(deadline) {
			a.warnStagef("CA验证", "轮询 CA 授权超时 (%s)", timeout)
			return fmt.Errorf("authorization timeout")
		}
		time.Sleep(3 * time.Second)
		body, err := a.postAsGet(authzURL, a.kid, accountKey)
		if err != nil {
			continue
		}
		var st struct {
			Status     string `json:"status"`
			Challenges []struct {
				Type  string          `json:"type"`
				Error json.RawMessage `json:"error,omitempty"`
			} `json:"challenges"`
		}
		if json.Unmarshal(body, &st) == nil {
			if st.Status != lastStatus {
				a.logStagef("CA验证", "CA 授权状态更新: %s", st.Status)
				lastStatus = st.Status
			}
			switch st.Status {
			case "valid":
				a.logStage("CA验证", "CA 验证通过，授权状态已变更为 valid")
				return nil
			case "invalid", "expired":
				var chalErr string
				for _, ch := range st.Challenges {
					if len(ch.Error) > 0 {
						chalErr = fmt.Sprintf(" (%s: %s)", ch.Type, string(ch.Error))
						break
					}
				}
				errStr := fmt.Sprintf("authorization %s%s", st.Status, chalErr)
				a.warnStagef("CA验证", "CA 校验未通过: %s", errStr)
				return fmt.Errorf("%s", errStr)
			}
		}
	}
}

