package api

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// UpgradeSigner signs upgrade payloads when set. The signed payload is
// "<version>\n<sha256>" using Ed25519; agents pinning an upgrade public
// key verify it before installing. Nil means upgrades are sent unsigned
// (sha256-only verification on the agent).
var UpgradeSigner func(version, sha256 string) []byte

// UpgradePubKeyHex is the hex-encoded Ed25519 public key matching
// UpgradeSigner, exposed via GET /api/v1/version so operators can pin
// it on agents (-upgrade-pubkey). Empty when no signer is configured.
var UpgradePubKeyHex string

// signUpgrade returns the signature for an upgrade, or nil when no signer
// is configured.
func signUpgrade(version, sha256 string) []byte {
	if UpgradeSigner == nil {
		return nil
	}
	return UpgradeSigner(version, sha256)
}

// InitSigner derives a persistent Ed25519 signing key from the vault
// passphrase and installs it as the upgrade signer.
//
// Derivation (not a stored key) means no new secret to manage and no
// migration: the same vault passphrase always yields the same keypair
// across restarts, so agents pinning the public key keep verifying.
//
// Agents without a pinned key (-upgrade-pubkey empty) skip signature
// verification, so enabling the signer never breaks existing agents;
// it only starts producing signatures they ignore.
func InitSigner(vaultPass string) (string, error) {
	if vaultPass == "" {
		return "", fmt.Errorf("vault passphrase is empty, cannot derive upgrade signing key")
	}
	// Domain-separated SHA-256 KDF. The vault passphrase is 256 bits of
	// CSPRNG output (openssl rand -hex 32), so a single hash with domain
	// separation is a sound KDF here; no stretching needed.
	sum := sha256.Sum256([]byte("watchman-upgrade-sign-v1\x00" + vaultPass))
	priv := ed25519.NewKeyFromSeed(sum[:])
	pub := priv.Public().(ed25519.PublicKey)
	UpgradeSigner = func(version, sha256 string) []byte {
		return ed25519.Sign(priv, []byte(version+"\n"+sha256))
	}
	UpgradePubKeyHex = hex.EncodeToString(pub)
	return UpgradePubKeyHex, nil
}
