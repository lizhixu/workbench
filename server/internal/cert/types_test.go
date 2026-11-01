package cert

import "testing"

// TestPresetDirectoryURLs pins the CA presets to their official ACME
// directories. Google Trust Services used to be shipped with a non-existent
// host (dv.acme.pki.goog): it resolves to a generic Google frontend that
// answers with its default *.google.com certificate, so the handshake fails
// with "x509: certificate is valid for *.google.com ... not dv.acme.pki.goog"
// before any ACME request is made.
func TestPresetDirectoryURLs(t *testing.T) {
	want := map[string]string{
		"letsencrypt":         "https://acme-v02.api.letsencrypt.org/directory",
		"letsencrypt_staging": "https://acme-staging-v02.api.letsencrypt.org/directory",
		"google":              "https://dv.acme-v02.api.pki.goog/directory",
		"google_staging":      "https://dv.acme-v02.test-api.pki.goog/directory",
	}
	for _, p := range Presets {
		if exp, ok := want[p.ID]; ok && p.DirectoryURL != exp {
			t.Errorf("preset %s directory_url = %q, want %q", p.ID, p.DirectoryURL, exp)
		}
		// requires_contact must mirror what the CA actually enforces, so the
		// UI can mark the email field required before the user hits "save".
		if pc := RequiresContact(p.DirectoryURL); pc != p.RequiresContact {
			t.Errorf("preset %s requires_contact = %v, but RequiresContact(directory) = %v", p.ID, p.RequiresContact, pc)
		}
	}
}

// TestNormalizeDirectoryURL: obsolete endpoints stored by earlier builds must
// be rewritten on load/save, while custom CA URLs pass through untouched.
func TestNormalizeDirectoryURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://dv.acme.pki.goog/directory", "https://dv.acme-v02.api.pki.goog/directory"},
		{"https://dv.acme.pki.goog/directory/", "https://dv.acme-v02.api.pki.goog/directory"},
		{" https://dv.acme.staging.pki.goog/directory ", "https://dv.acme-v02.test-api.pki.goog/directory"},
		{"https://dv.acme-v02.api.pki.goog/directory", "https://dv.acme-v02.api.pki.goog/directory"},
		{"https://acme-v02.api.letsencrypt.org/directory", "https://acme-v02.api.letsencrypt.org/directory"},
		{"https://ca.internal:9000/acme/dir", "https://ca.internal:9000/acme/dir"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeDirectoryURL(c.in); got != c.want {
			t.Errorf("NormalizeDirectoryURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRequiresContact pins the contact policy to the CA host: GTS answers
// newAccount without a contact field with 400 malformed ("Accounts must have
// at least one contact."), while Let's Encrypt and custom CAs accept it.
func TestRequiresContact(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://dv.acme-v02.api.pki.goog/directory", true},
		{"https://dv.acme-v02.test-api.pki.goog/directory", true},
		{"https://pki.goog/directory", true},
		{"https://acme-v02.api.letsencrypt.org/directory", false},
		{"https://acme.zerossl.com/v2/DV90", false},
		{"https://ca.internal:9000/acme/dir", false},
		{"", false},
	}
	for _, c := range cases {
		if got := RequiresContact(c.url); got != c.want {
			t.Errorf("RequiresContact(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

// TestValidateAccountContact covers the pre-flight guard: GTS must be told to
// fill an email, while Let's Encrypt keeps working with an empty contact.
func TestValidateAccountContact(t *testing.T) {
	const gts = "https://dv.acme-v02.api.pki.goog/directory"
	const le = "https://acme-v02.api.letsencrypt.org/directory"

	if err := ValidateAccountContact(gts, ""); err == nil {
		t.Errorf("GTS without email must be rejected")
	}
	if err := ValidateAccountContact(gts, "  "); err == nil {
		t.Errorf("GTS with whitespace-only email must be rejected")
	}
	if err := ValidateAccountContact(le, ""); err != nil {
		t.Errorf("Let's Encrypt without email must be allowed, got %v", err)
	}
	if err := ValidateAccountContact(gts, "ops@example.org"); err != nil {
		t.Errorf("GTS with a valid email must pass, got %v", err)
	}
	// Email domains are case-insensitive; publicsuffix matches literal labels,
	// so the check must lowercase before consulting the PSL.
	if err := ValidateAccountContact(gts, "OPS@EXAMPLE.ORG"); err != nil {
		t.Errorf("uppercase email domain must pass, got %v", err)
	}
	if err := ValidateAccountContact(gts, "not-an-email"); err == nil {
		t.Errorf("malformed email must be rejected")
	}
	if err := ValidateAccountContact(gts, "admin@watchman.local"); err == nil {
		t.Errorf("email domain without a public suffix must be rejected")
	}
}
