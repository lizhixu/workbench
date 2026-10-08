package rpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"watchman/proto/agentpb"
)

const heartbeatGraceFactor = 3

// persistedAgent is the on-disk shape of an Agent (without live fields like
// status, lastSeen — those are runtime-only and reset on reload).
type persistedAgent struct {
	Hostname   string    `json:"hostname"`
	OS         string    `json:"os"`
	Arch       string    `json:"arch"`
	Distro     string    `json:"distro"`
	Version    string    `json:"version"`
	Registered time.Time `json:"registered"`
	Group      string    `json:"group"`
	Tags       []string  `json:"tags"`
	Uptime     int64     `json:"uptime"`
	CPUCores   int32     `json:"cpu_cores"`
	MemTotal   int64     `json:"mem_total"`
	InternalIP string    `json:"internal_ip"`
	PublicIP   string    `json:"public_ip"`
	Location   string    `json:"location"`
	// Optional billing & traffic quota configurations
	Price           float64 `json:"price,omitempty"`
	Currency        string  `json:"currency,omitempty"`
	BillingCycle    string  `json:"billing_cycle,omitempty"`
	ExpiresAt       string  `json:"expires_at,omitempty"`
	AutoRenewal     bool    `json:"auto_renewal,omitempty"`
	TrafficLimitGB  float64 `json:"traffic_limit_gb,omitempty"`
	TrafficCalcType string  `json:"traffic_calc_type,omitempty"`
	TrafficResetDay int     `json:"traffic_reset_day,omitempty"`
	RenewalURL      string  `json:"renewal_url,omitempty"`
	Notes           string  `json:"notes,omitempty"`
	// AuthToken is the long-lived token issued at registration; kept here
	// (NOT in r.tokens) so a server restart doesn't invalidate it.
	AuthToken string `json:"auth_token"`
}

// Registry holds all known agents (online and recently offline) and the live
// stream hubs for connected agents. The agents map + auth tokens are
// persisted to disk (dataDir/agents.json) so a server restart does not
// invalidate already-installed agents.
type Registry struct {
	mu       sync.RWMutex
	agents   map[string]*Agent        // by agent_id (persisted)
	tokens   map[string]string        // auth_token -> agent_id (persisted; mirror of Agent.AuthToken for fast lookup)
	hubs     map[string]*Hub          // by agent_id (only when connected; runtime-only)
	enroll   map[string]*enrollTicket // enroll_token -> ticket (in-memory MVP)
	upgrades map[string]*UpgradeState // agent_id -> active upgrade state
	tunnel   *Coordinator             // reverse TCP tunnels (nil = disabled)
	dataDir  string
	log      *slog.Logger
}

type UpgradeState struct {
	TargetVersion string    `json:"target_version"`
	Stage         string    `json:"stage"` // "dispatched", "downloading", "verifying", "replacing", "restarting", "error"
	StartedAt     time.Time `json:"started_at"`
	Error         string    `json:"error,omitempty"`
}

type enrollTicket struct {
	AgentIDHint string    `json:"agent_id_hint,omitempty"`
	Created     time.Time `json:"created"`
	Expires     time.Time `json:"expires"`
	Used        bool      `json:"used"`
}

// Agent is the server-side view of a managed host.
type Agent struct {
	ID         string
	Hostname   string
	OS         string
	Arch       string
	Distro     string
	Version    string
	Status     string // online / offline
	LastSeen   time.Time
	Registered time.Time
	Group      string
	Tags       []string
	Uptime     int64 // real OS uptime in seconds (since boot), from registration/metrics
	CPUCores   int32
	MemTotal   int64
	InternalIP string
	PublicIP   string
	Location   string
	// Optional billing & traffic quota configurations
	Price           float64
	Currency        string
	BillingCycle    string
	ExpiresAt       string
	AutoRenewal     bool
	TrafficLimitGB  float64
	TrafficCalcType string
	TrafficResetDay int
	RenewalURL      string
	Notes           string
	// ReconnectReason is the reason sent by the agent on its most recent registration.
	ReconnectReason string
	// AuthToken is the long-lived token; persisted with the agent so the
	// server can validate reconnects after a restart.
	AuthToken string
}

