package apps

import (
	"testing"

	"watchman/server/internal/vault"
)

func TestResolveVaultRef(t *testing.T) {
	s, err := vault.NewStore(t.TempDir(), "test-pass")
	if err != nil {
		t.Fatal(err)
	}
	cred, err := s.Create(&vault.Credential{
		Name: "db", Type: "database", Username: "root", Secret: "s3cr3t",
	})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct{ in, want string }{
		{"{{ vault:" + cred.ID + ":secret }}", "s3cr3t"},
		{"{{ vault:" + cred.ID + ":username }}", "root"},
		// Unknown field must keep the placeholder instead of injecting the
		// secret into a variable the operator did not ask for.
		{"{{ vault:" + cred.ID + ":password }}", "{{ vault:" + cred.ID + ":password }}"},
		// Unknown credential keeps the placeholder.
		{"{{ vault:missing:secret }}", "{{ vault:missing:secret }}"},
		// Non-ref values pass through untouched.
		{"plain-value", "plain-value"},
	}
	for _, tc := range cases {
		if got := resolveVaultRef(s, tc.in); got != tc.want {
			t.Errorf("resolveVaultRef(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
