package rpc

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

// zstaticProvinceCode resolves the province segment of host location
// strings (ip-api.com "RegionName-City" form); unrecognized values fall
// back to Beijing.
func TestZstaticProvinceCode(t *testing.T) {
	cases := map[string]string{
		"浙江省-杭州市":       "zj",
		"浙江省":           "zj",
		"浙江 杭州":         "zj",
		"浙江杭州":          "zj",
		"北京市-朝阳区":       "bj",
		"上海市":           "sh",
		"重庆市-渝北区":       "cq",
		"黑龙江省-哈尔滨市":     "hl",
		"内蒙古自治区-呼和浩特市":  "nm",
		"广西壮族自治区-南宁市":   "gx",
		"新疆维吾尔自治区-乌鲁木齐": "xj",
		"宁夏回族自治区-银川市":   "nx",
		"西藏自治区-拉萨":      "xz",
		"四川省-成都市":       "sc",
		"广东省深圳市":        "gd",
		"中国":            "bj",
		"":              "bj",
		"   ":           "bj",
		"Atlantis":      "bj",
		"香港":            "bj",
	}
	for location, want := range cases {
		if got := zstaticProvinceCode(location); got != want {
			t.Errorf("zstaticProvinceCode(%q) = %q, want %q", location, got, want)
		}
	}
}

// DefaultProbeTargets builds one Zstatic node per carrier for the host's
// province, falling back to Beijing for unknown locations.
func TestDefaultProbeTargets(t *testing.T) {
	want := map[string]string{
		carrierTelecom: "zj-ct-v4.ip.zstaticcdn.com:80",
		carrierUnicom:  "zj-cu-v4.ip.zstaticcdn.com:80",
		carrierMobile:  "zj-cm-v4.ip.zstaticcdn.com:80",
	}
	if got := DefaultProbeTargets("浙江省-杭州市"); !reflect.DeepEqual(got, want) {
		t.Fatalf("zhejiang: got %v, want %v", got, want)
	}
	want = map[string]string{
		carrierTelecom: "bj-ct-v4.ip.zstaticcdn.com:80",
		carrierUnicom:  "bj-cu-v4.ip.zstaticcdn.com:80",
		carrierMobile:  "bj-cm-v4.ip.zstaticcdn.com:80",
	}
	if got := DefaultProbeTargets(""); !reflect.DeepEqual(got, want) {
		t.Fatalf("empty location: got %v, want %v", got, want)
	}
}

// normalizeProbeTargets accepts host[:port] endpoints (port defaulting to
// 80), treats empty values as "carrier disabled", and rejects everything
// else.
func TestNormalizeProbeTargets(t *testing.T) {
	ok := map[string]string{
		"telecom": "zj-ct-v4.ip.zstaticcdn.com:80",
		"unicom":  "gd-cu-v4.ip.zstaticcdn.com", // port defaults to 80
		"mobile":  "  192.0.2.1:8080  ",         // trimmed, explicit port
	}
	got, err := normalizeProbeTargets(ok)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]string{
		carrierTelecom: "zj-ct-v4.ip.zstaticcdn.com:80",
		carrierUnicom:  "gd-cu-v4.ip.zstaticcdn.com:80",
		carrierMobile:  "192.0.2.1:8080",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized = %v, want %v", got, want)
	}

	// Empty map / all-empty values mean "no carrier probed".
	if got, err := normalizeProbeTargets(nil); err != nil || got != nil {
		t.Fatalf("nil: got (%v, %v), want (nil, nil)", got, err)
	}
	if got, err := normalizeProbeTargets(map[string]string{"telecom": "  "}); err != nil || got != nil {
		t.Fatalf("blank value: got (%v, %v), want (nil, nil)", got, err)
	}

	bad := map[string]map[string]string{
		"URL scheme":      {"telecom": "https://www.zstaticcdn.com/"},
		"scheme-less URL": {"telecom": "//zj-ct-v4.ip.zstaticcdn.com"},
		"space inside":    {"telecom": "zj-ct-v4.ip.zstaticcdn.com :80"},
		"port 0":          {"telecom": "zj-ct-v4.ip.zstaticcdn.com:0"},
		"port 65536":      {"telecom": "zj-ct-v4.ip.zstaticcdn.com:65536"},
		"port not number": {"telecom": "zj-ct-v4.ip.zstaticcdn.com:http"},
		"empty host":      {"telecom": ":80"},
		"bad host chars":  {"telecom": "zj ct;rm -rf:v4.ip.zstaticcdn.com:80"},
		"over-long host":  {"telecom": strings.Repeat("a", 254) + ".com"},
		"unknown carrier": {"carrier": "zj-ct-v4.ip.zstaticcdn.com:80"},
		"path in target":  {"telecom": "zj-ct-v4.ip.zstaticcdn.com:80/ping"},
	}
	for name, raw := range bad {
		if _, err := normalizeProbeTargets(raw); err == nil {
			t.Errorf("%s: normalizeProbeTargets(%q) should fail", name, raw)
		}
	}
}