// Hub is the live connection state for a connected agent.
type Hub struct {
	AgentID   string
	heartbeat int32
	lastSeen  time.Time
	sendCh    chan *agentpb.ServerMessage
	// doneCh is closed by unbind to broadcast "hub is gone". Send selects on
	// it so a blocked send wakes up immediately; sendCh itself is NEVER
	// closed, which rules out the send-on-closed-channel panic (and the data
	// race the detector reports for concurrent send vs close) by construction.
	doneCh chan struct{}
	// closed is set under mu in unbind before doneCh is closed, so Send can
	// fail fast once the hub is unbound.
	closed       bool
	stream       agentpb.AgentService_ConnectServer
	tunnel       *Coordinator // reverse TCP tunnels (nil = disabled)
	mu           sync.RWMutex
	termHandlers map[string]func(*agentpb.TerminalOutput)
	// Generic response handlers keyed by op_id / exec_id / session_id.
	respHandlers map[string]func(*agentpb.AgentMessage)
	// Latest metrics sample (for REST polling).
	lastMetrics *agentpb.MetricsSample
	// Back-reference to registry so metrics can update the persisted Agent.
	registry *Registry
}

// NewRegistry creates a registry, loading any persisted agents from
// dataDir/agents.json. dataDir may be empty (tests / in-memory mode), in
// which case no persistence is attempted.
func NewRegistry(dataDir string, log *slog.Logger) *Registry {
	if log == nil {
		log = slog.Default()
	}
	r := &Registry{
		agents:   make(map[string]*Agent),
		hubs:     make(map[string]*Hub),
		tokens:   make(map[string]string),
		enroll:   make(map[string]*enrollTicket),
		upgrades: make(map[string]*UpgradeState),
		dataDir:  dataDir,
		log:      log,
	}
	if dataDir != "" {
		if err := r.loadAgents(); err != nil {
			log.Warn("load persisted agents", "err", err)
		} else {
			log.Info("loaded persisted agents", "count", len(r.agents))
		}
		if err := r.loadEnroll(); err != nil {
			log.Warn("load persisted enroll tokens", "err", err)
		} else {
			log.Info("loaded persisted enroll tokens", "count", len(r.enroll))
		}
	}
	return r
}

// agentsFile is the path to the persisted registry file.
func (r *Registry) agentsFile() string {
	if r.dataDir == "" {
		return ""
	}
	return filepath.Join(r.dataDir, "agents.json")
}

// loadAgents reads dataDir/agents.json and rebuilds the agents + tokens maps.
// Status is reset to "offline" (no live hub until the agent reconnects).
func (r *Registry) loadAgents() error {
	path := r.agentsFile()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var raw map[string]persistedAgent
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, p := range raw {
		a := &Agent{
			ID:              id,
			Hostname:        p.Hostname,
			OS:              p.OS,
			Arch:            p.Arch,
			Distro:          p.Distro,
			Version:         p.Version,
			Status:          "offline",
			Registered:      p.Registered,
			Group:           p.Group,
			Tags:            p.Tags,
			Uptime:          p.Uptime,
			CPUCores:        p.CPUCores,
			MemTotal:        p.MemTotal,
			InternalIP:      p.InternalIP,
			PublicIP:        p.PublicIP,
			Location:        p.Location,
			Price:           p.Price,
			Currency:        p.Currency,
			BillingCycle:    p.BillingCycle,
			ExpiresAt:       p.ExpiresAt,
			AutoRenewal:     p.AutoRenewal,
			TrafficLimitGB:  p.TrafficLimitGB,
			TrafficCalcType: p.TrafficCalcType,
			TrafficResetDay: p.TrafficResetDay,
			RenewalURL:      p.RenewalURL,
			Notes:           p.Notes,
			AuthToken:       p.AuthToken,
		}
		if a.Tags == nil {
			a.Tags = []string{}
		}
		r.agents[id] = a
		if p.AuthToken != "" {
			r.tokens[p.AuthToken] = id
		}
	}
	return nil
}

