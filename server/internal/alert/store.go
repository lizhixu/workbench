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
	"strings"
	"sync"
	"time"
)

// RuleType enumerates the supported alert rule conditions.
type RuleType string

const (
	RuleOffline  RuleType = "offline"   // agent goes offline
	RuleOnline   RuleType = "online"    // agent comes online
	RuleCPUHigh  RuleType = "cpu_high"  // CPU usage > threshold % for duration
	RuleMemHigh  RuleType = "mem_high"  // memory usage > threshold % for duration
	RuleDiskHigh RuleType = "disk_high" // any mount usage > threshold %
	RuleAnomaly  RuleType = "anomaly"   // AI/statistical anomaly: value deviates > threshold × σ from recent mean
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
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Type        RuleType  `json:"type"`
	Severity    Severity  `json:"severity"`
	Threshold   float64   `json:"threshold"`        // e.g. 90 for 90%; for anomaly = σ multiplier (e.g. 3)
	Duration    int       `json:"duration"`         // seconds the condition must hold (0 = immediate)
	Metric      string    `json:"metric,omitempty"` // for anomaly: cpu / mem / net_rx / net_tx / disk_read / disk_write
	HostFilter  string    `json:"host_filter"`      // empty = all hosts; otherwise hostname substring
	GroupFilter string    `json:"group_filter"`     // empty = all groups
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
}

// Event is a fired alert.
type Event struct {
	ID               string    `json:"id"`
	RuleID           string    `json:"rule_id"`
	RuleName         string    `json:"rule_name"`
	Severity         Severity  `json:"severity"`
	HostID           string    `json:"host_id"`
	Hostname         string    `json:"hostname"`
	CertID           string    `json:"cert_id,omitempty"` // set for certificate alerts (e.g. builtin-cert-expiry)
	Message          string    `json:"message"`
	FiredAt          time.Time `json:"fired_at"`
	Resolved         bool      `json:"resolved"`
	ResolvedAt       time.Time `json:"resolved_at,omitempty"`
	AIInterpretation string    `json:"ai_interpretation,omitempty"` // AI-generated root-cause/suggestion
}

// firingKeyOf returns the dedup key for an event: ruleID:hostID for host
// alerts, ruleID:cert:<certID> for certificate alerts. The ":cert:" segment
// keeps certificate keys from colliding with host keys.
func firingKeyOf(ruleID, hostID, certID string) string {
	if certID != "" {
		return ruleID + ":cert:" + certID
	}
	return ruleID + ":" + hostID
}

// WebhookConfig defines where notifications are sent.
type WebhookConfig struct {
	URL     string `json:"url"`
	Secret  string `json:"secret"`
	Enabled bool   `json:"enabled"`
}

// Store persists rules, events, and webhook config.
type Store struct {
	mu       sync.RWMutex
	rules    map[string]*Rule
	events   []*Event
	webhook  WebhookConfig
	filePath string
	// Track which rules are currently firing per host (to avoid duplicate events).
	firing map[string]bool // key = ruleID:hostID
	// Track when each rule:host condition was first observed true, so rules with
	// a Duration only fire once the condition has held that long. In-memory only:
	// a restart deliberately restarts every sustain window rather than firing on
	// a window it never actually observed.
	pending map[string]time.Time // key = ruleID:hostID
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
		pending:  map[string]time.Time{},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	// Seed the built-in "host online"/"host offline" rules (AGENTS.md 3.11:
	// built-in monitor items). Idempotent: only inserts when no rule of that
	// type exists, so users who delete or customise a default keep their change.
	s.seedBuiltinRules()
	return s, nil
}

// seedBuiltinRules installs the built-in online/offline notification rules the
// first time the store is created, so out-of-the-box deployments get host
// up/down alerts without manual setup. Existing rules of the same type are left
// untouched.
func (s *Store) seedBuiltinRules() {
	seeds := []*Rule{
		{
			ID:       "builtin-online",
			Name:     "主机上线通知",
			Type:     RuleOnline,
			Severity: SeverityInfo,
			Enabled:  true,
		},
		{
			ID:       "builtin-offline",
			Name:     "主机离线告警",
			Type:     RuleOffline,
			Severity: SeverityCritical,
			Enabled:  true,
		},
	}

	existing := map[RuleType]bool{}
	for _, r := range s.rules {
		existing[r.Type] = true
	}

	changed := false
	for _, seed := range seeds {
		if existing[seed.Type] {
			continue
		}
		seed.CreatedAt = time.Now()
		s.rules[seed.ID] = seed
		changed = true
	}
	if changed {
		_ = s.persist()
	}
}

