package main

import (
	"encoding/base64"
	"net/url"
	"reflect"
	"testing"

	v2server "github.com/jonathaneoliver/infinite-streaming/go-proxy/internal/v2/server"
)

// mustQuery parses a raw query string into url.Values, failing the test on
// error.
func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	q, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", raw, err)
	}
	return q
}

func TestConfigOnConnect_NoProxyArgs(t *testing.T) {
	patch, has, err := parseProxyArgs(mustQuery(t, "player_id=abc&play_id=def"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if has {
		t.Fatalf("hasProxy = true, want false for a query with no proxy.* args")
	}
	if patch != nil {
		t.Fatalf("patch = %v, want nil when no proxy.* args present", patch)
	}
}

func TestConfigOnConnect_ParseScalarShape(t *testing.T) {
	patch, has, err := parseProxyArgs(mustQuery(t,
		"player_id=abc&proxy.shape.rate_mbps=2.5&proxy.shape.delay_ms=120&proxy.shape.loss_pct=1.5"))
	if err != nil || !has {
		t.Fatalf("parse: has=%v err=%v", has, err)
	}
	want := map[string]any{
		"shape": map[string]any{
			"rate_mbps": 2.5,
			"delay_ms":  120.0,
			"loss_pct":  1.5,
		},
	}
	if !reflect.DeepEqual(patch, want) {
		t.Fatalf("patch mismatch\n got: %#v\nwant: %#v", patch, want)
	}
}

func TestConfigOnConnect_ParseNestedAndArrays(t *testing.T) {
	patch, _, err := parseProxyArgs(mustQuery(t,
		"proxy.fault_rules[0].type=corrupted"+
			"&proxy.fault_rules[0].mode=requests"+
			"&proxy.fault_rules[0].frequency=3"+
			"&proxy.fault_rules[0].filter.request_kind[0]=segment"+
			"&proxy.fault_rules[0].filter.request_kind[1]=partial"))
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	rules, ok := patch["fault_rules"].([]any)
	if !ok || len(rules) != 1 {
		t.Fatalf("fault_rules shape wrong: %#v", patch["fault_rules"])
	}
	rule := rules[0].(map[string]any)
	if rule["type"] != "corrupted" || rule["mode"] != "requests" || rule["frequency"] != 3.0 {
		t.Fatalf("rule scalars wrong: %#v", rule)
	}
	filter := rule["filter"].(map[string]any)
	kinds := filter["request_kind"].([]any)
	if !reflect.DeepEqual(kinds, []any{"segment", "partial"}) {
		t.Fatalf("request_kind array wrong: %#v", kinds)
	}
}

func TestConfigOnConnect_BoolAndStringCoercion(t *testing.T) {
	patch, _, err := parseProxyArgs(mustQuery(t,
		"proxy.content.strip_resolution=true&proxy.content.variant_order=ascending"))
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	content := patch["content"].(map[string]any)
	if content["strip_resolution"] != true {
		t.Fatalf("strip_resolution = %#v, want bool true", content["strip_resolution"])
	}
	if content["variant_order"] != "ascending" {
		t.Fatalf("variant_order = %#v, want string", content["variant_order"])
	}
}

func TestConfigOnConnect_LabelsKeyVerbatim(t *testing.T) {
	// Label keys may contain dots — everything after labels. is one key.
	patch, _, err := parseProxyArgs(mustQuery(t,
		"proxy.labels.test=steady_cap&proxy.labels.ci.run=42"))
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	labels := patch["labels"].(map[string]any)
	if labels["test"] != "steady_cap" {
		t.Fatalf("labels.test = %#v", labels["test"])
	}
	if labels["ci.run"] != "42" && labels["ci.run"] != 42.0 {
		// "42" parses as a number; either is acceptable as a label value.
		t.Fatalf("labels[ci.run] = %#v, want the dotted key preserved", labels["ci.run"])
	}
}

func TestConfigOnConnect_CfgBase64BaseWithOverride(t *testing.T) {
	cfg := base64.RawURLEncoding.EncodeToString([]byte(`{"shape":{"rate_mbps":1.0,"delay_ms":50}}`))
	patch, has, err := parseProxyArgs(mustQuery(t,
		"proxy.cfg="+cfg+"&proxy.shape.rate_mbps=9"))
	if err != nil || !has {
		t.Fatalf("parse: has=%v err=%v", has, err)
	}
	shape := patch["shape"].(map[string]any)
	// Flat arg overrides cfg per-field; untouched cfg field survives.
	if shape["rate_mbps"] != 9.0 {
		t.Fatalf("rate_mbps = %#v, want 9 (flat arg overrides cfg)", shape["rate_mbps"])
	}
	if shape["delay_ms"] != 50.0 {
		t.Fatalf("delay_ms = %#v, want 50 (from cfg, untouched)", shape["delay_ms"])
	}
}

func TestConfigOnConnect_ParseErrors(t *testing.T) {
	cases := map[string]string{
		"bad base64":      "proxy.cfg=!!!not-base64!!!",
		"bad json in cfg": "proxy.cfg=" + base64.RawURLEncoding.EncodeToString([]byte(`{not json}`)),
		"non-numeric idx": "proxy.fault_rules[x].type=corrupted",
		"type conflict":   "proxy.shape=2&proxy.shape.rate_mbps=3",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseProxyArgs(mustQuery(t, raw)); err == nil {
				t.Fatalf("expected error for %q, got nil", raw)
			}
		})
	}
}