// persistAgentsLocked writes the agent registry to disk. Caller must hold r.mu.
func (r *Registry) persistAgentsLocked() error {
	path := r.agentsFile()
	if path == "" {
		return nil
	}
	raw := make(map[string]persistedAgent, len(r.agents))
	for id, a := range r.agents {
		raw[id] = persistedAgent{
			Hostname:        a.Hostname,
			OS:              a.OS,
			Arch:            a.Arch,
			Distro:          a.Distro,
			Version:         a.Version,
			Registered:      a.Registered,
			Group:           a.Group,
			Tags:            a.Tags,
			Uptime:          a.Uptime,
			CPUCores:        a.CPUCores,
			MemTotal:        a.MemTotal,
			InternalIP:      a.InternalIP,
			PublicIP:        a.PublicIP,
			Location:        a.Location,
			Price:           a.Price,
			Currency:        a.Currency,
			BillingCycle:    a.BillingCycle,
			ExpiresAt:       a.ExpiresAt,
			AutoRenewal:     a.AutoRenewal,
			TrafficLimitGB:  a.TrafficLimitGB,
			TrafficCalcType: a.TrafficCalcType,
			TrafficResetDay: a.TrafficResetDay,
			RenewalURL:      a.RenewalURL,
			Notes:           a.Notes,
			AuthToken:       a.AuthToken,
		}
	}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func (r *Registry) IssueEnrollToken() string {
	tok := randomToken(24)
	r.mu.Lock()
	r.enroll[tok] = &enrollTicket{
		Created: time.Now(),
		Expires: time.Now().Add(24 * time.Hour),
	}
	_ = r.persistEnrollLocked()
	r.mu.Unlock()
	return tok
}

// enrollFile is the path to the persisted enroll-ticket file.
func (r *Registry) enrollFile() string {
	if r.dataDir == "" {
		return ""
	}
	return filepath.Join(r.dataDir, "enroll.json")
}

// persistEnrollLocked writes enroll tickets to disk (0600: tokens are
// secrets). Caller must hold r.mu.
func (r *Registry) persistEnrollLocked() error {
	path := r.enrollFile()
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(r.enroll, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// loadEnroll reads persisted enroll tickets, dropping expired ones.
func (r *Registry) loadEnroll() error {
	path := r.enrollFile()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var raw map[string]*enrollTicket
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for tok, t := range raw {
		if t == nil || t.Used || now.After(t.Expires) {
			continue
		}
		r.enroll[tok] = t
	}
	return nil
}

func (r *Registry) Register(ctx context.Context, req *agentpb.RegisterRequest, authToken string) (*Hub, *agentpb.RegisterResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	const (
		heartbeatSec = 30
		sessionKeep  = 60
	)

	if authToken != "" || req.GetEnrollToken() == "" {
		agentID := req.GetAgentId()
		if agentID == "" {
			return nil, nil, errors.New("missing agent_id on reconnect")
		}
		if authToken != "" {
			if id, ok := r.tokens[stripBearer(authToken)]; !ok || id != agentID {
				return nil, nil, errors.New("invalid auth token")
			}
		}
		a := r.agents[agentID]
		if a == nil {
			return nil, nil, fmt.Errorf("unknown agent %q", agentID)
		}
		applyReg(a, req)
		a.Status = "online"
		a.LastSeen = time.Now()
		if upg, ok := r.upgrades[agentID]; ok {
			if req.GetAgentVersion() == upg.TargetVersion || req.GetReconnectReason() == "upgrade" || upg.TargetVersion == "" {
				delete(r.upgrades, agentID)
			}
		}
		_ = r.persistAgentsLocked() // persist updated hostname/os/etc.

		hub := newHub(agentID, heartbeatSec, r)
		hub.tunnel = r.tunnel
		// The agent reconnected: drop tunnels bound to the old stream's hub.
		if r.tunnel != nil {
			r.tunnel.CloseByAgent(agentID)
		}
		r.hubs[agentID] = hub
		return hub, &agentpb.RegisterResponse{
			Ok:                   true,
			AgentId:              agentID,
			HeartbeatIntervalSec: heartbeatSec,
			SessionKeepSec:       sessionKeep,
			TrafficResetDay:      int32(normTrafficResetDay(a.TrafficResetDay)),
		}, nil
	}

	ticket := r.enroll[req.GetEnrollToken()]
	if ticket == nil {
		return nil, nil, errors.New("invalid enroll token")
	}
	if ticket.Used {
		return nil, nil, errors.New("enroll token already used")
	}
	if time.Now().After(ticket.Expires) {
		delete(r.enroll, req.GetEnrollToken())
		_ = r.persistEnrollLocked()
		return nil, nil, errors.New("enroll token expired")
	}
	ticket.Used = true
	delete(r.enroll, req.GetEnrollToken())
	_ = r.persistEnrollLocked()

	agentID := req.GetAgentId()
	if agentID == "" {
		agentID = randomToken(12)
	}
	authTokenNew := randomToken(32)
	a := &Agent{
		ID:         agentID,
		Registered: time.Now(),
		Status:     "online",
		LastSeen:   time.Now(),
		Tags:       []string{},
		AuthToken:  authTokenNew,
	}
	applyReg(a, req)
	delete(r.upgrades, agentID)
	r.agents[agentID] = a
	r.tokens[authTokenNew] = agentID
	_ = r.persistAgentsLocked() // safe: caller holds r.mu

	hub := newHub(agentID, heartbeatSec, r)
	hub.tunnel = r.tunnel
	// Fresh enroll for a known agent id: drop stale tunnels as above.
	if r.tunnel != nil {
		r.tunnel.CloseByAgent(agentID)
	}
	r.hubs[agentID] = hub
	r.log.Info("agent registered", "agent_id", agentID, "hostname", req.GetHostname())
	return hub, &agentpb.RegisterResponse{
		Ok:                   true,
		AgentId:              agentID,
		AuthToken:            authTokenNew,
		HeartbeatIntervalSec: heartbeatSec,
		SessionKeepSec:       sessionKeep,
		TrafficResetDay:      int32(normTrafficResetDay(a.TrafficResetDay)),
	}, nil
}

func applyReg(a *Agent, req *agentpb.RegisterRequest) {
	if h := req.GetHostname(); h != "" {
		a.Hostname = h
	}
	if v := req.GetOs(); v != "" {
		a.OS = v
	}
	if v := req.GetArch(); v != "" {
		a.Arch = v
	}
	if v := req.GetDistro(); v != "" {
		a.Distro = v
	}
	if v := req.GetAgentVersion(); v != "" {
		a.Version = v
	}
	if v := req.GetUptime(); v > 0 {
		a.Uptime = v
	}
	if v := req.GetCpuCores(); v > 0 {
		a.CPUCores = v
	}
	if v := req.GetMemTotal(); v > 0 {
		a.MemTotal = v
	}
	if v := req.GetInternalIp(); v != "" {
		a.InternalIP = v
	}
	if v := req.GetPublicIp(); v != "" {
		a.PublicIP = v
	}
	if v := req.GetLocation(); v != "" {
		a.Location = v
	}
	a.ReconnectReason = req.GetReconnectReason()
}

func (r *Registry) ListAgents() []*Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Agent, 0, len(r.agents))
	for _, a := range r.agents {
		cp := *a
		out = append(out, &cp)
	}
	// Map iteration order is randomized in Go, so return a stable order or the
	// host list reshuffles on every refresh. Registration time keeps a host in
	// the same slot across calls; ID is a deterministic tiebreaker for agents
	// registered in the same instant (or with a zero timestamp).
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Registered.Equal(out[j].Registered) {
			return out[i].Registered.Before(out[j].Registered)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (r *Registry) GetAgent(id string) *Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if a, ok := r.agents[id]; ok {
		cp := *a
		return &cp
	}
	return nil
}

func (r *Registry) Hub(id string) *Hub {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.hubs[id]
}

// SetAgentGroup sets the group for an agent.
func (r *Registry) SetAgentGroup(id, group string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.agents[id]
	if !ok {
		return fmt.Errorf("agent not found")
	}
	a.Group = group
	_ = r.persistAgentsLocked()
	return nil
}

// SetAgentTags sets tags for an agent.
func (r *Registry) SetAgentTags(id string, tags []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.agents[id]
	if !ok {
		return fmt.Errorf("agent not found")
	}
	a.Tags = tags
	_ = r.persistAgentsLocked()
	return nil
}

// ClearReconnectReason clears the one-shot reconnect reason for an agent after
// it has been consumed by the alert engine, ensuring subsequent checks behave normally.
func (r *Registry) ClearReconnectReason(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a, ok := r.agents[id]; ok {
		a.ReconnectReason = ""
	}
}

// SetAgentUpgrading marks an agent as actively undergoing an upgrade.
func (r *Registry) SetAgentUpgrading(agentID, targetVersion string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upgrades[agentID] = &UpgradeState{
		TargetVersion: targetVersion,
		Stage:         "dispatched",
		StartedAt:     time.Now(),
	}
}

// SetAgentUpgradeProgress updates the current progress stage of an agent upgrade.
func (r *Registry) SetAgentUpgradeProgress(agentID, stage, errStr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.upgrades[agentID]
	if !ok {
		st = &UpgradeState{
			StartedAt: time.Now(),
		}
		r.upgrades[agentID] = st
	}
	if stage != "" {
		st.Stage = stage
	}
	if errStr != "" {
		st.Error = errStr
	}
}

// GetAgentUpgrade returns the active upgrade state for an agent, if any.
// States older than 180 seconds are automatically expired.
func (r *Registry) GetAgentUpgrade(agentID string) *UpgradeState {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.upgrades[agentID]
	if !ok {
		return nil
	}
	if time.Since(st.StartedAt) > 180*time.Second {
		delete(r.upgrades, agentID)
		return nil
	}
	cp := *st
	return &cp
}

// ClearAgentUpgrade removes the upgrade state for an agent.
func (r *Registry) ClearAgentUpgrade(agentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.upgrades, agentID)
}

// HostBillingConfig carries optional user-managed finance, traffic quota and note configs for a host.
type HostBillingConfig struct {
	Price           float64 `json:"price"`
	Currency        string  `json:"currency"`
	BillingCycle    string  `json:"billing_cycle"`
	ExpiresAt       string  `json:"expires_at"`
	AutoRenewal     bool    `json:"auto_renewal"`
	TrafficLimitGB  float64 `json:"traffic_limit_gb"`
	TrafficCalcType string  `json:"traffic_calc_type"`
	TrafficResetDay int     `json:"traffic_reset_day"`
	RenewalURL      string  `json:"renewal_url"`
	Notes           string  `json:"notes"`
}

// normTrafficResetDay clamps the panel-configured billing reset day to the
// range the agent supports (1..28); 0/unset means the 1st of the month.
func normTrafficResetDay(d int) int {
	if d < 1 || d > 28 {
		return 1
	}
	return d
}

// SetAgentBilling updates optional billing, traffic limits and notes for a host.
// NOTE: a changed TrafficResetDay reaches the agent on its next (re)connect
// via RegisterResponse; already-connected agents keep their current cycle
// until then.
func (r *Registry) SetAgentBilling(id string, b HostBillingConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.agents[id]
	if !ok {
		return fmt.Errorf("agent not found")
	}
	a.Price = b.Price
	a.Currency = b.Currency
	a.BillingCycle = b.BillingCycle
	a.ExpiresAt = b.ExpiresAt
	a.AutoRenewal = b.AutoRenewal
	a.TrafficLimitGB = b.TrafficLimitGB
	if b.TrafficCalcType == "" {
		a.TrafficCalcType = "both"
	} else {
		a.TrafficCalcType = b.TrafficCalcType
	}
	if b.TrafficResetDay <= 0 {
		a.TrafficResetDay = 1
	} else if b.TrafficResetDay > 28 {
		// The agent only supports reset days 1..28 (every month has 28
		// days); larger values would silently collapse to 1 there.
		a.TrafficResetDay = 28
	} else {
		a.TrafficResetDay = b.TrafficResetDay
	}
	a.RenewalURL = b.RenewalURL
	a.Notes = b.Notes
	_ = r.persistAgentsLocked()
	return nil
}

// RenameGroup rewrites every agent whose group is oldName to newName and
// returns how many records changed. Passing an empty newName clears the group
// (used when a group is deleted) so hosts are never left pointing at a group
// that no longer exists.
func (r *Registry) RenameGroup(oldName, newName string) int {
	if oldName == "" {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, a := range r.agents {
		if a.Group == oldName {
			a.Group = newName
			n++
		}
	}
	if n > 0 {
		_ = r.persistAgentsLocked()
	}
	return n
}

// broadcastTimeout bounds a maintenance broadcast so a shutdown is never
// blocked by a slow or wedged agent connection.
const broadcastTimeout = 3 * time.Second

// BroadcastMaintenance sends a maintenance notice to all currently connected agents.
// Called prior to graceful server shutdown or restart so agents persist a
// "maintenance" reconnect reason and the alert engine stays quiet (see AGENTS.md 8.6).
//
// The hub list is snapshotted under the read lock but frames are sent outside
// it: Hub.Send may block up to 5s per agent when the outbound channel is full,
// and holding r.mu across that would stall ListAgents/Hub — and therefore every
// HTTP request that resolves a host — for the whole broadcast.
func (r *Registry) BroadcastMaintenance(reason string, durationSec int) {
	r.mu.RLock()
	hubs := make([]*Hub, 0, len(r.hubs))
	for _, hub := range r.hubs {
		if hub != nil {
			hubs = append(hubs, hub)
		}
	}
	r.mu.RUnlock()

	if len(hubs) == 0 {
		return
	}
	r.log.Info("broadcasting maintenance notice", "reason", reason, "agents", len(hubs))

	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		wg.Add(len(hubs))
		for _, hub := range hubs {
			go func(h *Hub) {
				defer wg.Done()
				// Build a fresh message per hub; protobuf messages must not be
				// shared across goroutines that may lazily populate caches.
				h.Send(&agentpb.ServerMessage{
					Payload: &agentpb.ServerMessage_Maintenance{
						Maintenance: &agentpb.MaintenanceNotice{
							Reason:              reason,
							ExpectedDurationSec: int32(durationSec),
						},
					},
				})
			}(hub)
		}
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(broadcastTimeout):
		r.log.Warn("maintenance broadcast timed out", "reason", reason, "agents", len(hubs))
	}
}

// CountOnline returns how many agents are currently connected.
func (r *Registry) CountOnline() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, a := range r.agents {
		if a.Status == "online" {
			n++
		}
	}
	return n
}

