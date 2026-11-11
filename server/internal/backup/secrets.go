package backup

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"watchman/server/internal/vault"
)

const secretsFile = "server-secrets.json"

// ServerSecrets never appears in an API response. Archives are admin-only and
// carry these keys to preserve vault encryption and pinned upgrade signatures.
type ServerSecrets struct {
	JWTKey    string `json:"jwt_key"`
	VaultPass string `json:"vault_pass"`
}

func EnsureServerSecrets(dataDir, jwtKey, vaultPass string) (ServerSecrets, error) {
	p := filepath.Join(dataDir, secretsFile)
	var keys ServerSecrets
	if raw, err := os.ReadFile(p); err == nil {
		if err := json.Unmarshal(raw, &keys); err != nil || keys.JWTKey == "" || keys.VaultPass == "" {
			return keys, fmt.Errorf("控制端密钥文件无效")
		}
		if raw, err := os.ReadFile(filepath.Join(dataDir, "vault.json")); err == nil {
			if err := vault.ValidateSecrets(raw, keys.VaultPass); err != nil {
				return keys, err
			}
		} else if !os.IsNotExist(err) {
			return keys, err
		}
		return keys, os.Chmod(p, 0600)
	} else if !os.IsNotExist(err) {
		return keys, err
	}
	keys = ServerSecrets{JWTKey: jwtKey, VaultPass: vaultPass}
	for _, key := range []*string{&keys.JWTKey, &keys.VaultPass} {
		if *key == "" {
			b := make([]byte, 32)
			if _, err := rand.Read(b); err != nil {
				return keys, err
			}
			*key = hex.EncodeToString(b)
		}
	}
	// Never establish a new identity over an existing vault it cannot read.
	if raw, err := os.ReadFile(filepath.Join(dataDir, "vault.json")); err == nil {
		if err := vault.ValidateSecrets(raw, keys.VaultPass); err != nil {
			return keys, fmt.Errorf("无法解密现有凭据，请提供原 Vault 密钥: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return keys, err
	}
	return keys, atomicJSON(p, keys)
}

func atomicJSON(p string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}
