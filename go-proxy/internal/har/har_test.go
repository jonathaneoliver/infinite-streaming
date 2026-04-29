package har

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBuild_EmptySources(t *testing.T) {
	doc := Build(nil, BuildOptions{SessionID: "sess-1", PlayerID: "player-a"})
	if doc.Log.Version != HARVersion {
		t.Errorf("version = %q, want %q", doc.Log.Version, HARVersion)
	}
	if doc.Log.Creator.Name != CreatorName {
		t.Errorf("creator name = %q, want %q", doc.Log.Creator.Name, CreatorName)
	}
	if len(doc.Log.Entries) != 0 {
		t.Errorf("entries = %d, want 0", len(doc.Log.Entries))
	}
	// Session metadata always lands under _extensions.session.
	sess, ok := doc.Log.Extensions["session"].(map[string]string)
	if !ok {
		t.Fatalf("expected _extensions.session map, got %T", doc.Log.Extensions["session"])
	}
	if sess["session_id"] != "sess-1" || sess["player_id"] != "player-a" {
		t.Errorf("session metadata wrong: %+v", sess)
	}
}

func TestBuild_TimingMapping(t *testing.T) {
	src := Source{
		Timestamp:   time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC),
		Method:      "GET",
		URL:         "https://example.test/segment.m4s",
		ContentType: "video/mp4",
		Status:      200,
		BytesIn:     1024 * 50,
		BytesOut:    256,
		DNSMs:       2.5,
		ConnectMs:   8.1,
		TLSMs:       12.0,
		TTFBMs:      40.3,
		TransferMs:  120.5,
		TotalMs:     183.4,
	}
	doc := Build([]Source{src}, BuildOptions{SessionID: "s"})
	if len(doc.Log.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(doc.Log.Entries))
	}
	e := doc.Log.Entries[0]
	if e.Time != 183.4 {
		t.Errorf("Time = %v, want 183.4", e.Time)
	}
	if e.Timings.DNS != 2.5 || e.Timings.Connect != 8.1 || e.Timings.SSL != 12.0 ||
		e.Timings.Wait != 40.3 || e.Timings.Receive != 120.5 {
		t.Errorf("Timings mapping off: %+v", e.Timings)
	}
	// HAR spec: blocked/send default to -1 ("not applicable").
	if e.Timings.Blocked != -1 || e.Timings.Send != -1 {
		t.Errorf("Blocked/Send not defaulted to -1: %+v", e.Timings)
	}
	if e.Request.Method != "GET" || e.Request.URL != src.URL {
		t.Errorf("Request mapping off: %+v", e.Request)
	}
	if e.Response.Status != 200 || e.Response.StatusText != "OK" {
		t.Errorf("Response status mapping off: %+v", e.Response)
	}
	if e.Response.Content.MimeType != "video/mp4" {
		t.Errorf("Content.MimeType = %q, want video/mp4", e.Response.Content.MimeType)
	}
	// startedDateTime must be ISO8601 in UTC.
	if !strings.HasSuffix(e.StartedDateTime, "Z") {
		t.Errorf("startedDateTime not UTC: %q", e.StartedDateTime)
	}
}

func TestBuild_NegativeTimingsCollapseToMinusOne(t *testing.T) {
	// HAR 1.2: -1 means "not applicable / not measured".
	doc := Build([]Source{{
		URL:    "x",
		Status: 0,
		// All timings 0 (e.g. faulted before connect).
	}}, BuildOptions{})
	e := doc.Log.Entries[0]
	if e.Timings.DNS != -1 || e.Timings.Connect != -1 || e.Timings.SSL != -1 ||
		e.Timings.Wait != -1 || e.Timings.Receive != -1 {
		t.Errorf("zero timings should map to -1: %+v", e.Timings)
	}
}

func TestBuild_FaultExtension(t *testing.T) {
	doc := Build([]Source{{
		URL:           "https://example.test/seg.m4s",
		Status:        404,
		Faulted:       true,
		FaultType:     "404",
		FaultAction:   "respond",
		FaultCategory: "http",
		RequestKind:   "segment",
	}}, BuildOptions{})
	ext := doc.Log.Entries[0].Extensions
	if ext == nil {
		t.Fatal("expected entry _extensions")
	}
	if got := ext["requestKind"]; got != "segment" {
		t.Errorf("requestKind = %v, want segment", got)
	}
	fault, ok := ext["fault"].(map[string]interface{})
	if !ok {
		t.Fatalf("fault block missing or wrong type: %T", ext["fault"])
	}
	if fault["faulted"] != true || fault["type"] != "404" || fault["action"] != "respond" || fault["category"] != "http" {
		t.Errorf("fault block wrong: %+v", fault)
	}
}