// CountByGroup returns how many agents are in the given group name.
func (r *Registry) CountByGroup(name string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, a := range r.agents {
		if a.Group == name {
			n++
		}
	}
	return n
}

// DeleteAgent removes an agent from the registry.
func (r *Registry) DeleteAgent(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.agents[id]; !ok {
		return fmt.Errorf("agent not found")
	}
	delete(r.agents, id)
	delete(r.hubs, id)
	delete(r.upgrades, id)
	// Clean up auth tokens pointing to this agent.
	for tok, aid := range r.tokens {
		if aid == id {
			delete(r.tokens, tok)
		}
	}
	_ = r.persistAgentsLocked()
	return nil
}

func (r *Registry) ReapStale() {
	r.mu.Lock()
	now := time.Now()
	// Snapshot the stale IDs under r.mu, then read hub.lastSeen under hub.mu:
	// lastSeen is also written by handleAgentMessage under hub.mu, and an
	// unsynchronized read here is a data race (torn time.Time on 32-bit).
	type staleHub struct {
		id        string
		heartbeat int32
	}
	var stale []staleHub
	for id, hub := range r.hubs {
		hub.mu.Lock()
		idle := now.Sub(hub.lastSeen)
		hb := hub.heartbeat
		hub.mu.Unlock()
		if idle > time.Duration(hb)*heartbeatGraceFactor*time.Second {
			stale = append(stale, staleHub{id: id, heartbeat: hb})
		}
	}
	for _, s := range stale {
		delete(r.hubs, s.id)
		if a, ok := r.agents[s.id]; ok {
			a.Status = "offline"
		}
		r.log.Info("agent reaped (stale)", "agent_id", s.id)
	}
	r.mu.Unlock()
}

