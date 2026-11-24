package sysinfo

import (
	"reflect"
	"testing"
)

func TestParseUfwStatus(t *testing.T) {
	out := `Status: active

To                         Action      From
--                         ------      ----
22/tcp                     ALLOW IN    Anywhere
22/tcp (v6)                ALLOW IN    Anywhere (v6)
8080/tcp                   DENY IN     Anywhere
8080/tcp (v6)              DENY IN     Anywhere (v6)
53/udp                     ALLOW IN    Anywhere
443/tcp                    ALLOW OUT   Anywhere
8443/tcp                   DENY OUT    Anywhere
`
	want := []FirewallRule{
		{Port: 22, Protocol: "tcp", Action: "allow"},
		{Port: 8080, Protocol: "tcp", Action: "deny"},
		{Port: 53, Protocol: "udp", Action: "allow"},
	}
	if got := parseUfwStatus(out); !reflect.DeepEqual(got, want) {
		t.Errorf("parseUfwStatus() = %+v, want %+v", got, want)
	}
	// Inactive firewall: no rules table, no rules.
	if got := parseUfwStatus("Status: inactive\n"); len(got) != 0 {
		t.Errorf("parseUfwStatus(inactive) = %+v, want empty", got)
	}
	// Outbound rules are out of scope for v1 and must not leak into the
	// inbound list (their direction would also break delete/conflict
	// handling, which only targets inbound rules).
	outOnly := "443/tcp                    ALLOW OUT   Anywhere\n8443/tcp                   DENY OUT    Anywhere\n"
	if got := parseUfwStatus(outOnly); len(got) != 0 {
		t.Errorf("parseUfwStatus(out-only) = %+v, want empty", got)
	}
}

func TestParseFirewalldPorts(t *testing.T) {
	want := []FirewallRule{
		{Port: 8080, Protocol: "tcp", Action: "allow"},
		{Port: 9090, Protocol: "tcp", Action: "allow"},
	}
	if got := parseFirewalldPorts("8080/tcp 9090/tcp"); !reflect.DeepEqual(got, want) {
		t.Errorf("parseFirewalldPorts() = %+v, want %+v", got, want)
	}
	if got := parseFirewalldPorts(""); len(got) != 0 {
		t.Errorf("parseFirewalldPorts(\"\") = %+v, want empty", got)
	}
}

func TestParseFirewalldRichRules(t *testing.T) {
	out := `rule family="ipv4" port port="8080" protocol="tcp" reject
rule family="ipv4" port port="53" protocol="udp" accept
rule family="ipv4" source address="10.0.0.0/8" accept
`
	want := []FirewallRule{
		{Port: 8080, Protocol: "tcp", Action: "deny"},
		{Port: 53, Protocol: "udp", Action: "allow"},
	}
	if got := parseFirewalldRichRules(out); !reflect.DeepEqual(got, want) {
		t.Errorf("parseFirewalldRichRules() = %+v, want %+v", got, want)
	}
}

func TestParseIptablesRules(t *testing.T) {
	out := `-P INPUT ACCEPT
-P FORWARD DROP
-A INPUT -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
-A INPUT -p tcp -m tcp --dport 22 -j ACCEPT
-A INPUT -p tcp -m tcp --dport 8080 -j DROP
-A INPUT -p udp -m udp --dport 53 -j REJECT
-A INPUT -p tcp -m tcp --dport 22 -j ACCEPT
`
	want := []FirewallRule{
		{Port: 22, Protocol: "tcp", Action: "allow"},
		{Port: 8080, Protocol: "tcp", Action: "deny"},
		{Port: 53, Protocol: "udp", Action: "deny"},
	}
	if got := parseIptablesRules(out); !reflect.DeepEqual(got, want) {
		t.Errorf("parseIptablesRules() = %+v, want %+v", got, want)
	}
}

func TestPickFirewallTargets(t *testing.T) {
	rules := []FirewallRule{
		{Port: 8080, Protocol: "tcp", Action: "deny"},
		{Port: 8080, Protocol: "tcp", Action: "allow"},
		{Port: 8080, Protocol: "udp", Action: "deny"},
		{Port: 9090, Protocol: "tcp", Action: "allow"},
	}
	// Requesting allow for 8080/tcp: the existing allow is same-action,
	// the deny is opposite and must be removed first.
	same, opposite := pickFirewallTargets(rules, 8080, "tcp", "allow")
	if len(same) != 1 || same[0].Action != "allow" {
		t.Errorf("same = %+v, want one allow rule", same)
	}
	if len(opposite) != 1 || opposite[0].Action != "deny" {
		t.Errorf("opposite = %+v, want one deny rule", opposite)
	}
	// No rule at all for 22/udp.
	same, opposite = pickFirewallTargets(rules, 22, "udp", "allow")
	if len(same) != 0 || len(opposite) != 0 {
		t.Errorf("empty pair = %+v %+v, want both empty", same, opposite)
	}
}

func TestSplitPortProto(t *testing.T) {
	if port, proto, ok := splitPortProto("8080/tcp"); !ok || port != 8080 || proto != "tcp" {
		t.Errorf("splitPortProto(8080/tcp) = %d %q %v", port, proto, ok)
	}
	for _, bad := range []string{"8080", "0/tcp", "65536/tcp", "80/sctp", "abc/tcp", "80/TCP"} {
		if _, _, ok := splitPortProto(bad); ok {
			t.Errorf("splitPortProto(%q) unexpectedly ok", bad)
		}
	}
}

func TestValidFirewallPort(t *testing.T) {
	for _, bad := range []int32{0, -1, 65536} {
		if err := validFirewallPort(bad); err == nil {
			t.Errorf("validFirewallPort(%d) unexpectedly nil", bad)
		}
	}
	if err := validFirewallPort(9090); err != nil {
		t.Errorf("validFirewallPort(9090) = %v (9090 is rejected server-side, not here)", err)
	}
}

func TestValidFirewallProtocol(t *testing.T) {
	if p, err := validFirewallProtocol(""); err != nil || p != "tcp" {
		t.Errorf("validFirewallProtocol(\"\") = %q, %v", p, err)
	}
	if p, err := validFirewallProtocol("UDP"); err != nil || p != "udp" {
		t.Errorf("validFirewallProtocol(UDP) = %q, %v", p, err)
	}
	if _, err := validFirewallProtocol("sctp"); err == nil {
		t.Error("validFirewallProtocol(sctp) unexpectedly nil")
	}
}