func TestConfigOnConnect_CoerceURLValue(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{"true", true},
		{"false", false},
		{"2.5", 2.5},
		{"6000000", 6000000.0},
		{"1080p", "1080p"},
		{"avc1", "avc1"},
		{"inf", "inf"},
		{"NaN", "NaN"},
		{"ascending", "ascending"},
	}
	for _, c := range cases {
		if got := coerceURLValue(c.in); got != c.want {
			t.Errorf("coerceURLValue(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestConfigOnConnect_StripProxyArgs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"keeps non-proxy, drops proxy", "player_id=abc&proxy.shape.rate_mbps=2.5&play_id=def", "player_id=abc&play_id=def"},
		{"drops cfg too", "proxy.cfg=ABC&player_id=x", "player_id=x"},
		{"preserves order", "a=1&proxy.x=2&b=3&proxy.y=4&c=5", "a=1&b=3&c=5"},
		{"all proxy", "proxy.a=1&proxy.b=2", ""},
		{"empty", "", ""},
		{"nothing to strip", "player_id=abc", "player_id=abc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripProxyArgs(c.in); got != c.want {
				t.Errorf("stripProxyArgs(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestConfigOnConnect_MaterializeEndToEnd parses proxy.* args and runs them
// through the SAME translator the PATCH API uses (v2server.ApplyConfigPatch),
// asserting the flat v1 SessionData keys the request path reads. This proves
// the URL vocabulary lands on the real session model end-to-end.
func TestConfigOnConnect_MaterializeEndToEnd(t *testing.T) {
	patch, has, err := parseProxyArgs(mustQuery(t,
		"proxy.shape.rate_mbps=2.5"+
			"&proxy.shape.delay_ms=120"+
			"&proxy.shape.loss_pct=1.5"+
			"&proxy.content.variant_order=ascending"+
			"&proxy.content.strip_resolution=true"+
			"&proxy.labels.test=cfg712"+
			"&proxy.fault_rules[0].type=corrupted"+
			"&proxy.fault_rules[0].mode=requests"+
			"&proxy.fault_rules[0].frequency=3"+
			"&proxy.fault_rules[0].filter.request_kind[0]=segment"))
	if err != nil || !has {
		t.Fatalf("parse: has=%v err=%v", has, err)
	}
	sess := SessionData{}
	if aerr := v2server.ApplyConfigPatch(sess, patch); aerr != nil {
		t.Fatalf("ApplyConfigPatch: %v", aerr)
	}
	// shape → nftables_*
	if got := getFloat(sess, "nftables_bandwidth_mbps"); got != 2.5 {
		t.Errorf("nftables_bandwidth_mbps = %v, want 2.5", got)
	}
	if got := getInt(sess, "nftables_delay_ms"); got != 120 {
		t.Errorf("nftables_delay_ms = %v, want 120", got)
	}
	if got := getFloat(sess, "nftables_packet_loss"); got != 1.5 {
		t.Errorf("nftables_packet_loss = %v, want 1.5", got)
	}
	// content → content_*
	if got := getString(sess, "content_variant_order"); got != "ascending" {
		t.Errorf("content_variant_order = %q, want ascending", got)
	}
	if !getBool(sess, "content_strip_resolution") {
		t.Errorf("content_strip_resolution = false, want true")
	}
	// fault_rules[segment] → segment_*
	if got := getString(sess, "segment_failure_type"); got != "corrupted" {
		t.Errorf("segment_failure_type = %q, want corrupted", got)
	}
	if got := getInt(sess, "segment_failure_frequency"); got != 3 {
		t.Errorf("segment_failure_frequency = %v, want 3", got)
	}
	if got := getString(sess, "segment_failure_mode"); got != "requests" {
		t.Errorf("segment_failure_mode = %q, want requests", got)
	}
	// labels → _v2_labels
	labels, ok := sess["_v2_labels"].(map[string]any)
	if !ok || labels["test"] != "cfg712" {
		t.Errorf("_v2_labels = %#v, want test=cfg712", sess["_v2_labels"])
	}
}

// TestConfigOnConnect_MaterializeRejectsVariantFilter documents that config-on-
// connect inherits the PATCH translator's limits: filter.variant has no v1
// surface yet, so it is rejected at materialization (→ 400 on the bootstrap).
func TestConfigOnConnect_MaterializeRejectsVariantFilter(t *testing.T) {
	patch, _, err := parseProxyArgs(mustQuery(t,
		"proxy.fault_rules[0].type=corrupted"+
			"&proxy.fault_rules[0].filter.variant.bandwidth_above=6000000"))
	if err != nil {
		t.Fatalf("parse should succeed (translation is where it's rejected): %v", err)
	}
	if aerr := v2server.ApplyConfigPatch(SessionData{}, patch); aerr == nil {
		t.Fatalf("expected ApplyConfigPatch to reject filter.variant, got nil")
	}
}
