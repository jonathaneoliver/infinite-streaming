// sb_config_on_connect_test.go exercises #712 config-on-connect end-to-end and
// — per #717 — verifies the FULL proxy.* URL-arg vocabulary materializes onto
// the session correctly. Each case curls the bootstrap URL with proxy.* args,
// follows the 302, and asserts the resulting v1 SessionData fields straight off
// /api/sessions (no PATCH, no player). This is the foundation the
// characterization suite (#714) relies on: a silently-dropped or mis-mapped arg
// would make a config-on-connect run look correct while testing the wrong
// config.
package server_behavior

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// configProbe runs the bootstrap with a caller-supplied proxy.* query suffix,
// follows the 302, and returns the post-redirect final URL + the materialized
// session map. It registers a t.Cleanup that DELETEs the session so a run of
// many cases doesn't exhaust the small proxy session pool (config-on-connect
// mints a fresh player_id per probe).
func configProbe(t *testing.T, proxyArgs string) (*url.URL, map[string]any, string) {
	t.Helper()
	host := env("THROUGHPUT_HOST", defaultHost)
	apiPort := env("THROUGHPUT_API_PORT", defaultAPIPort)
	insecure := envBool("THROUGHPUT_INSECURE", defaultInsecure)
	apiBase := host + ":" + apiPort
	shaperBase := host + ":" + shaperPortFromUI(apiPort)
	c := newClient(insecure)

	content, err := discoverContent(c, apiBase)
	if err != nil {
		t.Fatalf("discover content: %v", err)
	}
	playerID := uuid.New().String()
	t.Cleanup(func() { releaseSession(apiBase, insecure, playerID) })

	masterURL := fmt.Sprintf(
		"https://%s/go-live/%s/master_6s.m3u8?player_id=%s&%s",
		shaperBase, url.PathEscape(content), url.QueryEscape(playerID), proxyArgs,
	)
	_, finalURL, err := httpGet(c, masterURL)
	if err != nil {
		t.Fatalf("master fetch: %v", err)
	}
	sess, err := getSessionMap(c, apiBase, playerID)
	if err != nil {
		t.Fatalf("get session map: %v", err)
	}
	return finalURL, sess, playerID
}

// bootstrapStatus fires the bootstrap GET WITHOUT following the redirect and
// returns the status code — used for the fail-closed negatives, where the
// proxy must reject with 400 (allocating no session). A fresh player_id is used
// per call; on a rare 3xx (a case that unexpectedly succeeded) the allocated
// session is released.
func bootstrapStatus(t *testing.T, proxyArgs string) int {
	t.Helper()
	host := env("THROUGHPUT_HOST", defaultHost)
	apiPort := env("THROUGHPUT_API_PORT", defaultAPIPort)
	insecure := envBool("THROUGHPUT_INSECURE", defaultInsecure)
	apiBase := host + ":" + apiPort
	shaperBase := host + ":" + shaperPortFromUI(apiPort)

	content, err := discoverContent(newClient(insecure), apiBase)
	if err != nil {
		t.Fatalf("discover content: %v", err)
	}
	playerID := uuid.New().String()
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}},
	}
	u := fmt.Sprintf("https://%s/go-live/%s/master_6s.m3u8?player_id=%s&%s",
		shaperBase, url.PathEscape(content), url.QueryEscape(playerID), proxyArgs)
	resp, err := client.Get(u)
	if err != nil {
		t.Fatalf("bootstrap GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		releaseSession(apiBase, insecure, playerID)
	}
	return resp.StatusCode
}

// releaseSession frees the proxy session slot (best-effort).
func releaseSession(apiBase string, insecure bool, playerID string) {
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}}}
	req, err := http.NewRequest(http.MethodDelete, fmt.Sprintf("https://%s/api/v2/players/%s", apiBase, playerID), nil)
	if err != nil {
		return
	}
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
	}
}

// ---- helpers to read JSON-decoded SessionData values -----------------------

func asBool(v any) bool     { b, _ := v.(bool); return b }
func asString(v any) string { s, _ := v.(string); return s }
func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return -1
}

// ============================================================================
// shape: rate / delay / loss + redirect hygiene
// ============================================================================

