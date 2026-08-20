// Package auth implements user management, password hashing, and JWT-based
// authentication for the control server.
//
// User accounts are persisted to a JSON file (users.json) under the server's
// data directory. A default admin account is bootstrapped on first run with
// username "admin" and password "admin" — the user is expected to change it
// after first login. Passwords are stored as bcrypt hashes.
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// Role enumerates the permission levels.
type Role string

const (
	RoleAdmin    Role = "admin"    // full access incl. user management
	RoleOperator Role = "operator" // operate hosts: terminal, files, exec
	RoleViewer   Role = "viewer"   // read-only: view hosts/metrics/sessions
)

// User is a stored account record.
type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"password_hash"`
	Role         Role      `json:"role"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Claims is the JWT payload.
type Claims struct {
	Username string `json:"username"`
	Role     Role   `json:"role"`
	jwt.RegisteredClaims
}

// Store manages users on disk and issues/validates JWT tokens.
type Store struct {
	mu       sync.RWMutex
	users    map[string]*User // keyed by username
	filePath string
	jwtKey   []byte
}

// NewStore loads users from disk (or bootstraps the default admin) and returns
// a ready store. jwtKey is used for signing tokens; if empty, a random key is
// generated (tokens won't survive restart, which is acceptable for MVP).
func NewStore(dataDir, jwtKey string) (*Store, error) {
	if dataDir == "" {
		dataDir = filepath.Join(os.TempDir(), "watchman")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	s := &Store{
		users:    map[string]*User{},
		filePath: filepath.Join(dataDir, "users.json"),
	}
	if jwtKey != "" {
		s.jwtKey = []byte(jwtKey)
	} else {
		k := make([]byte, 32)
		_, _ = rand.Read(k)
		s.jwtKey = k
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Bootstrap default admin (admin / admin).
			return s.bootstrap()
		}
		return fmt.Errorf("read users file: %w", err)
	}
	var users []*User
	if err := json.Unmarshal(data, &users); err != nil {
		return fmt.Errorf("parse users file: %w", err)
	}
	s.mu.Lock()
	for _, u := range users {
		s.users[u.Username] = u
	}
	s.mu.Unlock()
	if len(s.users) == 0 {
		return s.bootstrap()
	}
	return nil
}

func (s *Store) bootstrap() error {
	hash, _ := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.DefaultCost)
	u := &User{
		ID:           randomID(),
		Username:     "admin",
		PasswordHash: string(hash),
		Role:         RoleAdmin,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	s.mu.Lock()
	s.users[u.Username] = u
	s.mu.Unlock()
	return s.persist()
}

func (s *Store) persist() error {
	s.mu.RLock()
	users := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		users = append(users, u)
	}
	s.mu.RUnlock()
	data, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.filePath, data, 0o600)
}

// Authenticate validates credentials and returns a signed JWT.
func (s *Store) Authenticate(username, password string) (string, *User, error) {
	s.mu.RLock()
	u, ok := s.users[username]
	s.mu.RUnlock()
	if !ok {
		return "", nil, errors.New("invalid username or password")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return "", nil, errors.New("invalid username or password")
	}
	tok, err := s.issueToken(u)
	if err != nil {
		return "", nil, err
	}
	return tok, u, nil
}

func (s *Store) issueToken(u *User) (string, error) {
	claims := Claims{
		Username: u.Username,
		Role:     u.Role,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Subject:   u.ID,
		},
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return t.SignedString(s.jwtKey)
}

// Validate parses a token and returns the claims if valid.
func (s *Store) Validate(tokenStr string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.jwtKey, nil
	})
	if err != nil {
		return nil, err
	}
	return claims, nil
}

// List returns all users (without password hashes).
func (s *Store) List() []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		clone := *u
		clone.PasswordHash = ""
		out = append(out, &clone)
	}
	return out
}

// Get returns a user by username.
func (s *Store) Get(username string) (*User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[username]
	if !ok {
		return nil, false
	}
	clone := *u
	clone.PasswordHash = ""
	return &clone, true
}

// Create adds a new user. Returns error if username exists.
func (s *Store) Create(username, password string, role Role) (*User, error) {
	if username == "" || password == "" {
		return nil, errors.New("username and password required")
	}
	if role != RoleAdmin && role != RoleOperator && role != RoleViewer {
		return nil, errors.New("invalid role")
	}
	s.mu.Lock()
	if _, exists := s.users[username]; exists {
		s.mu.Unlock()
		return nil, errors.New("username already exists")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	u := &User{
		ID:           randomID(),
		Username:     username,
		PasswordHash: string(hash),
		Role:         role,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	s.users[username] = u
	s.mu.Unlock()
	if err := s.persist(); err != nil {
		return nil, err
	}
	clone := *u
	clone.PasswordHash = ""
	return &clone, nil
}

// UpdateRole changes a user's role.
func (s *Store) UpdateRole(username string, role Role) error {
	if role != RoleAdmin && role != RoleOperator && role != RoleViewer {
		return errors.New("invalid role")
	}
	s.mu.Lock()
	u, ok := s.users[username]
	if !ok {
		s.mu.Unlock()
		return errors.New("user not found")
	}
	u.Role = role
	u.UpdatedAt = time.Now()
	s.mu.Unlock()
	return s.persist()
}

// UpdatePassword sets a new password for a user.
func (s *Store) UpdatePassword(username, oldPassword, newPassword string) error {
	if newPassword == "" {
		return errors.New("new password required")
	}
	s.mu.Lock()
	u, ok := s.users[username]
	if !ok {
		s.mu.Unlock()
		return errors.New("user not found")
	}
	// Verify old password unless caller is admin resetting another user's
	// password (handled by ResetPassword).
	if oldPassword != "" {
		if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(oldPassword)); err != nil {
			s.mu.Unlock()
			return errors.New("old password incorrect")
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	u.PasswordHash = string(hash)
	u.UpdatedAt = time.Now()
	s.mu.Unlock()
	return s.persist()
}

// ResetPassword allows an admin to set another user's password without the old one.
func (s *Store) ResetPassword(username, newPassword string) error {
	return s.UpdatePassword(username, "", newPassword)
}

// Delete removes a user. The last admin cannot be deleted.
func (s *Store) Delete(username string) error {
	s.mu.Lock()
	u, ok := s.users[username]
	if !ok {
		s.mu.Unlock()
		return errors.New("user not found")
	}
	if u.Role == RoleAdmin {
		admins := 0
		for _, x := range s.users {
			if x.Role == RoleAdmin {
				admins++
			}
		}
		if admins <= 1 {
			s.mu.Unlock()
			return errors.New("cannot delete the last admin account")
		}
	}
	delete(s.users, username)
	s.mu.Unlock()
	return s.persist()
}

// HasPermission checks whether a role may perform an action.
func HasPermission(role Role, action string) bool {
	switch role {
	case RoleAdmin:
		return true
	case RoleOperator:
		switch action {
		case "manage_users", "delete_host", "view_sessions":
			return false
		default:
			return true
		}
	case RoleViewer:
		switch action {
		case "view_hosts", "view_metrics", "view_sessions", "view_sysinfo":
			return true
		default:
			return false
		}
	}
	return false
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}