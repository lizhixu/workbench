package apps

import "testing"

// TestResolveGatewayUpstream: 异地组网是网关模式的其中一条路径而非前置条件——
// 双方都在组网内走 100.x，否则直连应用主机的公网 IP，再回退内网 IP；绝不用
// 127.0.0.1（会指向网关自身造成流量黑洞）。
func TestResolveGatewayUpstream(t *testing.T) {
	cases := []struct {
		name                       string
		appMesh, gwMesh            string
		appPublicIP, appInternalIP string
		want                       string
		wantErr                    bool
	}{
		{
			name:    "both in mesh prefers overlay",
			appMesh: "100.64.0.5", gwMesh: "100.64.0.1",
			appPublicIP: "1.2.3.4", appInternalIP: "10.0.0.5",
			want: "100.64.0.5:8080",
		},
		{
			name:    "app in mesh but gateway not falls back to direct public",
			appMesh: "100.64.0.5", gwMesh: "",
			appPublicIP: "1.2.3.4", appInternalIP: "10.0.0.5",
			want: "1.2.3.4:8080",
		},
		{
			name:    "neither in mesh uses public IP",
			appMesh: "", gwMesh: "",
			appPublicIP: "1.2.3.4", appInternalIP: "10.0.0.5",
			want: "1.2.3.4:8080",
		},
		{
			name:    "no public IP falls back to internal",
			appMesh: "", gwMesh: "",
			appPublicIP: "", appInternalIP: "10.0.0.5",
			want: "10.0.0.5:8080",
		},
		{
			name:    "nothing resolvable errors instead of 127.0.0.1",
			appMesh: "", gwMesh: "",
			appPublicIP: "", appInternalIP: "",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		got, err := resolveGatewayUpstream(8080, tc.appMesh, tc.gwMesh, tc.appPublicIP, tc.appInternalIP)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: expected error, got upstream %q", tc.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
		if got == "127.0.0.1:8080" {
			t.Errorf("%s: upstream must never be loopback", tc.name)
		}
	}
}

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
