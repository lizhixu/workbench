// Package vault implements an encrypted credential store for the control
// server. Credentials (SSH passwords, database passwords, API keys) are stored
// as AES-256-GCM encrypted JSON on disk. The encryption key is derived from a
// server-provided passphrase via scrypt.
//
// This is a local secrets manager — for production deployments with multiple
// server replicas, use an external KMS (HashiCorp Vault, AWS KMS, etc.) and
// wire it in here.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/scrypt"
)

// Credential is a stored secret entry.
type Credential struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Type        string    `json:"type"` // ssh_password / api_key / database / custom
	Username    string    `json:"username"`
	Secret      string    `json:"secret"` // plaintext, only in memory; encrypted on disk
	Host        string    `json:"host"`   // optional: associated host
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// onDiskEntry is the persisted form: the secret is encrypted.
type onDiskEntry struct {
	Credential
	EncryptedSecret []byte `json:"encrypted_secret"`
	Nonce           []byte `json:"nonce"`
}

// Store manages encrypted credentials.
type Store struct {
	mu       sync.RWMutex
	creds    map[string]*Credential
	filePath string
	gcm      cipher.AEAD
}

// NewStore creates or loads a credential vault. passphrase is used to derive
// the encryption key; if empty, a random key is generated (credentials won't
// survive restart).
func NewStore(dataDir, passphrase string) (*Store, error) {
	if dataDir == "" {
		dataDir = filepath.Join(os.TempDir(), "watchman")
	}
	s := &Store{
		creds:    map[string]*Credential{},
		filePath: filepath.Join(dataDir, "vault.json"),
	}
	key, err := deriveKey(passphrase)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	s.gcm = gcm
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func deriveKey(passphrase string) ([]byte, error) {
	if passphrase == "" {
		k := make([]byte, 32)
		_, _ = rand.Read(k)
		return k, nil
	}
	// Use a fixed salt for deterministic key derivation (the passphrase is
	// the secret; the salt prevents rainbow tables). In a real system the
	// salt would be stored alongside the vault.
	return scrypt.Key([]byte(passphrase), []byte("watchman-vault-salt-v1"), 32768, 8, 1, 32)
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var entries []onDiskEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return err
	}
	s.mu.Lock()
	for _, e := range entries {
		// Decrypt the secret.
		if len(e.EncryptedSecret) > 0 && len(e.Nonce) > 0 {
			plain, err := s.gcm.Open(nil, e.Nonce, e.EncryptedSecret, nil)
			if err != nil {
				// Skip undecryptable entries (wrong key).
				continue
			}
			e.Secret = string(plain)
		}
		c := e.Credential
		s.creds[c.ID] = &c
	}
	s.mu.Unlock()
	return nil
}

func (s *Store) persist() error {
	s.mu.RLock()
	entries := make([]onDiskEntry, 0, len(s.creds))
	for _, c := range s.creds {
		nonce := make([]byte, s.gcm.NonceSize())
		if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
			s.mu.RUnlock()
			return err
		}
		enc := s.gcm.Seal(nil, nonce, []byte(c.Secret), nil)
		// Clone the credential and clear the plaintext secret so it's not
		// serialized to disk alongside the encrypted blob.
		clone := *c
		clone.Secret = ""
		entries = append(entries, onDiskEntry{
			Credential:      clone,
			EncryptedSecret: enc,
			Nonce:           nonce,
		})
	}
	s.mu.RUnlock()
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.filePath, data, 0o600)
}

// List returns all credentials (without secrets).
func (s *Store) List() []*Credential {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Credential, 0, len(s.creds))
	for _, c := range s.creds {
		clone := *c
		clone.Secret = "" // never expose in list
		out = append(out, &clone)
	}
	return out
}

// Get returns a credential including the plaintext secret.
func (s *Store) Get(id string) (*Credential, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.creds[id]
	if !ok {
		return nil, false
	}
	clone := *c
	return &clone, true
}

// Create adds a new credential.
func (s *Store) Create(c *Credential) (*Credential, error) {
	if c.Name == "" || c.Secret == "" {
		return nil, errors.New("name and secret required")
	}
	c.ID = randomID()
	c.CreatedAt = time.Now()
	c.UpdatedAt = time.Now()
	s.mu.Lock()
	s.creds[c.ID] = c
	s.mu.Unlock()
	if err := s.persist(); err != nil {
		return nil, err
	}
	clone := *c
	clone.Secret = ""
	return &clone, nil
}

// Update modifies an existing credential.
func (s *Store) Update(id string, c *Credential) error {
	s.mu.Lock()
	existing, ok := s.creds[id]
	if !ok {
		s.mu.Unlock()
		return errors.New("credential not found")
	}
	if c.Name != "" {
		existing.Name = c.Name
	}
	if c.Type != "" {
		existing.Type = c.Type
	}
	if c.Username != "" {
		existing.Username = c.Username
	}
	if c.Secret != "" {
		existing.Secret = c.Secret
	}
	if c.Host != "" {
		existing.Host = c.Host
	}
	if c.Description != "" {
		existing.Description = c.Description
	}
	existing.UpdatedAt = time.Now()
	s.mu.Unlock()
	return s.persist()
}

// Delete removes a credential.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.creds[id]; !ok {
		return errors.New("credential not found")
	}
	delete(s.creds, id)
	return s.persist()
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}