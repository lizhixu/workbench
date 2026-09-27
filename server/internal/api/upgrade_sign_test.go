package api

import (
	"crypto/ed25519"
	"encoding/hex"
	"testing"
)

// InitSigner must derive the same keypair for the same passphrase
// (restart-stable), different keypairs for different passphrases, and
// produce signatures verifiable with the published public key.
func TestInitSignerDeterministic(t *testing.T) {
	oldSigner, oldPub := UpgradeSigner, UpgradePubKeyHex
	defer func() { UpgradeSigner, UpgradePubKeyHex = oldSigner, oldPub }()

	pub1, err := InitSigner("test-vault-passphrase")
	if err != nil {
		t.Fatalf("InitSigner: %v", err)
	}
	pub2, err := InitSigner("test-vault-passphrase")
	if err != nil {
		t.Fatalf("InitSigner: %v", err)
	}
	if pub1 != pub2 {
		t.Fatal("same passphrase must derive the same keypair")
	}
	pub3, err := InitSigner("different-passphrase")
	if err != nil {
		t.Fatalf("InitSigner: %v", err)
	}
	if pub1 == pub3 {
		t.Fatal("different passphrases must derive different keypairs")
	}

	// Signature round-trip against the published public key.
	// (Re-init: the "different passphrase" call above replaced the signer.)
	if _, err := InitSigner("test-vault-passphrase"); err != nil {
		t.Fatalf("InitSigner: %v", err)
	}
	sig := signUpgrade("v1.2.3", "abc123")
	if len(sig) == 0 {
		t.Fatal("expected a signature when signer is initialized")
	}
	pubRaw, err := hex.DecodeString(pub1)
	if err != nil {
		t.Fatalf("decode pubkey: %v", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pubRaw), []byte("v1.2.3\nabc123"), sig) {
		t.Fatal("signature does not verify with published public key")
	}
}

func TestInitSignerEmptyPass(t *testing.T) {
	if _, err := InitSigner(""); err == nil {
		t.Fatal("expected error for empty passphrase")
	}
}
