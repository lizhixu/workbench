// Package policy implements server-side high-risk command control for the
// control server. It persists a configurable blacklist/whitelist of regex
// patterns, checks incoming commands before they are dispatched to agents,
// and records an audit trail of blocked or confirmed-high-risk commands.
//
// The check engine is intentionally simple: blacklist wins over whitelist,
// and any matched high-risk pattern forces a confirmation round-trip
// (HTTP 409) unless the caller explicitly confirms via X-Confirm-Risk.
package policy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// RiskLevel classifies a command's risk.
type RiskLevel string

const (
	RiskLow    RiskLevel = "low"
	RiskMedium RiskLevel = "medium"
	RiskHigh   RiskLevel = "high"
	RiskBlock  RiskLevel = "blocked"
)

// Policy is the persisted command control configuration.
type Policy struct {
	Enabled          bool     `json:"enabled"`
	Blacklist        []string `json:"blacklist"`         // regex patterns; match => blocked
	Whitelist        []string `json:"whitelist"`         // regex patterns; match => allow (skip high-risk confirm)
	HighRiskPatterns []string `json:"high_risk_patterns"` // regex patterns; match => require confirm
}

// CheckResult is the outcome of evaluating a command against the policy.
type CheckResult struct {
	Allowed        bool      `json:"allowed"`
	RiskLevel      RiskLevel `json:"risk_level"`
	Reason         string    `json:"reason"`
	MatchedPattern string    `json:"matched_pattern,omitempty"`
	NeedsConfirm   bool      `json:"needs_confirm"`
}

// AuditEntry records a policy decision on a command.
type AuditEntry struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Username  string    `json:"username"`
	HostID    string    `json:"host_id"`
	HostIDs   []string  `json:"host_ids,omitempty"` // for batch exec
	Command   string    `json:"command"`
	Shell     string    `json:"shell"`
	RiskLevel RiskLevel `json:"risk_level"`
	Result    string    `json:"result"` // blocked / confirmed / allowed / denied
	Reason    string    `json:"reason"`
}

// Store persists the command policy and audit log.
type Store struct {
	mu       sync.Mutex
	dataDir  string
	log      *slog.Logger
	policy   Policy
	audit    []AuditEntry
	compiled struct {
		blacklist  []*regexp.Regexp
		whitelist  []*regexp.Regexp
		highRisk   []*regexp.Regexp
	}
}

// DefaultHighRiskPatterns are built-in high-risk command patterns that always
// apply (as a server-side baseline) even before user configuration.
var DefaultHighRiskPatterns = []string{
	`rm\s+-[rfRF]+\s+/(\s|$)`,        // rm -rf /
	`rm\s+-[rfRF]+\s+/\*`,            // rm -rf /*
	`mkfs`,                            // mkfs any filesystem
	`dd\s+if=.*of=/dev/(sd|nvme|hd)`, // dd to disk device
	`\bshutdown\b`,                    // shutdown
	`\breboot\b`,                      // reboot
	`\bhalt\b`,                        // halt
	`\bpoweroff\b`,                    // poweroff
	`:\s*\(\)\s*\{\s*:\|:\&\s*\}\s*;`, // fork bomb :(){:|:&};:
	`drop\s+table`,                    // sql drop table
	`drop\s+database`,                 // sql drop database
	`truncate\s+table`,                // sql truncate
	`chmod\s+-R\s+777\s+/`,           // chmod -R 777 /
	`>\s*/dev/sd[a-z]`,               // overwrite disk device
	`kill\s+-9\s+-1`,                  // kill all processes
	`killall\s+-9`,                    // killall -9
	`\binit\s+0\b`,                    // init 0 (shutdown)
	`\binit\s+6\b`,                    // init 6 (reboot)
	`systemctl\s+(stop|disable)\s+.*(ssh|sshd)`, // stopping ssh locks you out
}

