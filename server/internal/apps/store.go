package apps

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

const (
	appsFile         = "apps.json"
	deploymentsFile  = "app_deployments.jsonl"
	maxDeployHistory = 200 // per-application retained deployment records
)

// Store persists application entities and their deployment history, following
// the same in-memory + atomic-file pattern as the other watchman stores
// (network, commands, ...). Deployment history is a JSONL append log, so a
// crash mid-write drops at most one record.
type Store struct {
	mu   sync.RWMutex
	dir  string
	log  *slog.Logger
	apps map[string]*Application
	// deployments per app_id, newest first (index 0).
	deployments map[string][]*Deployment
}

// NewStore loads or initializes the application store under dataDir.
func NewStore(dataDir string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{
		dir:         dataDir,
		log:         log,
		apps:        make(map[string]*Application),
		deployments: make(map[string][]*Deployment),
	}
	if err := s.loadApps(); err != nil {
		return nil, err
	}
	if err := s.loadDeployments(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) loadApps() error {
	b, err := os.ReadFile(filepath.Join(s.dir, appsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", appsFile, err)
	}
	var list []*Application
	if err := json.Unmarshal(b, &list); err != nil {
		s.log.Warn("apps.json corrupted, starting empty", "err", err)
		return nil
	}
	for _, a := range list {
		s.apps[a.ID] = a
	}
	return nil
}

func (s *Store) loadDeployments() error {
	b, err := os.ReadFile(filepath.Join(s.dir, deploymentsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", deploymentsFile, err)
	}
	// JSONL: keep parsing past broken lines so one truncated record (crash
	// during append) cannot wipe the whole history.
	for _, line := range splitLines(b) {
		if len(line) == 0 {
			continue
		}
		var d Deployment
		if err := json.Unmarshal(line, &d); err != nil {
			s.log.Warn("skipping malformed deployment record", "err", err)
			continue
		}
		s.deployments[d.AppID] = append(s.deployments[d.AppID], &d)
	}
	for id := range s.deployments {
		list := s.deployments[id]
		sort.SliceStable(list, func(i, j int) bool { return list[i].StartedAt.After(list[j].StartedAt) })
		if len(list) > maxDeployHistory {
			s.deployments[id] = list[:maxDeployHistory]
		}
	}
	return nil
}

func (s *Store) saveAppsLocked() error {
	if err := os.MkdirAll(s.dir, 0755); err != nil {
		return err
	}
	list := make([]*Application, 0, len(s.apps))
	for _, a := range s.apps {
		list = append(list, a)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, fmt.Sprintf("%s.tmp.%d", appsFile, time.Now().UnixNano()))
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, appsFile))
}

// appendDeploymentLocked appends one JSONL record and prunes the in-memory
// window. On-disk pruning is best effort (rewrite the file); the tail can
// grow a bit until then, which is acceptable for a small-team deploy log.
func (s *Store) appendDeploymentLocked(d *Deployment) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.dir, deploymentsFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	list := append(s.deployments[d.AppID], d)
	sort.SliceStable(list, func(i, j int) bool { return list[i].StartedAt.After(list[j].StartedAt) })
	if len(list) > maxDeployHistory {
		list = list[:maxDeployHistory]
	}
	s.deployments[d.AppID] = list
	return nil
}

// ListApps returns all applications ordered by creation time.
// cloneApplication deep-copies an application so callers (deploy goroutines,
// the auto-healer, HTTP handlers) never share maps/slices with the live
// store object. This matters because the deploy engine mutates EnvVars
// in place (ResolveTemplate) while other goroutines marshal the same
// object — a shallow copy would risk "concurrent map iteration and map
// write" crashes.
func cloneApplication(a *Application) *Application {
	if a == nil {
		return nil
	}
	cp := *a
	if a.EnvVars != nil {
		cp.EnvVars = make(map[string]string, len(a.EnvVars))
		for k, v := range a.EnvVars {
			cp.EnvVars[k] = v
		}
	}
	if a.TemplateParams != nil {
		cp.TemplateParams = make(map[string]string, len(a.TemplateParams))
		for k, v := range a.TemplateParams {
			cp.TemplateParams[k] = v
		}
	}
	if a.Ports != nil {
		cp.Ports = append([]PortMapping(nil), a.Ports...)
	}
	if a.Volumes != nil {
		cp.Volumes = append([]string(nil), a.Volumes...)
	}
	return &cp
}

