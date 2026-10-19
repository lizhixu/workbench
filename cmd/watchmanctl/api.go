// REST control-plane client for the watchman panel. Only carries management
// calls (login, host lookup, tunnel client-info / listing / close) — business
// bytes never flow through the panel port, they go over the -opgrpc listener.
package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type apiClient struct {
	cfg *config
	hc  *http.Client
}

func newAPIClient(cfg *config) *apiClient {
	return &apiClient{
		cfg: cfg,
		hc: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					MinVersion:         tls.VersionTLS12,
					InsecureSkipVerify: cfg.Insecure, // explicitly requested via --insecure at login
				},
			},
		},
	}
}

// apiError carries the panel's {"error": "..."} message back to the user.
type apiError struct {
	Code    int
	Message string
}

func (e *apiError) Error() string { return e.Message }

func (a *apiClient) doJSON(method, path string, body, out any) error {
	if a.cfg.Server == "" {
		return fmt.Errorf("no panel configured; run `watchmanctl login --server <url>` first")
	}
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimSuffix(a.cfg.Server, "/")+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("reach panel: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e)
		msg := e.Error
		if msg == "" {
			msg = resp.Status
		}
		return &apiError{Code: resp.StatusCode, Message: msg}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// login exchanges credentials for a JWT.
func (a *apiClient) login(username, password string) (token, role string, err error) {
	var out struct {
		Token string `json:"token"`
		User  struct {
			Username string `json:"username"`
			Role     string `json:"role"`
		} `json:"user"`
	}
	if err := a.doJSON(http.MethodPost, "/api/v1/auth/login", map[string]string{
		"username": username, "password": password,
	}, &out); err != nil {
		return "", "", err
	}
	if out.Token == "" {
		return "", "", fmt.Errorf("panel returned no token")
	}
	return out.Token, out.User.Role, nil
}

// tunnelClientInfo is GET /api/v1/tunnels/client-info.
type tunnelClientInfo struct {
	Enabled  bool              `json:"enabled"`
	Endpoint string            `json:"endpoint"`
	CLI      map[string]string `json:"cli"`
}

func (a *apiClient) clientInfo() (*tunnelClientInfo, error) {
	var out tunnelClientInfo
	if err := a.doJSON(http.MethodGet, "/api/v1/tunnels/client-info", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// hostDTO is the panel's host list item (subset).
type hostDTO struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	Status   string `json:"status"`
	OS       string `json:"os"`
}

// resolveHost turns a host id or exact hostname into a host id.
func (a *apiClient) resolveHost(idOrName string) (*hostDTO, error) {
	var out struct {
		Data []hostDTO `json:"data"`
	}
	if err := a.doJSON(http.MethodGet, "/api/v1/hosts", nil, &out); err != nil {
		return nil, err
	}
	var hostnameHits []hostDTO
	for i := range out.Data {
		h := &out.Data[i]
		if h.ID == idOrName {
			return h, nil
		}
		if h.Hostname == idOrName {
			hostnameHits = append(hostnameHits, *h)
		}
	}
	switch len(hostnameHits) {
	case 0:
		return nil, fmt.Errorf("no host matches %q", idOrName)
	case 1:
		return &hostnameHits[0], nil
	default:
		ids := make([]string, 0, len(hostnameHits))
		for _, h := range hostnameHits {
			ids = append(ids, h.ID)
		}
		return nil, fmt.Errorf("hostname %q is ambiguous (%s); use the host id", idOrName, strings.Join(ids, ", "))
	}
}

// myTunnel is one live operator tunnel leg owned by the caller.
type myTunnel struct {
	ConnID      string    `json:"conn_id"`
	AgentID     string    `json:"agent_id"`
	Target      string    `json:"target"`
	CreatedAt   time.Time `json:"created_at"`
	IdleSeconds int64     `json:"idle_seconds"`
}

func (a *apiClient) listMyTunnels() ([]myTunnel, error) {
	var out struct {
		Tunnels []myTunnel `json:"tunnels"`
	}
	if err := a.doJSON(http.MethodGet, "/api/v1/tunnels/mine", nil, &out); err != nil {
		return nil, err
	}
	return out.Tunnels, nil
}

func (a *apiClient) closeMyTunnel(id string) error {
	return a.doJSON(http.MethodDelete, "/api/v1/tunnels/mine/"+id, nil, nil)
}
