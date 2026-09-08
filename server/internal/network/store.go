package network

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// NetworkConfig holds global overlay network settings (Headscale or Tailscale SaaS).
type NetworkConfig struct {
	ControlPlane      string    `json:"control_plane"`        // "headscale" or "tailscale"
	ServerURL         string    `json:"server_url"`           // e.g. "https://headscale.example.com", empty for official SaaS
	AuthKey           string    `json:"auth_key"`             // Pre-shared enrollment auth key
	AcceptRoutes      bool      `json:"accept_routes"`        // Pass --accept-routes=true
	AdvertiseExitNode bool      `json:"advertise_exit_node"`  // Allow advertising as exit node
	UpdatedAt         time.Time `json:"updated_at"`
}

// NodeStatus tracks cached network status for an agent node.
type NodeStatus struct {
	HostID       string    `json:"host_id"`
	Installed    bool      `json:"installed"`
	Online       bool      `json:"online"`       // Wireguard link state
	IP           string    `json:"ip"`           // 100.x.y.z Tailscale IPv4
	IPv6         string    `json:"ipv6"`         // Tailscale IPv6
	NodeName     string    `json:"node_name"`    // FQDN or hostname registered in tailscale
	Version      string    `json:"version"`      // tailscale client version
	Direct       bool      `json:"direct"`       // Whether P2P direct link is established
	DERP         string    `json:"derp"`         // Relay region, e.g. "DERP(tok)" or "Direct"
	LatencyMS    float64   `json:"latency_ms"`   // Ping or relay latency
	Subnets      []string  `json:"subnets"`      // Advertised or accepted subnets
	LastChecked  time.Time `json:"last_checked"`
	ErrorMessage string    `json:"error_message,omitempty"`
}

// Store persists overlay network configurations and cached node status snapshots.
type Store struct {
	mu       sync.RWMutex
	dataDir  string
	log      *slog.Logger
	config   NetworkConfig
	nodes    map[string]NodeStatus // keyed by host_id
}

type fileState struct {
	Config NetworkConfig         `json:"config"`
	Nodes  map[string]NodeStatus `json:"nodes"`
}

// NewStore loads or initializes the network store.
func NewStore(dataDir string, log *slog.Logger) (*Store, error) {
	s := &Store{
		dataDir: dataDir,
		log:     log,
		nodes:   make(map[string]NodeStatus),
		config: NetworkConfig{
			ControlPlane: "headscale",
			ServerURL:    "",
			AuthKey:      "",
			AcceptRoutes: true,
			UpdatedAt:    time.Now(),
		},
	}

	path := filepath.Join(dataDir, "network.json")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if err := s.saveLocked(); err != nil {
				return nil, fmt.Errorf("init network.json: %w", err)
			}
			return s, nil
		}
		return nil, fmt.Errorf("read network.json: %w", err)
	}

	var state fileState
	if err := json.Unmarshal(b, &state); err != nil {
		log.Warn("network.json corrupted, reinitializing", "err", err)
		return s, nil
	}

	if state.Config.ControlPlane != "" {
		s.config = state.Config
	}
	if state.Nodes != nil {
		s.nodes = state.Nodes
	}

	return s, nil
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(s.dataDir, 0755); err != nil {
		return err
	}
	state := fileState{
		Config: s.config,
		Nodes:  s.nodes,
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dataDir, fmt.Sprintf("network.json.tmp.%d", time.Now().UnixNano()))
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	target := filepath.Join(s.dataDir, "network.json")
	return os.Rename(tmp, target)
}

// GetConfig returns the current network configuration.
func (s *Store) GetConfig() NetworkConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}

// UpdateConfig updates network configuration.
func (s *Store) UpdateConfig(cfg NetworkConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg.UpdatedAt = time.Now()
	s.config = cfg
	return s.saveLocked()
}

// GetNodeStatus returns cached node status.
func (s *Store) GetNodeStatus(hostID string) (NodeStatus, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.nodes[hostID]
	return st, ok
}

// GetAllNodes returns a copy of all cached node statuses.
func (s *Store) GetAllNodes() map[string]NodeStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	res := make(map[string]NodeStatus, len(s.nodes))
	for k, v := range s.nodes {
		res[k] = v
	}
	return res
}

// SetNodeStatus updates or saves status for a specific node.
func (s *Store) SetNodeStatus(st NodeStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st.LastChecked = time.Now()
	s.nodes[st.HostID] = st
	return s.saveLocked()
}

// DeleteNode removes a node's cached status.
func (s *Store) DeleteNode(hostID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.nodes, hostID)
	return s.saveLocked()
}
