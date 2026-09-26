package api

// UpgradeSigner signs upgrade payloads when set. The signed payload is
// "<version>\n<sha256>" using Ed25519; agents pinning an upgrade public
// key verify it before installing. Nil means upgrades are sent unsigned
// (sha256-only verification on the agent).
var UpgradeSigner func(version, sha256 string) []byte

// signUpgrade returns the signature for an upgrade, or nil when no signer
// is configured.
func signUpgrade(version, sha256 string) []byte {
	if UpgradeSigner == nil {
		return nil
	}
	return UpgradeSigner(version, sha256)
}
