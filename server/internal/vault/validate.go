package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"fmt"
)

// ValidateSecrets verifies every encrypted entry without exposing plaintext.
// Restore must fail closed on a wrong migration key instead of skipping rows.
func ValidateSecrets(data []byte, passphrase string) error {
	var entries []onDiskEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("凭据文件格式错误")
	}
	key, err := deriveKey(passphrase)
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if len(e.EncryptedSecret) == 0 && len(e.Nonce) == 0 {
			continue
		}
		if len(e.Nonce) != gcm.NonceSize() {
			return fmt.Errorf("凭据密文格式错误")
		}
		if _, err := gcm.Open(nil, e.Nonce, e.EncryptedSecret, nil); err != nil {
			return fmt.Errorf("Vault 密钥不匹配，无法解密归档凭据")
		}
	}
	return nil
}
