// Package audit implements the unified operational audit trail for the
// control server. It records who did what to which host at what time —
// logins, terminal sessions, command dispatch, file operations, host
// management, Docker operations and user management — with the result and
// risk classification of each action.
//
// Entries are appended to a JSONL file (one JSON object per line) so writes
// are cheap and crash-safe; the most recent maxEntries are also kept in
// memory for fast filtered queries. Entries are never rewritten.
package audit

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Risk levels, aligned with the policy package's classification.
const (
	RiskLow    = "low"
	RiskMedium = "medium"
	RiskHigh   = "high"
)

// Results.
const (
	ResultSuccess = "success"
	ResultFailed  = "failed"
	ResultBlocked = "blocked"
)

// Entry is a single audited action.
type Entry struct {
	ID         string    `json:"id"`
	Timestamp  time.Time `json:"timestamp"`
	Username   string    `json:"username"`
	Action     string    `json:"action"`      // login / exec / file_mkdir / host_unbind / ...
	TargetType string    `json:"target_type"` // host / user / session / system / file
	TargetID   string    `json:"target_id,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	IP         string    `json:"ip,omitempty"`
	UserAgent  string    `json:"user_agent,omitempty"`
	RiskLevel  string    `json:"risk_level"` // low / medium / high
	Result     string    `json:"result"`     // success / failed / blocked
}

// Query filters for listing entries.
type Query struct {
	Username string
	Action   string // exact action or prefix match via trailing "*"
	TargetID string
	Result   string
	Risk     string
	From     time.Time
	To       time.Time
	Limit    int
	Offset   int
}

// Stats summarizes the entries matching a query (ignoring pagination).
type Stats struct {
	Total    int `json:"total"`
	Today    int `json:"today"`
	HighRisk int `json:"high_risk"`
	Failed   int `json:"failed"`
}

const maxEntries = 5000

// Store persists audit entries to dataDir/audit.log (JSONL).
type Store struct {
	mu      sync.Mutex
	dataDir string
	log     *slog.Logger
	entries []Entry // newest last
	nextID  int64
}

// NewStore creates the audit store and loads recent history from disk.
func NewStore(dataDir string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{dataDir: dataDir, log: log, entries: make([]Entry, 0, 256)}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load reads the trailing portion of the JSONL log into memory. Only the
// last maxEntries lines are parsed to bound startup cost.
func (s *Store) load() error {
	if s.dataDir == "" {
		return nil
	}
	path := filepath.Join(s.dataDir, "audit.log")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read audit.log: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > maxEntries+1 {
		lines = lines[len(lines)-maxEntries-1:]
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // tolerate a torn last line
		}
		s.entries = append(s.entries, e)
		if id := parseID(e.ID); id > s.nextID {
			s.nextID = id
		}
	}
	s.log.Info("audit store loaded", "entries", len(s.entries))
	return nil
}

func parseID(id string) int64 {
	var n int64
	_, _ = fmt.Sscanf(id, "a%d", &n)
	return n
}

// Record appends an entry, filling defaults, and persists it.
func (s *Store) Record(e Entry) {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	if e.RiskLevel == "" {
		e.RiskLevel = RiskLow
	}
	if e.Result == "" {
		e.Result = ResultSuccess
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	e.ID = fmt.Sprintf("a%d", s.nextID)
	s.entries = append(s.entries, e)
	if len(s.entries) > maxEntries {
		s.entries = s.entries[len(s.entries)-maxEntries:]
	}
	_ = s.appendLine(e)
}

// appendLine writes one JSON line; must be called with the lock held.
func (s *Store) appendLine(e Entry) error {
	if s.dataDir == "" {
		return nil
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.dataDir, "audit.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// List returns entries matching the query (newest first) plus stats over the
// full filtered set.
func (s *Store) List(q Query) ([]Entry, Stats) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	var stats Stats
	matched := make([]Entry, 0, 64)

	for i := len(s.entries) - 1; i >= 0; i-- {
		e := s.entries[i]
		if !q.match(e) {
			continue
		}
		stats.Total++
		if !e.Timestamp.Before(today) {
			stats.Today++
		}
		if e.RiskLevel == RiskHigh {
			stats.HighRisk++
		}
		if e.Result != ResultSuccess {
			stats.Failed++
		}
		matched = append(matched, e)
	}

	start, end := q.Offset, q.Offset+q.Limit
	if start > len(matched) {
		start = len(matched)
	}
	if end > len(matched) || q.Limit <= 0 {
		end = len(matched)
	}
	return matched[start:end], stats
}

func (q Query) match(e Entry) bool {
	if q.Username != "" && !strings.EqualFold(e.Username, q.Username) {
		return false
	}
	if q.Action != "" && !matchAction(q.Action, e.Action) {
		return false
	}
	if q.TargetID != "" && !strings.Contains(e.TargetID, q.TargetID) {
		return false
	}
	if q.Result != "" && e.Result != q.Result {
		return false
	}
	if q.Risk != "" && e.RiskLevel != q.Risk {
		return false
	}
	if !q.From.IsZero() && e.Timestamp.Before(q.From) {
		return false
	}
	if !q.To.IsZero() && e.Timestamp.After(q.To) {
		return false
	}
	return true
}

// matchAction supports exact match ("exec") or prefix match when the filter
// ends with "*" ("file_*" matches file_mkdir, file_upload, ...).
func matchAction(pattern, action string) bool {
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(action, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == action
}

// RequestInfo extracts client IP and user agent from a request. The IP
// prefers the proxy-forwarded address set by the fronting nginx.
func RequestInfo(r *http.Request) (ip, ua string) {
	ip = r.Header.Get("X-Real-IP")
	if ip == "" {
		ip = r.Header.Get("X-Forwarded-For")
		if i := strings.Index(ip, ","); i > 0 {
			ip = strings.TrimSpace(ip[:i])
		}
	}
	if ip == "" && r != nil && r.RemoteAddr != "" {
		if i := strings.LastIndex(r.RemoteAddr, ":"); i > 0 {
			ip = r.RemoteAddr[:i]
		} else {
			ip = r.RemoteAddr
		}
	}
	return ip, r.Header.Get("User-Agent")
}

// ToCSV renders the matching entries as CSV for export.
func (s *Store) ToCSV(q Query) []byte {
	entries, _ := s.List(Query{
		Username: q.Username, Action: q.Action, TargetID: q.TargetID,
		Result: q.Result, Risk: q.Risk, From: q.From, To: q.To,
		Limit: maxEntries,
	})
	var b strings.Builder
	b.WriteString("id,timestamp,username,action,target_type,target_id,risk_level,result,ip,detail\n")
	for _, e := range entries {
		b.WriteString(fmt.Sprintf("%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n",
			e.ID, e.Timestamp.Format(time.RFC3339), csv(e.Username), csv(e.Action),
			csv(e.TargetType), csv(e.TargetID), csv(e.RiskLevel), csv(e.Result),
			csv(e.IP), csv(e.Detail)))
	}
	return []byte(b.String())
}

// csv quotes a CSV field when it contains separators or quotes.
func csv(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		return "\"" + strings.ReplaceAll(s, "\"", "\"\"") + "\""
	}
	return s
}