// EffectiveProbeTargets gates on the per-host switch, prefers stored
// targets, and falls back to the Zstatic defaults of the host location.
func TestEffectiveProbeTargets(t *testing.T) {
	if got := EffectiveProbeTargets(nil); got != nil {
		t.Fatalf("nil agent: got %v, want nil", got)
	}
	off := &Agent{ProbeEnabled: false, Location: "浙江省-杭州市", ProbeTargets: map[string]string{"telecom": "a:80"}}
	if got := EffectiveProbeTargets(off); got != nil {
		t.Fatalf("disabled: got %v, want nil", got)
	}

	on := &Agent{ProbeEnabled: true, Location: "浙江省-杭州市", ProbeTargets: map[string]string{"telecom": "zj-ct-v4.ip.zstaticcdn.com:80"}}
	want := map[string]string{carrierTelecom: "zj-ct-v4.ip.zstaticcdn.com:80"}
	if got := EffectiveProbeTargets(on); !reflect.DeepEqual(got, want) {
		t.Fatalf("stored targets: got %v, want %v", got, want)
	}

	// Blank carrier values are skipped, not probed.
	partial := &Agent{ProbeEnabled: true, Location: "浙江省-杭州市", ProbeTargets: map[string]string{"telecom": "zj-ct-v4.ip.zstaticcdn.com:80", "mobile": "  "}}
	want = map[string]string{carrierTelecom: "zj-ct-v4.ip.zstaticcdn.com:80"}
	if got := EffectiveProbeTargets(partial); !reflect.DeepEqual(got, want) {
		t.Fatalf("partial targets: got %v, want %v", got, want)
	}

	// No stored targets at all → Zstatic defaults for the host location.
	fallback := &Agent{ProbeEnabled: true, Location: "四川省-成都市"}
	want = map[string]string{
		carrierTelecom: "sc-ct-v4.ip.zstaticcdn.com:80",
		carrierUnicom:  "sc-cu-v4.ip.zstaticcdn.com:80",
		carrierMobile:  "sc-cm-v4.ip.zstaticcdn.com:80",
	}
	if got := EffectiveProbeTargets(fallback); !reflect.DeepEqual(got, want) {
		t.Fatalf("default targets: got %v, want %v", got, want)
	}
}

// SetAgentBilling persists valid per-carrier targets and rejects invalid
// ones without leaving the agent half-mutated.
func TestSetAgentBillingProbeTargets(t *testing.T) {
	r := NewRegistry("", slog.Default())
	r.agents["h1"] = &Agent{ID: "h1", Hostname: "a"}

	cfg := HostBillingConfig{
		ProbeEnabled: true,
		ProbeTargets: map[string]string{"telecom": "zj-ct-v4.ip.zstaticcdn.com", "unicom": "", "mobile": "192.0.2.1:8080"},
	}
	if err := r.SetAgentBilling("h1", cfg); err != nil {
		t.Fatalf("set valid targets: %v", err)
	}
	want := map[string]string{carrierTelecom: "zj-ct-v4.ip.zstaticcdn.com:80", carrierMobile: "192.0.2.1:8080"}
	if got := r.agents["h1"].ProbeTargets; !reflect.DeepEqual(got, want) {
		t.Fatalf("stored targets = %v, want %v", got, want)
	}
	if !r.agents["h1"].ProbeEnabled {
		t.Fatal("probe_enabled should be stored")
	}

	// Invalid carrier key: nothing on the agent may change.
	bad := HostBillingConfig{ProbeEnabled: false, ProbeTargets: map[string]string{"carrier": "zj-ct-v4.ip.zstaticcdn.com:80"}}
	if err := r.SetAgentBilling("h1", bad); err == nil {
		t.Fatal("unknown carrier should be rejected")
	}
	if got := r.agents["h1"].ProbeTargets; !reflect.DeepEqual(got, want) {
		t.Fatalf("rejected save clobbered targets: %v", got)
	}
	if !r.agents["h1"].ProbeEnabled {
		t.Fatal("rejected save must not clear probe_enabled")
	}

	// Disabling probing is a valid update (targets cleared).
	if err := r.SetAgentBilling("h1", HostBillingConfig{ProbeEnabled: false}); err != nil {
		t.Fatalf("disable probing: %v", err)
	}
	if r.agents["h1"].ProbeEnabled || r.agents["h1"].ProbeTargets != nil {
		t.Fatalf("expected probing disabled with no targets, got enabled=%v targets=%v",
			r.agents["h1"].ProbeEnabled, r.agents["h1"].ProbeTargets)
	}
}
