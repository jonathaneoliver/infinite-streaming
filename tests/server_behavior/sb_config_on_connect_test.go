// sb_config_on_connect_test.go exercises #712 config-on-connect: the
// bootstrap playback URL carries proxy.* args, the proxy materializes the
// session config atomically and 302s to a session-bound URL with the args
// stripped — so config is live before the first segment, with no separate
// PATCH round-trip. Mirrors newProbe's bootstrap but injects proxy.* args and
// asserts the resulting session state directly off /api/sessions.
package server_behavior

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// configProbe runs the bootstrap with a caller-supplied proxy.* query suffix
// and returns the post-redirect final URL plus the materialized session map.
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

// TestConfigOnConnect_Shape: a rate cap supplied on the bootstrap URL is live
// on the session before any PATCH, and the redirect URL is clean of proxy.*.
func TestConfigOnConnect_Shape(t *testing.T) {
	finalURL, sess, playerID := configProbe(t,
		"proxy.shape.rate_mbps=2.5&proxy.shape.delay_ms=120&proxy.labels.test=cfg712")

	// The 302 must land on a session port with no proxy.* leaking through.
	if strings.Contains(finalURL.RawQuery, "proxy.") {
		t.Errorf("redirect URL still carries proxy.* args: %s", finalURL.String())
	}
	if !strings.Contains(finalURL.RawQuery, "player_id=") {
		t.Errorf("redirect URL dropped player_id: %s", finalURL.String())
	}

	// Config is materialized on the session BEFORE any control-plane PATCH.
	if got := asFloat(sess["nftables_bandwidth_mbps"]); got != 2.5 {
		t.Errorf("nftables_bandwidth_mbps = %v, want 2.5 (player_id=%s)", sess["nftables_bandwidth_mbps"], playerID)
	}
	if got := asFloat(sess["nftables_delay_ms"]); got != 120 {
		t.Errorf("nftables_delay_ms = %v, want 120", sess["nftables_delay_ms"])
	}
}

// TestConfigOnConnect_FaultRule: a corrupted-segment fault rule supplied on the
// bootstrap URL lands on the v1 segment surface before the first fetch.
func TestConfigOnConnect_FaultRule(t *testing.T) {
	_, sess, _ := configProbe(t,
		"proxy.fault_rules[0].type=corrupted"+
			"&proxy.fault_rules[0].mode=requests"+
			"&proxy.fault_rules[0].frequency=3"+
			"&proxy.fault_rules[0].filter.request_kind[0]=segment")

	if got, _ := sess["segment_failure_type"].(string); got != "corrupted" {
		t.Errorf("segment_failure_type = %q, want corrupted", got)
	}
	if got := asFloat(sess["segment_failure_frequency"]); got != 3 {
		t.Errorf("segment_failure_frequency = %v, want 3", sess["segment_failure_frequency"])
	}
}

// asFloat coerces a JSON-decoded numeric (float64) session field to float64,
// returning a sentinel that won't match any expected value on type mismatch.
func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return -1
}
