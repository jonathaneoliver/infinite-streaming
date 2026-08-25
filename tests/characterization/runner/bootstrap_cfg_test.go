package runner

import (
	"net/url"
	"strings"
	"testing"
)

// TestCfgBootstrapURL guards the deferred config-on-connect URL (#937): the probe
// must GET the SAME shape the CLI's up-front shaperBootstrapURL produced — the
// shaper port derived from the API port, and player_id / group_id / proxy.cfg as
// query args on the clip's master playlist.
func TestCfgBootstrapURL(t *testing.T) {
	got, err := cfgBootstrapURL("https://host.example:21000", "big buck", "PID-1", "grp-9", "BLOB==")
	if err != nil {
		t.Fatalf("cfgBootstrapURL: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("result not a URL: %v", err)
	}
	if u.Scheme != "https" {
		t.Errorf("scheme = %q, want https", u.Scheme)
	}
	if u.Host != "host.example:21081" { // 21000 API → 21081 shaper
		t.Errorf("host = %q, want host.example:21081 (shaper port)", u.Host)
	}
	if !strings.HasPrefix(u.Path, "/go-live/") || !strings.HasSuffix(u.Path, "/master_6s.m3u8") {
		t.Errorf("path = %q, want /go-live/<clip>/master_6s.m3u8", u.Path)
	}
	// The clip must be path-escaped in the emitted URL (space → %20).
	if !strings.Contains(got, "/go-live/big%20buck/master_6s.m3u8") {
		t.Errorf("clip not path-escaped in URL: %q", got)
	}
	q := u.Query()
	if q.Get("player_id") != "PID-1" {
		t.Errorf("player_id = %q, want PID-1", q.Get("player_id"))
	}
	if q.Get("group_id") != "grp-9" {
		t.Errorf("group_id = %q, want grp-9", q.Get("group_id"))
	}
	if q.Get("proxy.cfg") != "BLOB==" {
		t.Errorf("proxy.cfg = %q, want BLOB==", q.Get("proxy.cfg"))
	}
	// group_broadcast must be absent — the matrix group is mirrored (broadcast).
	if _, ok := q["group_broadcast"]; ok {
		t.Errorf("group_broadcast should be absent (mirrored group), got %q", q.Get("group_broadcast"))
	}
}

// TestCfgBootstrapURLNoGroup: an ungrouped arm omits group_id entirely.
func TestCfgBootstrapURLNoGroup(t *testing.T) {
	got, err := cfgBootstrapURL("https://h:30000", "clip", "PID", "", "B")
	if err != nil {
		t.Fatalf("cfgBootstrapURL: %v", err)
	}
	u, _ := url.Parse(got)
	if u.Host != "h:30081" {
		t.Errorf("host = %q, want h:30081", u.Host)
	}
	if _, ok := u.Query()["group_id"]; ok {
		t.Errorf("group_id should be absent when empty, got %q", u.Query().Get("group_id"))
	}
}