// updateAgentMetrics writes the latest uptime and hardware info from a
// metrics sample back into the persisted Agent struct so the host list
// API always returns fresh values even without a live hub lookup.
func (r *Registry) updateAgentMetrics(agentID string, m *agentpb.MetricsSample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.agents[agentID]
	if !ok {
		return
	}
	if m.GetUptime() > 0 {
		a.Uptime = m.GetUptime()
	}
	if m.GetMemTotal() > 0 {
		a.MemTotal = m.GetMemTotal()
	}
	a.LastSeen = time.Now()
}

func newHub(agentID string, heartbeatSec int32, reg *Registry) *Hub {
	return &Hub{
		AgentID:      agentID,
		heartbeat:    heartbeatSec,
		lastSeen:     time.Now(),
		sendCh:       make(chan *agentpb.ServerMessage, 128),
		doneCh:       make(chan struct{}),
		termHandlers: make(map[string]func(*agentpb.TerminalOutput)),
		respHandlers: make(map[string]func(*agentpb.AgentMessage)),
		registry:     reg,
	}
}

func (h *Hub) bind(stream agentpb.AgentService_ConnectServer) {
	h.mu.Lock()
	h.stream = stream
	h.mu.Unlock()
	go h.sendPump(stream)
}

func (h *Hub) unbind() {
	h.mu.Lock()
	h.stream = nil
	if !h.closed {
		h.closed = true
		// Wake blocked Senders via doneCh; sendCh itself is never closed so
		// a concurrent Send can neither panic nor race the detector (§8.10).
		// The closed-guard also makes a second unbind a safe no-op.
		close(h.doneCh)
	}
	// Snapshot all handlers and clear the maps BEFORE invoking callbacks: the
	// handlers (e.g. metrics-poll in metrics.Store) re-enter SetRespHandler to
	// deregister themselves, which would self-deadlock on this non-reentrant
	// mutex if we called them while still holding it. That deadlock pinned
	// hub.mu forever and froze every reader (LastMetrics, alert monitor).
	handlers := make(map[string]func(*agentpb.AgentMessage), len(h.respHandlers))
	for id, fn := range h.respHandlers {
		handlers[id] = fn
		delete(h.respHandlers, id)
	}
	for id := range h.termHandlers {
		delete(h.termHandlers, id)
	}
	h.mu.Unlock()
	for _, fn := range handlers {
		if fn != nil {
			fn(nil) // signal disconnection
		}
	}
}