type persisted struct {
	Rules   []*Rule       `json:"rules"`
	Events  []*Event      `json:"events"`
	Webhook WebhookConfig `json:"webhook"`
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
	// Restore active alerts into s.firing so a server restart does not re-fire
	// existing un-resolved alerts (CPU/mem/disk high, offline, cert expiry, etc.).
	for _, e := range s.events {
		if e.Resolved || e.RuleID == "" {
			continue
		}
		if e.CertID != "" {
			s.firing[firingKeyOf(e.RuleID, "", e.CertID)] = true
			continue
		}
		if e.HostID != "" {
			s.firing[firingKeyOf(e.RuleID, e.HostID, "")] = true
		}
	}
	// Deduplicate redundant historical "builtin-online" events for the same host:
	// Server restarts previously generated duplicate "host online" notifications
	// on each boot. Keep only the most recent online notification per host.
	s.dedupHistoricalOnlineEventsLocked()
	s.mu.Unlock()
	return nil
}

// dedupHistoricalOnlineEventsLocked cleans up duplicate historical online events
// generated by previous control server restarts. Caller must hold s.mu.
func (s *Store) dedupHistoricalOnlineEventsLocked() {
	seenOnlineHost := make(map[string]bool)
	var filtered []*Event
	// Events are stored chronological (oldest to newest). Iterate in reverse to
	// retain the most recent online notification per host.
	for i := len(s.events) - 1; i >= 0; i-- {
		e := s.events[i]
		if e.RuleID == "builtin-online" {
			if seenOnlineHost[e.HostID] {
				continue
			}
			seenOnlineHost[e.HostID] = true
		}
		filtered = append(filtered, e)
	}
	// Reverse back to chronological order.
	for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
		filtered[i], filtered[j] = filtered[j], filtered[i]
	}
	if len(filtered) != len(s.events) {
		s.events = filtered
		_ = s.persistLocked()
	}
}

func (s *Store) snapshotForPersistLocked() persisted {
	rules := make([]*Rule, 0, len(s.rules))
	for _, r := range s.rules {
		rules = append(rules, r)
	}
	events := make([]*Event, len(s.events))
	copy(events, s.events)
	if len(events) > 500 {
		events = events[len(events)-500:]
	}
	return persisted{
		Rules:   rules,
		Events:  events,
		Webhook: s.webhook,
	}
}

func atomicWriteFile(filePath string, data []byte) error {
	dir := filepath.Dir(filePath)
	tmp := filepath.Join(dir, fmt.Sprintf("%s.tmp.%d", filepath.Base(filePath), time.Now().UnixNano()))
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filePath); err != nil {
		_ = os.Remove(filePath)
		if err2 := os.Rename(tmp, filePath); err2 != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	return nil
}

func (s *Store) persistLocked() error {
	p := s.snapshotForPersistLocked()
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(s.filePath, data)
}

func (s *Store) persist() error {
	s.mu.RLock()
	p := s.snapshotForPersistLocked()
	s.mu.RUnlock()

	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(s.filePath, data)
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

// DeleteEvent removes a specific event by ID.
func (s *Store) DeleteEvent(id string) error {
	s.mu.Lock()
	idx := -1
	for i, e := range s.events {
		if e.ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		s.mu.Unlock()
		return fmt.Errorf("event not found")
	}
	s.events = append(s.events[:idx], s.events[idx+1:]...)
	s.mu.Unlock()
	return s.persist()
}

// ClearEvents removes all events or only resolved events.
func (s *Store) ClearEvents(resolvedOnly bool) error {
	s.mu.Lock()
	if resolvedOnly {
		unresolved := make([]*Event, 0, len(s.events))
		for _, e := range s.events {
			if !e.Resolved {
				unresolved = append(unresolved, e)
			}
		}
		s.events = unresolved
	} else {
		s.events = make([]*Event, 0)
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

// firingCertIDs returns the certificate IDs that currently hold an active
// firing entry for ruleID. Used to sweep reminders whose certificate was
// deleted while the alert was still firing.
func (s *Store) firingCertIDs(ruleID string) []string {
	prefix := ruleID + ":cert:"
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for k, v := range s.firing {
		if v && strings.HasPrefix(k, prefix) {
			out = append(out, strings.TrimPrefix(k, prefix))
		}
	}
	return out
}

// markPending records the first time a rule's condition was seen true for a
// host and reports how long it has held. The second return value is false when
// this is the first observation, so callers can distinguish "just started" from
// "has been true for 0s".
func (s *Store) markPending(key string, now time.Time) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	since, ok := s.pending[key]
	if !ok {
		s.pending[key] = now
		return 0, false
	}
	return now.Sub(since), true
}

// clearPending forgets the sustain window for a rule:host pair, so the next
// time the condition appears it starts counting from zero.
func (s *Store) clearPending(key string) {
	s.mu.Lock()
	delete(s.pending, key)
	s.mu.Unlock()
}

func randomID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