func TestBuild_NoFaultNoExtension(t *testing.T) {
	doc := Build([]Source{{URL: "x", Status: 200}}, BuildOptions{})
	if ext := doc.Log.Entries[0].Extensions; ext != nil {
		t.Errorf("expected no entry _extensions on a clean request, got %+v", ext)
	}
}

func TestBuild_IncidentBlock(t *testing.T) {
	now := time.Date(2026, 4, 28, 13, 30, 0, 0, time.UTC)
	inc := &Incident{
		Reason:    "frozen",
		Source:    "player",
		PlayerID:  "player-a",
		SessionID: "sess-1",
		Timestamp: now,
		Metadata: map[string]interface{}{
			"buffer_depth_s": 0.0,
			"player_state":   "buffering",
		},
	}
	doc := Build(nil, BuildOptions{
		SessionID: "sess-1",
		PlayerID:  "player-a",
		Incident:  inc,
	})
	got, ok := doc.Log.Extensions["incident"].(*Incident)
	if !ok {
		t.Fatalf("incident block wrong type: %T", doc.Log.Extensions["incident"])
	}
	if got.Reason != "frozen" || got.Source != "player" {
		t.Errorf("incident block wrong: %+v", got)
	}
}

func TestBuild_DefaultsAppliedToRequest(t *testing.T) {
	// Empty Method should default to GET; empty ContentType should default
	// to application/octet-stream.
	doc := Build([]Source{{URL: "x", Status: 200}}, BuildOptions{})
	e := doc.Log.Entries[0]
	if e.Request.Method != "GET" {
		t.Errorf("method default = %q, want GET", e.Request.Method)
	}
	if e.Response.Content.MimeType != "application/octet-stream" {
		t.Errorf("mime default = %q, want application/octet-stream", e.Response.Content.MimeType)
	}
}

func TestBuild_RoundTripsThroughJSON(t *testing.T) {
	// A real HAR consumer (Chrome DevTools, Charles) parses the JSON
	// shape — make sure ours marshals + unmarshals to the same shape.
	doc := Build([]Source{
		{URL: "https://example.test/a", Status: 200, BytesIn: 100, TotalMs: 5,
			DNSMs: 1, ConnectMs: 1, TTFBMs: 1, TransferMs: 2},
		{URL: "https://example.test/b", Status: 404, Faulted: true, FaultType: "404"},
	}, BuildOptions{SessionID: "s", PlayerID: "p"})

	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic map[string]interface{}
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Top-level shape: { log: { version, creator, entries } }
	logRaw, ok := generic["log"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected top-level log object, got %T", generic["log"])
	}
	if logRaw["version"] != "1.2" {
		t.Errorf("version = %v, want 1.2", logRaw["version"])
	}
	entries, ok := logRaw["entries"].([]interface{})
	if !ok || len(entries) != 2 {
		t.Fatalf("entries = %v (count %d), want 2", logRaw["entries"], len(entries))
	}
	// Faulted entry should carry _extensions.fault even after JSON round-trip.
	second := entries[1].(map[string]interface{})
	ext := second["_extensions"].(map[string]interface{})
	if ext["fault"] == nil {
		t.Errorf("fault extension lost in round-trip: %+v", second)
	}
}

func TestStatusTextFor(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{200, "OK"},
		{206, "Partial Content"},
		{301, "Moved Permanently"},
		{404, "Not Found"},
		{429, "Too Many Requests"},
		{500, "Internal Server Error"},
		{504, "Gateway Timeout"},
		// Unknown specifics should fall through to bucketed text.
		{418, "Client Error"},
		{599, "Server Error"},
		{0, ""},
	}
	for _, c := range cases {
		if got := statusTextFor(c.status); got != c.want {
			t.Errorf("statusTextFor(%d) = %q, want %q", c.status, got, c.want)
		}
	}
}

func TestMsOrNeg(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{0, -1},
		{-5, -1},
		{0.001, 0.001},
		{42, 42},
	}
	for _, c := range cases {
		if got := msOrNeg(c.in); got != c.want {
			t.Errorf("msOrNeg(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestDefaultStr(t *testing.T) {
	if got := defaultStr("", "GET"); got != "GET" {
		t.Errorf("defaultStr(\"\", \"GET\") = %q, want GET", got)
	}
	if got := defaultStr("POST", "GET"); got != "POST" {
		t.Errorf("defaultStr(\"POST\", ...) = %q, want POST", got)
	}
}