func (h *Hub) sendPump(stream agentpb.AgentService_ConnectServer) {
	for {
		select {
		case msg := <-h.sendCh:
			if err := stream.Send(msg); err != nil {
				return
			}
		case <-h.doneCh:
			// Hub unbound: the stream is dead, drop the rest and exit.
			return
		}
	}
}

func (h *Hub) Send(msg *agentpb.ServerMessage) bool {
	// Check stream availability and closed state under read lock, then release
	// immediately before attempting the channel send. Holding RLock across a
	// blocking channel send causes write starvation on Go's write-preferring
	// sync.RWMutex: a pending unbind() Lock() would stall every subsequent
	// RLock reader (LastMetrics, ReapStale), freezing the alert monitor.
	//
	// sendCh is never closed (unbind closes doneCh instead), so the send below
	// cannot race a channel close: no panic, no recover, race-detector clean.
	// A concurrent unbind wakes the select via doneCh and Send returns false.
	h.mu.RLock()
	if h.stream == nil || h.closed {
		h.mu.RUnlock()
		slog.Default().Warn("hub.Send: stream is nil or hub closed", "agent_id", h.AgentID)
		return false
	}
	ch := h.sendCh
	done := h.doneCh
	h.mu.RUnlock()

	select {
	case ch <- msg:
		return true
	case <-done:
		return false
	case <-time.After(5 * time.Second):
		slog.Default().Warn("hub.Send: sendCh full after 5s", "agent_id", h.AgentID)
		return false
	}
}

