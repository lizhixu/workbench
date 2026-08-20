// Package alert implements rule-based alerting for the control server.
//
// Rules are JSON-persisted and evaluated periodically by a Monitor goroutine
// against the live agent registry. When a rule fires, an Event is recorded
// and (if configured) a webhook notification is sent. Events are also
// persisted so the audit trail survives restarts.
package alert

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// RuleType enumerates the supported alert rule conditions.
type RuleType string

const (
	RuleOffline    RuleType = "offline"     // agent goes offline
	RuleCPUHigh    RuleType = "cpu_high"    // CPU usage > threshold % for duration
	RuleMemHigh    RuleType = "mem_high"    // memory usage > threshold % for duration
	RuleDiskHigh   RuleType = "disk_high"   // any mount usage > threshold %
	RuleAnomaly    RuleType = "anomaly"     // AI/statistical anomaly: value deviates > threshold × σ from recent mean
)

// Severity classifies alert events.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Rule defines an alert condition.
type Rule struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Type       RuleType  `json:"type"`
	Severity   Severity  `json:"severity"`
	Threshold  float64   `json:"threshold"`  // e.g. 90 for 90%; for anomaly = σ multiplier (e.g. 3)
	Duration   int       `json:"duration"`   // seconds the condition must hold (0 = immediate)
	Metric     string    `json:"metric,omitempty"`  // for anomaly: cpu / mem / net_rx / net_tx / disk_read / disk_write
	HostFilter  string   `json:"host_filter"` // empty = all hosts; otherwise hostname substring
	GroupFilter string   `json:"group_filter"` // empty = all groups
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
}

// Event is a fired alert.
type Event struct {
	ID               string    `json:"id"`
	RuleID           string    `json:"rule_id"`
	RuleName         string    `json:"rule_name"`
	Severity         Severity  `json:"severity"`
	HostID           string    `json:"host_id"`
	Hostname         string    `json:"hostname"`
	Message          string    `json:"message"`
	FiredAt          time.Time `json:"fired_at"`
	Resolved         bool      `json:"resolved"`
	ResolvedAt       time.Time `json:"resolved_at,omitempty"`
	AIInterpretation string    `json:"ai_interpretation,omitempty"` // AI-generated root-cause/suggestion
}

// WebhookConfig defines where notifications are sent.
type WebhookConfig struct {
	URL     string `json:"url"`
	Secret  string `json:"secret"`
	Enabled bool   `json:"enabled"`
}

// Store persists rules, events, and webhook config.
type Store struct {
	mu        sync.RWMutex
	rules     map[string]*Rule
	events    []*Event
	webhook   WebhookConfig
	filePath  string
	// Track which rules are currently firing per host (to avoid duplicate events).
	firing map[string]bool // key = ruleID:hostID
}

// NewStore loads (or creates) the alert store.
func NewStore(dataDir string) (*Store, error) {
	if dataDir == "" {
		dataDir = filepath.Join(os.TempDir(), "watchman")
	}
	s := &Store{
		rules:    map[string]*Rule{},
		filePath: filepath.Join(dataDir, "alerts.json"),
		firing:   map[string]bool{},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

type persisted struct {
	Rules   []*Rule        `json:"rules"`
	Events  []*Event       `json:"events"`
	Webhook WebhookConfig  `json:"webhook"`
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	s.mu.Lock()
	for _, r := range p.Rules {
		s.rules[r.ID] = r
	}
	s.events = p.Events
	s.webhook = p.Webhook
	s.mu.Unlock()
	return nil
}

func (s *Store) persist() error {
	s.mu.RLock()
	p := persisted{
		Rules:   make([]*Rule, 0, len(s.rules)),
		Events:  s.events,
		Webhook: s.webhook,
	}
	for _, r := range s.rules {
		p.Rules = append(p.Rules, r)
	}
	s.mu.RUnlock()
	// Cap events at 500 to prevent unbounded growth.
	if len(p.Events) > 500 {
		p.Events = p.Events[len(p.Events)-500:]
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.filePath, data, 0o600)
}

// ---- Rules ----

func (s *Store) ListRules() []*Rule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Rule, 0, len(s.rules))
	for _, r := range s.rules {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) CreateRule(r *Rule) error {
	if r.ID == "" {
		r.ID = randomID()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	s.mu.Lock()
	s.rules[r.ID] = r
	s.mu.Unlock()
	return s.persist()
}

func (s *Store) UpdateRule(r *Rule) error {
	s.mu.Lock()
	if _, ok := s.rules[r.ID]; !ok {
		s.mu.Unlock()
		return fmt.Errorf("rule not found")
	}
	s.rules[r.ID] = r
	s.mu.Unlock()
	return s.persist()
}

func (s *Store) DeleteRule(id string) error {
	s.mu.Lock()
	delete(s.rules, id)
	s.mu.Unlock()
	return s.persist()
}

// ---- Events ----

func (s *Store) ListEvents(limit int) []*Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit > 0 && len(s.events) > limit {
		out := make([]*Event, limit)
		copy(out, s.events[len(s.events)-limit:])
		return out
	}
	out := make([]*Event, len(s.events))
	copy(out, s.events)
	return out
}

func (s *Store) addEvent(e *Event) {
	s.mu.Lock()
	s.events = append(s.events, e)
	if len(s.events) > 500 {
		s.events = s.events[len(s.events)-500:]
	}
	s.mu.Unlock()
}

// AckEvent marks an event as resolved.
func (s *Store) AckEvent(id string) error {
	s.mu.Lock()
	found := false
	for _, e := range s.events {
		if e.ID == id {
			e.Resolved = true
			e.ResolvedAt = time.Now()
			found = true
			break
		}
	}
	s.mu.Unlock()
	if !found {
		return fmt.Errorf("event not found")
	}
	return s.persist()
}

// AckAllEvents marks all unacknowledged events as resolved.
func (s *Store) AckAllEvents() error {
	s.mu.Lock()
	now := time.Now()
	for _, e := range s.events {
		if !e.Resolved {
			e.Resolved = true
			e.ResolvedAt = now
		}
	}
	s.mu.Unlock()
	return s.persist()
}

// ---- Webhook ----

func (s *Store) GetWebhook() WebhookConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.webhook
}

func (s *Store) SetWebhook(w WebhookConfig) error {
	s.mu.Lock()
	s.webhook = w
	s.mu.Unlock()
	return s.persist()
}

// ---- Internal helpers ----

func (s *Store) isFiring(key string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.firing[key]
}

func (s *Store) setFiring(key string, v bool) {
	s.mu.Lock()
	s.firing[key] = v
	s.mu.Unlock()
}

func randomID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}