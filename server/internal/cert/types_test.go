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