// NewStore creates a policy store, loading the persisted policy and audit log.
func NewStore(dataDir string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{
		dataDir: dataDir,
		log:     log,
		policy: Policy{
			Enabled:          true,
			HighRiskPatterns: append([]string{}, DefaultHighRiskPatterns...),
		},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	s.compile()
	return s, nil
}

func (s *Store) load() error {
	if s.dataDir == "" {
		return nil
	}
	path := filepath.Join(s.dataDir, "command_policy.json")
	if data, err := os.ReadFile(path); err == nil {
		var p Policy
		if err := json.Unmarshal(data, &p); err != nil {
			return fmt.Errorf("parse command_policy.json: %w", err)
		}
		// Ensure built-in high-risk patterns are always present.
		s.policy = mergeDefaults(p)
	}

	auditPath := filepath.Join(s.dataDir, "command_audit.json")
	if data, err := os.ReadFile(auditPath); err == nil {
		var entries []AuditEntry
		if err := json.Unmarshal(data, &entries); err == nil {
			s.audit = entries
		}
	}
	return nil
}

// mergeDefaults ensures built-in high-risk patterns are always present even
// if the persisted config predates them.
func mergeDefaults(p Policy) Policy {
	have := make(map[string]bool, len(p.HighRiskPatterns))
	for _, pat := range p.HighRiskPatterns {
		have[pat] = true
	}
	for _, d := range DefaultHighRiskPatterns {
		if !have[d] {
			p.HighRiskPatterns = append(p.HighRiskPatterns, d)
		}
	}
	return p
}

func (s *Store) savePolicy() error {
	if s.dataDir == "" {
		return nil
	}
	path := filepath.Join(s.dataDir, "command_policy.json")
	data, err := json.MarshalIndent(s.policy, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func (s *Store) saveAudit() error {
	if s.dataDir == "" {
		return nil
	}
	path := filepath.Join(s.dataDir, "command_audit.json")
	// Cap the audit log to the most recent 1000 entries.
	if len(s.audit) > 1000 {
		s.audit = s.audit[len(s.audit)-1000:]
	}
	data, err := json.MarshalIndent(s.audit, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func (s *Store) compile() {
	s.compiled.blacklist = compileAll(s.policy.Blacklist)
	s.compiled.whitelist = compileAll(s.policy.Whitelist)
	s.compiled.highRisk = compileAll(s.policy.HighRiskPatterns)
}

func compileAll(patterns []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			continue
		}
		out = append(out, re)
	}
	return out
}

// GetPolicy returns a copy of the current policy. Pattern lists are always
// non-nil: a nil slice marshals to JSON `null`, and clients that iterate or
// read `.length` on it break.
func (s *Store) GetPolicy() Policy {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.policy
	if p.Blacklist == nil {
		p.Blacklist = []string{}
	}
	if p.Whitelist == nil {
		p.Whitelist = []string{}
	}
	if p.HighRiskPatterns == nil {
		p.HighRiskPatterns = []string{}
	}
	return p
}

// SetPolicy updates the policy, recompiles patterns, and persists.
func (s *Store) SetPolicy(p Policy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policy = mergeDefaults(p)
	s.compile()
	return s.savePolicy()
}

// Check evaluates a command against the policy.
func (s *Store) Check(command string) *CheckResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.policy.Enabled {
		return &CheckResult{Allowed: true, RiskLevel: RiskLow, Reason: "policy disabled"}
	}

	// 1. Whitelist short-circuits (trusted commands skip confirm).
	for _, re := range s.compiled.whitelist {
		if re.MatchString(command) {
			return &CheckResult{Allowed: true, RiskLevel: RiskLow, Reason: "whitelisted", MatchedPattern: re.String()}
		}
	}

	// 2. Blacklist => blocked outright.
	for _, re := range s.compiled.blacklist {
		if re.MatchString(command) {
			return &CheckResult{Allowed: false, RiskLevel: RiskBlock, Reason: "命令命中黑名单规则", MatchedPattern: re.String()}
		}
	}

	// 3. High-risk => needs confirmation but allowed if confirmed.
	for _, re := range s.compiled.highRisk {
		if re.MatchString(command) {
			return &CheckResult{Allowed: true, RiskLevel: RiskHigh, Reason: "命令属于高危操作，需要二次确认", MatchedPattern: re.String(), NeedsConfirm: true}
		}
	}

	return &CheckResult{Allowed: true, RiskLevel: RiskLow, Reason: "ok"}
}

// CheckRisk is a flattened adapter over Check for consumers (the AI planner)
// that only need the risk classification, without importing the CheckResult
// type. It returns the risk level string, whether the command is blocked
// outright, whether it needs confirmation, and the matched pattern.
func (s *Store) CheckRisk(command string) (riskLevel string, blocked bool, needsConfirm bool, matchedPattern string) {
	res := s.Check(command)
	return string(res.RiskLevel), !res.Allowed, res.NeedsConfirm, res.MatchedPattern
}

// RecordAudit appends an audit entry and persists.
func (s *Store) RecordAudit(entry AuditEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry.ID == "" {
		entry.ID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now()
	}
	s.audit = append(s.audit, entry)
	_ = s.saveAudit()
}

// ListAudit returns audit entries (newest first), optionally filtered.
func (s *Store) ListAudit(limit int, username, hostID string) []AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AuditEntry, 0, len(s.audit))
	for i := len(s.audit) - 1; i >= 0; i-- {
		e := s.audit[i]
		if username != "" && e.Username != username {
			continue
		}
		if hostID != "" && e.HostID != hostID && !containsID(e.HostIDs, hostID) {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func containsID(ids []string, target string) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}