// handleAgentMessage processes agent->server messages.
func (h *Hub) handleAgentMessage(msg *agentpb.AgentMessage) error {
	switch p := msg.Payload.(type) {
	case *agentpb.AgentMessage_Heartbeat:
		h.mu.Lock()
		h.lastSeen = time.Unix(p.Heartbeat.GetTs(), 0)
		h.mu.Unlock()
		h.Send(&agentpb.ServerMessage{Payload: &agentpb.ServerMessage_Heartbeat{
			Heartbeat: &agentpb.HeartbeatAck{Ts: p.Heartbeat.GetTs()},
		}})
		return nil
	case *agentpb.AgentMessage_Ack:
		h.dispatchResp(p.Ack.GetRef(), msg)
		return nil
	case *agentpb.AgentMessage_TermOut:
		h.mu.Lock()
		fn := h.termHandlers[p.TermOut.GetSessionId()]
		h.mu.Unlock()
		if fn != nil {
			fn(p.TermOut)
		}
		return nil
	case *agentpb.AgentMessage_FileChunk:
		h.dispatchResp(p.FileChunk.GetOpId(), msg)
		return nil
	case *agentpb.AgentMessage_ExecResult:
		h.dispatchResp(p.ExecResult.GetExecId(), msg)
		return nil
	case *agentpb.AgentMessage_Metrics:
		h.mu.Lock()
		h.lastMetrics = p.Metrics
		h.mu.Unlock()
		if h.registry != nil && p.Metrics.GetUptime() > 0 {
			h.registry.updateAgentMetrics(h.AgentID, p.Metrics)
		}
		h.dispatchResp("metrics-live", msg)
		h.dispatchResp("metrics-poll", msg)
		return nil
	case *agentpb.AgentMessage_Sysinfo:
		// SysInfo responses are keyed by kind in the query; we use the kind as ref.
		h.dispatchResp("sysinfo:"+p.Sysinfo.GetKind(), msg)
		return nil
	case *agentpb.AgentMessage_Docker:
		h.dispatchResp(p.Docker.GetOpId(), msg)
		return nil
	case *agentpb.AgentMessage_ScanProgress:
		h.dispatchResp(p.ScanProgress.GetScanId(), msg)
		return nil
	case *agentpb.AgentMessage_UpgradeProgress:
		h.dispatchResp("upgrade", msg)
		if h.registry != nil && p.UpgradeProgress != nil {
			h.registry.SetAgentUpgradeProgress(h.AgentID, p.UpgradeProgress.GetStage(), p.UpgradeProgress.GetError())
		}
		return nil
	case *agentpb.AgentMessage_TunnelData:
		if h.tunnel != nil {
			h.tunnel.handleData(p.TunnelData)
		}
		return nil
	case *agentpb.AgentMessage_TunnelClose:
		if h.tunnel != nil {
			h.tunnel.handleClose(p.TunnelClose)
		}
		return nil
	default:
		return nil
	}
}

