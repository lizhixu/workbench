package api

import "testing"

// validFirewallRule 是防火墙写接口的服务端校验：端口范围、协议白名单，
// 9090（Agent 回连端口）硬拒；空协议默认 tcp。
func TestValidFirewallRule(t *testing.T) {
	if port, proto, err := validFirewallRule(firewallRuleBody{Port: 8080, Protocol: "tcp"}); err != nil || port != 8080 || proto != "tcp" {
		t.Errorf("8080/tcp 应通过，实际 port=%d proto=%q err=%v", port, proto, err)
	}
	if _, proto, err := validFirewallRule(firewallRuleBody{Port: 53, Protocol: ""}); err != nil || proto != "tcp" {
		t.Errorf("空协议应默认 tcp，实际 proto=%q err=%v", proto, err)
	}
	if _, proto, err := validFirewallRule(firewallRuleBody{Port: 53, Protocol: "UDP"}); err != nil || proto != "udp" {
		t.Errorf("UDP 应归一化为 udp，实际 proto=%q err=%v", proto, err)
	}
	cases := []struct {
		name string
		body firewallRuleBody
	}{
		{"端口 0", firewallRuleBody{Port: 0, Protocol: "tcp"}},
		{"端口 65536", firewallRuleBody{Port: 65536, Protocol: "tcp"}},
		{"端口负数", firewallRuleBody{Port: -1, Protocol: "tcp"}},
		{"9090 硬拒", firewallRuleBody{Port: 9090, Protocol: "tcp"}},
		{"9090 udp 同样硬拒", firewallRuleBody{Port: 9090, Protocol: "udp"}},
		{"未知协议", firewallRuleBody{Port: 80, Protocol: "sctp"}},
	}
	for _, tc := range cases {
		if _, _, err := validFirewallRule(tc.body); err == nil {
			t.Errorf("%s 应拒绝：%+v", tc.name, tc.body)
		}
	}
}