func TestConfigOnConnect_Shape(t *testing.T) {
	finalURL, sess, pid := configProbe(t,
		"proxy.shape.rate_mbps=2.5&proxy.shape.delay_ms=120&proxy.shape.loss_pct=1.5")

	// 302 lands on a session port with no proxy.* leaking through; player_id kept.
	if strings.Contains(finalURL.RawQuery, "proxy.") {
		t.Errorf("redirect URL still carries proxy.* args: %s", finalURL.String())
	}
	if !strings.Contains(finalURL.RawQuery, "player_id=") {
		t.Errorf("redirect URL dropped player_id: %s", finalURL.String())
	}
	// Materialized before any PATCH — and value-typed (number → float64).
	if got := asFloat(sess["nftables_bandwidth_mbps"]); got != 2.5 {
		t.Errorf("nftables_bandwidth_mbps = %v, want 2.5 (player_id=%s)", sess["nftables_bandwidth_mbps"], pid)
	}
	if got := asFloat(sess["nftables_delay_ms"]); got != 120 {
		t.Errorf("nftables_delay_ms = %v, want 120", sess["nftables_delay_ms"])
	}
	if got := asFloat(sess["nftables_packet_loss"]); got != 1.5 {
		t.Errorf("nftables_packet_loss = %v, want 1.5", sess["nftables_packet_loss"])
	}
}

// ============================================================================
// shape.transport_fault
// ============================================================================

func TestConfigOnConnect_TransportFault(t *testing.T) {
	_, sess, _ := configProbe(t,
		"proxy.shape.transport_fault.type=drop"+
			"&proxy.shape.transport_fault.frequency=2"+
			"&proxy.shape.transport_fault.consecutive=1")

	if got := asString(sess["transport_failure_type"]); got != "drop" {
		t.Errorf("transport_failure_type = %q, want drop", got)
	}
	if got := asString(sess["transport_fault_type"]); got != "drop" {
		t.Errorf("transport_fault_type = %q, want drop", got)
	}
	if got := asFloat(sess["transport_failure_frequency"]); got != 2 {
		t.Errorf("transport_failure_frequency = %v, want 2", sess["transport_failure_frequency"])
	}
}

// ============================================================================
// content
// ============================================================================

func TestConfigOnConnect_Content(t *testing.T) {
	_, sess, _ := configProbe(t,
		"proxy.content.strip_codecs=true"+
			"&proxy.content.strip_average_bandwidth=true"+
			"&proxy.content.strip_resolution=true"+
			"&proxy.content.overstate_bandwidth=true"+
			"&proxy.content.variant_order=ascending")

	for key := range map[string]struct{}{
		"content_strip_codecs":            {},
		"content_strip_average_bandwidth": {},
		"content_strip_resolution":        {},
		"content_overstate_bandwidth":     {},
	} {
		if !asBool(sess[key]) {
			t.Errorf("%s = %v, want true", key, sess[key])
		}
	}
	if got := asString(sess["content_variant_order"]); got != "ascending" {
		t.Errorf("content_variant_order = %q, want ascending", got)
	}
}

// ============================================================================
// transfer_timeouts
// ============================================================================

func TestConfigOnConnect_TransferTimeouts(t *testing.T) {
	_, sess, _ := configProbe(t,
		"proxy.transfer_timeouts.active_timeout_seconds=20"+
			"&proxy.transfer_timeouts.idle_timeout_seconds=8"+
			"&proxy.transfer_timeouts.applies_segments=true"+
			"&proxy.transfer_timeouts.applies_manifests=false")

	if got := asFloat(sess["transfer_active_timeout_seconds"]); got != 20 {
		t.Errorf("transfer_active_timeout_seconds = %v, want 20", sess["transfer_active_timeout_seconds"])
	}
	if got := asFloat(sess["transfer_idle_timeout_seconds"]); got != 8 {
		t.Errorf("transfer_idle_timeout_seconds = %v, want 8", sess["transfer_idle_timeout_seconds"])
	}
	if !asBool(sess["transfer_timeout_applies_segments"]) {
		t.Errorf("transfer_timeout_applies_segments = %v, want true", sess["transfer_timeout_applies_segments"])
	}
	if asBool(sess["transfer_timeout_applies_manifests"]) {
		t.Errorf("transfer_timeout_applies_manifests = %v, want false", sess["transfer_timeout_applies_manifests"])
	}
}

// ============================================================================
// shape.pattern (nested) — stashed on _v2_shape_pattern
// ============================================================================

func TestConfigOnConnect_Pattern(t *testing.T) {
	_, sess, _ := configProbe(t,
		"proxy.shape.pattern.template=pyramid"+
			"&proxy.shape.pattern.margin_pct=5"+
			"&proxy.shape.pattern.default_step_seconds=12")

	pat, ok := sess["_v2_shape_pattern"].(map[string]any)
	if !ok {
		t.Fatalf("_v2_shape_pattern not a map: %#v", sess["_v2_shape_pattern"])
	}
	if got := asString(pat["template"]); got != "pyramid" {
		t.Errorf("pattern.template = %q, want pyramid", got)
	}
	if got := asFloat(pat["margin_pct"]); got != 5 {
		t.Errorf("pattern.margin_pct = %v, want 5", pat["margin_pct"])
	}
	if got := asFloat(pat["default_step_seconds"]); got != 12 {
		t.Errorf("pattern.default_step_seconds = %v, want 12", pat["default_step_seconds"])
	}
}