// dispatchResp calls the registered response handler for the given ref, if any.
// The handler is NOT auto-deleted — streaming responses (FileChunk) need the
// handler to remain registered across multiple chunks. Callers remove the
// handler by calling SetRespHandler(ref, nil) when done (e.g. on EOF, Ack,
// or timeout).
func (h *Hub) dispatchResp(ref string, msg *agentpb.AgentMessage) {
	h.mu.Lock()
	fn := h.respHandlers[ref]
	h.mu.Unlock()
	if fn != nil {
		fn(msg)
	}
}

// SetRespHandler registers a one-shot handler for a response with the given ref.
func (h *Hub) SetRespHandler(ref string, fn func(*agentpb.AgentMessage)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if fn == nil {
		delete(h.respHandlers, ref)
	} else {
		h.respHandlers[ref] = fn
	}
}

// SetTermHandler registers a handler for terminal output of a session.
func (h *Hub) SetTermHandler(sessionID string, fn func(*agentpb.TerminalOutput)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if fn == nil {
		delete(h.termHandlers, sessionID)
	} else {
		h.termHandlers[sessionID] = fn
	}
}

// LastMetrics returns the most recent metrics sample.
func (h *Hub) LastMetrics() *agentpb.MetricsSample {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastMetrics
}

func stripBearer(t string) string {
	if len(t) > 7 && t[:7] == "Bearer " {
		return t[7:]
	}
	return t
}

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// MockConnectForTest marks the hub as connected for unit tests without a real gRPC stream.
func (h *Hub) MockConnectForTest() {
	h.mu.Lock()
	h.stream = &mockConnectServer{}
	h.mu.Unlock()
}

type mockConnectServer struct {
	agentpb.AgentService_ConnectServer
}

// RecvForTest reads the next message sent to the agent's outbound channel.
func (h *Hub) RecvForTest(timeout time.Duration) (*agentpb.ServerMessage, bool) {
	select {
	case msg := <-h.sendCh:
		return msg, true
	case <-time.After(timeout):
		return nil, false
	}
}

// DispatchRespForTest dispatches a mock agent response to registered handlers.
func (h *Hub) DispatchRespForTest(ref string, msg *agentpb.AgentMessage) {
	h.dispatchResp(ref, msg)
}
