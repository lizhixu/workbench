// Package groups implements host grouping and per-group user authorization
// ("分组与权限"). A group is a named bucket of hosts; a grant binds a user to
// a group so that user may see and operate the hosts in it.
//
// Host membership is stored on the host record itself (rpc.Agent.Group holds
// the group name), so this package owns only the group definitions and the
// user grants. That keeps a single source of truth for "which group is this
// host in" and avoids a second membership table drifting out of sync.
//
// Authorization model:
//   - admin always has access to every group.
//   - a user with NO grants at all has access to everything. This keeps
//     single-operator deployments working; scoping only kicks in once an
//     admin actually assigns a user to groups.
//   - otherwise the user may only reach hosts whose group is granted.
package groups

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// GrantRole is the level of access a user has within a group.
type GrantRole string

const (
	// GrantOperate allows terminal / file / exec operations on the group's hosts.
	GrantOperate GrantRole = "operate"
	// GrantView allows read-only access (metrics, sysinfo, sessions).
	GrantView GrantRole = "view"
)

// Group is a named host bucket.
type Group struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	ParentID    string    `json:"parent_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Grant binds a user to a group with a role.
type Grant struct {
	Username  string    `json:"username"`
	GroupID   string    `json:"group_id"`
	Role      GrantRole `json:"role"`
	GrantedAt time.Time `json:"granted_at"`
	GrantedBy string    `json:"granted_by,omitempty"`
}

type persisted struct {
	Groups []Group `json:"groups"`
	Grants []Grant `json:"grants"`
}

// Store persists groups and grants.
type Store struct {
	mu      sync.Mutex
	dataDir string
	log     *slog.Logger
	groups  []Group
	grants  []Grant
}

// NewStore loads groups and grants from disk.
func NewStore(dataDir string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{dataDir: dataDir, log: log}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) path() string {
	return filepath.Join(s.dataDir, "groups.json")
}

func (s *Store) load() error {
	if s.dataDir == "" {
		return nil
	}
	data, err := os.ReadFile(s.path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read groups.json: %w", err)
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("parse groups.json: %w", err)
	}
	s.groups = p.Groups
	s.grants = p.Grants
	return nil
}

func (s *Store) save() error {
	if s.dataDir == "" {
		return nil
	}
	data, err := json.MarshalIndent(persisted{Groups: s.groups, Grants: s.grants}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path(), data, 0o600)
}

// ---- Group CRUD ----

// List returns all groups sorted by name.
func (s *Store) List() []Group {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Group, len(s.groups))
	copy(out, s.groups)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns a group by ID.
func (s *Store) Get(id string) (Group, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.groups {
		if g.ID == id {
			return g, true
		}
	}
	return Group{}, false
}

// Create adds a group. Names must be unique.
func (s *Store) Create(g Group) (Group, error) {
	g.Name = strings.TrimSpace(g.Name)
	g.Description = strings.TrimSpace(g.Description)
	if g.Name == "" {
		return Group{}, errors.New("分组名称不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.groups {
		if x.Name == g.Name {
			return Group{}, errors.New("分组名称已存在")
		}
	}
	if g.ParentID != "" && !s.hasGroupLocked(g.ParentID) {
		return Group{}, errors.New("父分组不存在")
	}
	now := time.Now()
	g.ID = fmt.Sprintf("grp-%d", now.UnixNano())
	g.CreatedAt = now
	g.UpdatedAt = now
	s.groups = append(s.groups, g)
	if err := s.save(); err != nil {
		return Group{}, err
	}
	return g, nil
}

// Update renames or re-describes a group. It returns the previous name so the
// caller can migrate host records that reference the group by name.
func (s *Store) Update(id string, in Group) (oldName string, updated Group, err error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
	if in.Name == "" {
		return "", Group{}, errors.New("分组名称不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.groups {
		if s.groups[i].ID != id {
			continue
		}
		for _, x := range s.groups {
			if x.ID != id && x.Name == in.Name {
				return "", Group{}, errors.New("分组名称已存在")
			}
		}
		if in.ParentID != "" {
			if in.ParentID == id {
				return "", Group{}, errors.New("父分组不能是自身")
			}
			if !s.hasGroupLocked(in.ParentID) {
				return "", Group{}, errors.New("父分组不存在")
			}
		}
		oldName = s.groups[i].Name
		s.groups[i].Name = in.Name
		s.groups[i].Description = in.Description
		s.groups[i].ParentID = in.ParentID
		s.groups[i].UpdatedAt = time.Now()
		if err := s.save(); err != nil {
			return "", Group{}, err
		}
		return oldName, s.groups[i], nil
	}
	return "", Group{}, errors.New("分组不存在")
}

// Delete removes a group along with its grants. It returns the deleted
// group's name so the caller can clear it off host records.
func (s *Store) Delete(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.groups {
		if g.ParentID == id {
			return "", errors.New("请先删除或移动子分组")
		}
	}
	for i := range s.groups {
		if s.groups[i].ID != id {
			continue
		}
		name := s.groups[i].Name
		s.groups = append(s.groups[:i], s.groups[i+1:]...)
		kept := s.grants[:0]
		for _, gr := range s.grants {
			if gr.GroupID != id {
				kept = append(kept, gr)
			}
		}
		s.grants = kept
		if err := s.save(); err != nil {
			return "", err
		}
		return name, nil
	}
	return "", errors.New("分组不存在")
}

func (s *Store) hasGroupLocked(id string) bool {
	for _, g := range s.groups {
		if g.ID == id {
			return true
		}
	}
	return false
}

// ---- Grants ----

// ListGrants returns grants, optionally filtered by group and/or username.
func (s *Store) ListGrants(groupID, username string) []Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Grant, 0, len(s.grants))
	for _, g := range s.grants {
		if groupID != "" && g.GroupID != groupID {
			continue
		}
		if username != "" && g.Username != username {
			continue
		}
		out = append(out, g)
	}
	return out
}

// SetGrant creates or updates a user's grant on a group.
func (s *Store) SetGrant(groupID, username string, role GrantRole, grantedBy string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("用户名不能为空")
	}
	switch role {
	case GrantOperate, GrantView:
	case "":
		role = GrantOperate
	default:
		return fmt.Errorf("不支持的授权级别: %s", role)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasGroupLocked(groupID) {
		return errors.New("分组不存在")
	}
	for i := range s.grants {
		if s.grants[i].GroupID == groupID && s.grants[i].Username == username {
			s.grants[i].Role = role
			s.grants[i].GrantedAt = time.Now()
			s.grants[i].GrantedBy = grantedBy
			return s.save()
		}
	}
	s.grants = append(s.grants, Grant{
		Username:  username,
		GroupID:   groupID,
		Role:      role,
		GrantedAt: time.Now(),
		GrantedBy: grantedBy,
	})
	return s.save()
}

// RevokeGrant removes a user's grant on a group.
func (s *Store) RevokeGrant(groupID, username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.grants {
		if s.grants[i].GroupID == groupID && s.grants[i].Username == username {
			s.grants = append(s.grants[:i], s.grants[i+1:]...)
			return s.save()
		}
	}
	return errors.New("授权不存在")
}

// RevokeUser drops every grant belonging to a user (called when the user is
// deleted so stale grants don't accumulate).
func (s *Store) RevokeUser(username string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.grants[:0]
	changed := false
	for _, g := range s.grants {
		if g.Username == username {
			changed = true
			continue
		}
		kept = append(kept, g)
	}
	s.grants = kept
	if changed {
		_ = s.save()
	}
}

// ---- Authorization ----

// AllowedGroupNames returns the group names a user may reach. The second
// return value is false when the user is unrestricted (admin, or a user with
// no grants at all), in which case the name set is meaningless.
func (s *Store) AllowedGroupNames(username string, isAdmin bool) (map[string]bool, bool) {
	if isAdmin {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Does this user have any grant at all?
	granted := make(map[string]bool)
	any := false
	for _, gr := range s.grants {
		if gr.Username != username {
			continue
		}
		any = true
		for _, g := range s.groups {
			if g.ID == gr.GroupID {
				granted[g.Name] = true
				break
			}
		}
	}
	if !any {
		return nil, false
	}
	return granted, true
}

// CanAccessGroup reports whether the user may reach hosts in groupName.
func (s *Store) CanAccessGroup(username string, isAdmin bool, groupName string) bool {
	allowed, restricted := s.AllowedGroupNames(username, isAdmin)
	if !restricted {
		return true
	}
	return allowed[groupName]
}