func (s *Store) ListApps() []*Application {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]*Application, 0, len(s.apps))
	for _, a := range s.apps {
		list = append(list, cloneApplication(a))
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	return list
}

// GetApp returns a deep-ish copy of one application.
func (s *Store) GetApp(id string) (*Application, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.apps[id]
	if !ok {
		return nil, false
	}
	return cloneApplication(a), true
}

// PutApp inserts or replaces one application and persists the snapshot.
func (s *Store) PutApp(a *Application) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.apps[a.ID] = a
	return s.saveAppsLocked()
}

// UpdateApp mutates one application under the write lock and persists.
func (s *Store) UpdateApp(id string, mutate func(*Application) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[id]
	if !ok {
		return fmt.Errorf("app %s not found", id)
	}
	if err := mutate(a); err != nil {
		return err
	}
	return s.saveAppsLocked()
}

// DeleteApp removes one application (its deployment history is kept for audit).
func (s *Store) DeleteApp(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.apps[id]; !ok {
		return fmt.Errorf("app %s not found", id)
	}
	delete(s.apps, id)
	return s.saveAppsLocked()
}

// FindAppByWebhookToken resolves the application bound to a webhook token.
func (s *Store) FindAppByWebhookToken(token string) (*Application, bool) {
	if token == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.apps {
		if a.WebhookToken == token {
			return cloneApplication(a), true
		}
	}
	return nil, false
}

// AddDeployment appends a deployment record (callers hold no lock).
func (s *Store) AddDeployment(d *Deployment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendDeploymentLocked(d)
}

// UpdateDeployment mutates one deployment record by id.
func (s *Store) UpdateDeployment(appID, depID string, mutate func(*Deployment)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.deployments[appID] {
		if d.ID == depID {
			mutate(d)
			return s.rewriteDeploymentsLocked()
		}
	}
	return fmt.Errorf("deployment %s not found", depID)
}

// rewriteDeploymentsLocked rewrites the JSONL tail after a record mutation.
func (s *Store) rewriteDeploymentsLocked() error {
	var all []*Deployment
	ids := make([]string, 0, len(s.deployments))
	for id := range s.deployments {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		all = append(all, s.deployments[id]...)
	}
	var buf []byte
	for _, d := range all {
		b, err := json.Marshal(d)
		if err != nil {
			return err
		}
		buf = append(buf, b...)
		buf = append(buf, '\n')
	}
	tmp := filepath.Join(s.dir, fmt.Sprintf("%s.tmp.%d", deploymentsFile, time.Now().UnixNano()))
	if err := os.WriteFile(tmp, buf, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, deploymentsFile))
}

// ListDeployments returns deployment history for one app, newest first.
// offset/pageSize implement server-side pagination; total is the unpaged
// count (AGENTS.md 8.2).
func (s *Store) ListDeployments(appID string, offset, pageSize int) ([]*Deployment, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := s.deployments[appID]
	total := len(list)
	if offset >= total {
		return nil, total
	}
	end := total
	if pageSize > 0 && offset+pageSize < total {
		end = offset + pageSize
	}
	out := make([]*Deployment, 0, end-offset)
	for _, d := range list[offset:end] {
		cp := *d
		out = append(out, &cp)
	}
	return out, total
}

// GetDeployment returns one deployment record.
func (s *Store) GetDeployment(appID, depID string) (*Deployment, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, d := range s.deployments[appID] {
		if d.ID == depID {
			cp := *d
			return &cp, true
		}
	}
	return nil, false
}

// ListSuccessfulDeployments returns the successful history (newest first)
// used to resolve rollback targets.
func (s *Store) ListSuccessfulDeployments(appID string) []*Deployment {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Deployment
	for _, d := range s.deployments[appID] {
		if d.Status == DeploySuccess {
			cp := *d
			out = append(out, &cp)
		}
	}
	return out
}