// ============================================================================
// labels (dotted key kept verbatim) — surfaced on _v2_labels
// ============================================================================

func TestConfigOnConnect_Labels(t *testing.T) {
	_, sess, _ := configProbe(t,
		"proxy.labels.test=cocon717&proxy.labels.ci.run=42")

	labels, ok := sess["_v2_labels"].(map[string]any)
	if !ok {
		t.Fatalf("_v2_labels not a map: %#v", sess["_v2_labels"])
	}
	if got := asString(labels["test"]); got != "cocon717" {
		t.Errorf("labels[test] = %q, want cocon717", got)
	}
	// Dotted label key must be kept as ONE key, not nested.
	if _, present := labels["ci.run"]; !present {
		t.Errorf("dotted label key ci.run missing (mis-nested?): %#v", labels)
	}
}

// ============================================================================
// fault_rules[] (bracket/array form → per-surface v1 keys)
// ============================================================================

func TestConfigOnConnect_FaultRule(t *testing.T) {
	_, sess, _ := configProbe(t,
		"proxy.fault_rules[0].type=corrupted"+
			"&proxy.fault_rules[0].mode=requests"+
			"&proxy.fault_rules[0].frequency=3"+
			"&proxy.fault_rules[0].filter.request_kind[0]=segment")

	if got := asString(sess["segment_failure_type"]); got != "corrupted" {
		t.Errorf("segment_failure_type = %q, want corrupted", got)
	}
	if got := asFloat(sess["segment_failure_frequency"]); got != 3 {
		t.Errorf("segment_failure_frequency = %v, want 3", sess["segment_failure_frequency"])
	}
	if got := asString(sess["segment_failure_mode"]); got != "requests" {
		t.Errorf("segment_failure_mode = %q, want requests", got)
	}
}

// ============================================================================
// proxy.cfg base64 base tier + per-field override by flat args
// ============================================================================

func TestConfigOnConnect_CfgBase64Precedence(t *testing.T) {
	cfg := base64.RawURLEncoding.EncodeToString([]byte(`{"shape":{"rate_mbps":1.0,"delay_ms":50}}`))
	_, sess, _ := configProbe(t, "proxy.cfg="+cfg+"&proxy.shape.rate_mbps=9")

	// Flat arg overrides cfg per-field …
	if got := asFloat(sess["nftables_bandwidth_mbps"]); got != 9 {
		t.Errorf("nftables_bandwidth_mbps = %v, want 9 (flat arg overrides cfg)", sess["nftables_bandwidth_mbps"])
	}
	// … untouched cfg field survives.
	if got := asFloat(sess["nftables_delay_ms"]); got != 50 {
		t.Errorf("nftables_delay_ms = %v, want 50 (from cfg)", sess["nftables_delay_ms"])
	}
}

// ============================================================================
// value typing: number → float, true/false → bool, non-numeric → string
// ============================================================================

func TestConfigOnConnect_ValueTyping(t *testing.T) {
	_, sess, _ := configProbe(t,
		"proxy.shape.rate_mbps=2.5&proxy.content.strip_codecs=true&proxy.content.variant_order=ascending")

	if _, ok := sess["nftables_bandwidth_mbps"].(float64); !ok {
		t.Errorf("nftables_bandwidth_mbps not a number: %T", sess["nftables_bandwidth_mbps"])
	}
	if _, ok := sess["content_strip_codecs"].(bool); !ok {
		t.Errorf("content_strip_codecs not a bool: %T", sess["content_strip_codecs"])
	}
	if _, ok := sess["content_variant_order"].(string); !ok {
		t.Errorf("content_variant_order not a string: %T", sess["content_variant_order"])
	}
}

// ============================================================================
// fail-closed negatives — must 400, allocating no session
// ============================================================================

func TestConfigOnConnect_FailClosed(t *testing.T) {
	cases := map[string]string{
		"bad base64 cfg":    "proxy.cfg=!!!not-base64!!!",
		"non-numeric index": "proxy.fault_rules[x].type=corrupted",
		// filter.variant has no v1 surface yet → rejected (same boundary as PATCH).
		"unsupported filter.variant": "proxy.fault_rules[0].type=corrupted" +
			"&proxy.fault_rules[0].filter.variant.resolutions[0]=1080p",
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if got := bootstrapStatus(t, args); got != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 for %q", got, args)
			}
		})
	}
}
