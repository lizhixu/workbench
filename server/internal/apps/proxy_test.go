package apps

import "testing"

// TestValidProxyDomain: the domain is interpolated verbatim into a root-
// written nginx server block, so anything outside strict hostname shape
// must be rejected.
func TestValidProxyDomain(t *testing.T) {
	valid := []string{
		"example.com",
		"api.example.com",
		"a-b.example.com",
		"app1.internal",
		"localhost",
		"x.co",
	}
	for _, d := range valid {
		if !validProxyDomain(d) {
			t.Errorf("validProxyDomain(%q) = false, want true", d)
		}
	}
	invalid := []string{
		"",
		"example.com; rm -rf /",
		"example.com\nserver_name evil",
		"exa mple.com",
		"example.com}",
		"${host}.example.com",
		"-lead.example.com",
		"trail-.example.com",
		"double..dot.com",
		".leading.com",
		"trailing.com.",
		"under_score.com",
		"UPPER.com", // caller lowercases first; raw upper is rejected
	}
	for _, d := range invalid {
		if validProxyDomain(d) {
			t.Errorf("validProxyDomain(%q) = true, want false", d)
		}
	}
}
