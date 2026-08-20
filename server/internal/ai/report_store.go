package ai

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// OpsReport is a generated operational health report.
type OpsReport struct {
	ID          string         `json:"id"`
	GeneratedAt time.Time      `json:"generated_at"`
	Period      string         `json:"period"`       // e.g. "24h", "7d"
	HostIDs     []string       `json:"host_ids"`     // scope
	Summary     string         `json:"summary"`      // AI-written overview
	HealthScore float64        `json:"health_score"` // 0-100
	HostReports []HostReport   `json:"host_reports"`
	Suggestions []string       `json:"suggestions"`
	Model       string         `json:"model"`
}

// HostReport is a per-host section of an ops report.
type HostReport struct {
	HostID   string  `json:"host_id"`
	Hostname string  `json:"hostname"`
	Summary  string  `json:"summary"`
	Score    float64 `json:"score"`
	Alerts   int     `json:"alerts"`
}

// reportStore persists generated ops reports to disk.
type reportStore struct {
	mu      sync.Mutex
	dataDir string
	log     *slog.Logger
	reports map[string]*OpsReport
}

func newReportStore(dataDir string, log *slog.Logger) *reportStore {
	if log == nil {
		log = slog.Default()
	}
	rs := &reportStore{
		dataDir: dataDir,
		log:     log,
		reports: make(map[string]*OpsReport),
	}
	rs.load()
	return rs
}

func (rs *reportStore) dir() string {
	if rs.dataDir == "" {
		return ""
	}
	d := filepath.Join(rs.dataDir, "ops_reports")
	_ = os.MkdirAll(d, 0o755)
	return d
}

func (rs *reportStore) load() {
	d := rs.dir()
	if d == "" {
		return
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(d, e.Name()))
		if err != nil {
			continue
		}
		var r OpsReport
		if json.Unmarshal(data, &r) == nil && r.ID != "" {
			rs.reports[r.ID] = &r
		}
	}
}

func (rs *reportStore) Save(r *OpsReport) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.reports[r.ID] = r
	d := rs.dir()
	if d == "" {
		return nil
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(d, fmt.Sprintf("%s.json", r.ID)), data, 0o600)
}

func (rs *reportStore) Get(id string) (*OpsReport, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	r, ok := rs.reports[id]
	return r, ok
}

func (rs *reportStore) List() []*OpsReport {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make([]*OpsReport, 0, len(rs.reports))
	for _, r := range rs.reports {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].GeneratedAt.After(out[j].GeneratedAt)
	})
	return out
}