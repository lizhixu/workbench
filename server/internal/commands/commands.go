// Package commands implements the saved-command library ("自定义常用命令").
// Entries are user-authored shell snippets that can be recalled when pushing
// commands to hosts or typing in the web terminal.
//
// Persistence follows the same pattern as the other server stores: a single
// JSON file under the data directory, guarded by a mutex, written atomically
// enough for a self-hosted single-node deployment.
package commands

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

// Shell enumerates the shells a saved command can target.
const (
	ShellBash       = "bash"
	ShellSh         = "sh"
	ShellCmd        = "cmd"
	ShellPowerShell = "powershell"
)

// Entry is one saved command in the library.
type Entry struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Shell       string    `json:"shell"`
	Content     string    `json:"content"`
	Description string    `json:"description"`
	Tags        []string  `json:"tags,omitempty"`
	// Owner is the username that created the entry. Empty means "shared" —
	// a built-in or admin-published command visible to everyone.
	Owner     string    `json:"owner,omitempty"`
	Shared    bool      `json:"shared"`
	UseCount  int       `json:"use_count"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store persists the command library.
type Store struct {
	mu      sync.Mutex
	dataDir string
	log     *slog.Logger
	entries []Entry
}

// builtins seed a fresh install so the library is useful before the operator
// saves anything. They are shared and owned by nobody.
var builtins = []Entry{
	{
		Name:        "查看磁盘占用",
		Shell:       ShellBash,
		Content:     "df -h",
		Description: "列出各挂载点容量与使用率",
		Tags:        []string{"磁盘"},
	},
	{
		Name:        "查看内存占用",
		Shell:       ShellBash,
		Content:     "free -h",
		Description: "内存与 swap 使用概况",
		Tags:        []string{"内存"},
	},
	{
		Name:        "CPU 占用前十进程",
		Shell:       ShellBash,
		Content:     "ps -eo pid,ppid,user,%cpu,%mem,comm --sort=-%cpu | head -n 11",
		Description: "按 CPU 排序的进程清单",
		Tags:        []string{"进程"},
	},
	{
		Name:        "监听端口清单",
		Shell:       ShellBash,
		Content:     "ss -tulnp",
		Description: "TCP/UDP 监听端口与所属进程",
		Tags:        []string{"网络"},
	},
	{
		Name:        "系统负载与运行时长",
		Shell:       ShellBash,
		Content:     "uptime",
		Description: "1/5/15 分钟负载与开机时长",
		Tags:        []string{"负载"},
	},
	{
		Name:        "Docker 容器状态",
		Shell:       ShellBash,
		Content:     "docker ps -a",
		Description: "全部容器（含已退出）状态",
		Tags:        []string{"docker"},
	},
	{
		Name:        "查看磁盘占用 (Windows)",
		Shell:       ShellPowerShell,
		Content:     "Get-PSDrive -PSProvider FileSystem | Select-Object Name,Used,Free",
		Description: "各盘符已用与剩余空间",
		Tags:        []string{"磁盘", "windows"},
	},
	{
		Name:        "CPU 占用前十进程 (Windows)",
		Shell:       ShellPowerShell,
		Content:     "Get-Process | Sort-Object CPU -Descending | Select-Object -First 10 Id,ProcessName,CPU,WS",
		Description: "按 CPU 排序的进程清单",
		Tags:        []string{"进程", "windows"},
	},
}

// NewStore loads the library from disk, seeding builtins on first run.
func NewStore(dataDir string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{dataDir: dataDir, log: log}
	if err := s.load(); err != nil {
		return nil, err
	}
	if len(s.entries) == 0 {
		now := time.Now()
		for i := range builtins {
			e := builtins[i]
			e.ID = newID()
			e.Shared = true
			e.CreatedAt = now
			e.UpdatedAt = now
			s.entries = append(s.entries, e)
		}
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) path() string {
	return filepath.Join(s.dataDir, "commands.json")
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
		return fmt.Errorf("read commands.json: %w", err)
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("parse commands.json: %w", err)
	}
	s.entries = entries
	return nil
}

func (s *Store) save() error {
	if s.dataDir == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path(), data, 0o600)
}

// List returns entries visible to username: shared ones plus the user's own.
// shell filters by target shell when non-empty; q does a case-insensitive
// substring match against name, content, description and tags.
func (s *Store) List(username, shell, q string) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	needle := strings.ToLower(strings.TrimSpace(q))
	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		if !e.Shared && e.Owner != "" && e.Owner != username {
			continue
		}
		if shell != "" && e.Shell != shell {
			continue
		}
		if needle != "" && !matches(e, needle) {
			continue
		}
		out = append(out, e)
	}
	// Most-used first, then newest, so the list self-organises with usage.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].UseCount != out[j].UseCount {
			return out[i].UseCount > out[j].UseCount
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

func matches(e Entry, needle string) bool {
	if strings.Contains(strings.ToLower(e.Name), needle) ||
		strings.Contains(strings.ToLower(e.Content), needle) ||
		strings.Contains(strings.ToLower(e.Description), needle) {
		return true
	}
	for _, t := range e.Tags {
		if strings.Contains(strings.ToLower(t), needle) {
			return true
		}
	}
	return false
}

// Get returns one entry by ID.
func (s *Store) Get(id string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// Create adds a new entry owned by username.
func (s *Store) Create(e Entry, username string) (Entry, error) {
	if err := validate(&e); err != nil {
		return Entry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	e.ID = newID()
	e.Owner = username
	e.UseCount = 0
	e.CreatedAt = now
	e.UpdatedAt = now
	s.entries = append(s.entries, e)
	if err := s.save(); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// Update modifies an existing entry. Only the owner or an admin may edit a
// private entry; isAdmin also permits editing shared/built-in entries.
func (s *Store) Update(id string, in Entry, username string, isAdmin bool) (Entry, error) {
	if err := validate(&in); err != nil {
		return Entry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.entries {
		if s.entries[i].ID != id {
			continue
		}
		if !canMutate(s.entries[i], username, isAdmin) {
			return Entry{}, errors.New("无权修改该命令")
		}
		s.entries[i].Name = in.Name
		s.entries[i].Shell = in.Shell
		s.entries[i].Content = in.Content
		s.entries[i].Description = in.Description
		s.entries[i].Tags = in.Tags
		if isAdmin {
			s.entries[i].Shared = in.Shared
		}
		s.entries[i].UpdatedAt = time.Now()
		if err := s.save(); err != nil {
			return Entry{}, err
		}
		return s.entries[i], nil
	}
	return Entry{}, errors.New("命令不存在")
}

// Delete removes an entry subject to the same ownership rules as Update.
func (s *Store) Delete(id, username string, isAdmin bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.entries {
		if s.entries[i].ID != id {
			continue
		}
		if !canMutate(s.entries[i], username, isAdmin) {
			return errors.New("无权删除该命令")
		}
		s.entries = append(s.entries[:i], s.entries[i+1:]...)
		return s.save()
	}
	return errors.New("命令不存在")
}

// MarkUsed bumps the usage counter so hot commands float to the top.
func (s *Store) MarkUsed(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.entries {
		if s.entries[i].ID == id {
			s.entries[i].UseCount++
			_ = s.save()
			return
		}
	}
}

func canMutate(e Entry, username string, isAdmin bool) bool {
	if isAdmin {
		return true
	}
	return e.Owner != "" && e.Owner == username
}

func validate(e *Entry) error {
	e.Name = strings.TrimSpace(e.Name)
	e.Content = strings.TrimSpace(e.Content)
	e.Description = strings.TrimSpace(e.Description)
	if e.Name == "" {
		return errors.New("命令名称不能为空")
	}
	if e.Content == "" {
		return errors.New("命令内容不能为空")
	}
	switch e.Shell {
	case ShellBash, ShellSh, ShellCmd, ShellPowerShell:
	case "":
		e.Shell = ShellBash
	default:
		return fmt.Errorf("不支持的 shell 类型: %s", e.Shell)
	}
	return nil
}

func newID() string {
	return fmt.Sprintf("cmd-%d", time.Now().UnixNano())
}
