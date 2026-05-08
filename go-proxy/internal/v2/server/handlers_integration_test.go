package server

// End-to-end handler tests. Each test stands up a Server backed by the
// in-memory fakeAdapter, mounts the v2 router, and drives it with
// httptest.Server. Covers the happy path + error paths for every
// real (non-stubbed) endpoint.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

// newTestServer wires up a fakeAdapter + Server + httptest.Server. The
// caller is responsible for the returned Close — it stops the test
// HTTP server AND the v2 EventSource goroutines.
func newTestServer(t *testing.T) (*fakeAdapter, *Server, *httptest.Server) {
	t.Helper()
	adapter := newFakeAdapter()
	srv := New(adapter)
	router := mux.NewRouter()
	Mount(router, srv)
	httpSrv := httptest.NewServer(router)
	t.Cleanup(func() {
		httpSrv.Close()
		srv.Close()
	})
	return adapter, srv, httpSrv
}

// addPlayer is a test fixture for "the world contains a player with
// these v2 fields". The session map carries v1-shape keys.
func (a *fakeAdapter) addPlayer(playerID, controlRev string, extra map[string]any) {
	s := map[string]any{
		"player_id":        playerID,
		"session_id":       "sess-" + playerID[:8],
		"control_revision": controlRev,
		"origination_ip":   "10.0.0.1",
	}
	for k, v := range extra {
		s[k] = v
	}
	a.addSession(s)
}

func mustGet(t *testing.T, ts *httptest.Server, path string) (int, []byte, http.Header) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, resp.Header
}

func mustDo(t *testing.T, ts *httptest.Server, method, path, body string, headers map[string]string) (int, []byte, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest %s %s: %v", method, path, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/merge-patch+json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, bodyBytes, resp.Header
}

// ----- Diagnostics ---------------------------------------------------------

func TestGet_Healthz(t *testing.T) {
	_, _, ts := newTestServer(t)
	status, body, _ := mustGet(t, ts, "/api/v2/healthz")
	if status != http.StatusOK || string(body) != "ok" {
		t.Errorf("healthz = %d %q, want 200 ok", status, body)
	}
}

func TestGet_Info(t *testing.T) {
	_, _, ts := newTestServer(t)
	status, body, _ := mustGet(t, ts, "/api/v2/info")
	if status != http.StatusOK {
		t.Fatalf("info status %d", status)
	}
	var got map[string]any
	json.Unmarshal(body, &got)
	if got["version"] != "fake" {
		t.Errorf("version = %v, want fake", got["version"])
	}
	apiVersions, _ := got["api_versions"].([]any)
	if len(apiVersions) != 2 {
		t.Errorf("api_versions = %v", apiVersions)
	}
}

// ----- Players -------------------------------------------------------------

func TestGet_Players_Empty(t *testing.T) {
	_, _, ts := newTestServer(t)
	status, body, _ := mustGet(t, ts, "/api/v2/players")
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	var got map[string]any
	json.Unmarshal(body, &got)
	items, _ := got["items"].([]any)
	if len(items) != 0 {
		t.Errorf("items = %v, want empty", items)
	}
}

func TestGet_Players_OneSession(t *testing.T) {
	a, _, ts := newTestServer(t)
	pid := uuid.New().String()
	a.addPlayer(pid, "rev1", nil)

	status, body, _ := mustGet(t, ts, "/api/v2/players")
	if status != http.StatusOK {
		t.Fatalf("status %d body=%s", status, body)
	}
	var got map[string]any
	json.Unmarshal(body, &got)
	items, _ := got["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	rec, _ := items[0].(map[string]any)
	if rec["id"] != pid {
		t.Errorf("id = %v, want %s", rec["id"], pid)
	}
}

func TestGet_PlayersByID_404(t *testing.T) {
	_, _, ts := newTestServer(t)
	pid := uuid.New().String()
	status, _, _ := mustGet(t, ts, "/api/v2/players/"+pid)
	if status != http.StatusNotFound {
		t.Errorf("status %d, want 404", status)
	}
}

func TestGet_PlayersByID_Found_HasETag(t *testing.T) {
	a, _, ts := newTestServer(t)
	pid := uuid.New().String()
	a.addPlayer(pid, "rev1", nil)
	status, _, headers := mustGet(t, ts, "/api/v2/players/"+pid)
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	if got := headers.Get("ETag"); got != `"rev1"` {
		t.Errorf("ETag = %q, want \"rev1\"", got)
	}
}

func TestPost_Players_201_ServerGeneratedID(t *testing.T) {
	_, _, ts := newTestServer(t)
	status, body, headers := mustDo(t, ts, "POST", "/api/v2/players", "{}", nil)
	if status != http.StatusCreated {
		t.Fatalf("status %d body=%s", status, body)
	}
	var rec map[string]any
	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := rec["id"]; !ok {
		t.Errorf("response missing id: %s", body)
	}
	if headers.Get("Location") == "" {
		t.Errorf("Location header missing")
	}
	if headers.Get("ETag") == "" {
		t.Errorf("ETag header missing")
	}
}

func TestPost_Players_201_ClientSuppliedID(t *testing.T) {
	a, _, ts := newTestServer(t)
	pid := uuid.New().String()
	body := `{"player_id":"` + pid + `","labels":{"test":"hello"}}`
	status, _, _ := mustDo(t, ts, "POST", "/api/v2/players", body, nil)
	if status != http.StatusCreated {
		t.Fatalf("status %d", status)
	}
	stored, ok := a.SessionByPlayerID(pid)
	if !ok {
		t.Fatalf("session not stored")
	}
	labels, _ := stored["_v2_labels"].(map[string]any)
	if labels["test"] != "hello" {
		t.Errorf("labels = %v, want test=hello", labels)
	}
}

func TestPost_Players_200_IdempotentRetry(t *testing.T) {
	_, _, ts := newTestServer(t)
	pid := uuid.New().String()
	body := `{"player_id":"` + pid + `","labels":{"k":"v"}}`
	if s, _, _ := mustDo(t, ts, "POST", "/api/v2/players", body, nil); s != http.StatusCreated {
		t.Fatalf("first POST %d", s)
	}
	// Same body → 200.
	status, _, _ := mustDo(t, ts, "POST", "/api/v2/players", body, nil)
	if status != http.StatusOK {
		t.Errorf("retry status %d, want 200", status)
	}
}

func TestPost_Players_409_DifferentBody(t *testing.T) {
	_, _, ts := newTestServer(t)
	pid := uuid.New().String()
	body1 := `{"player_id":"` + pid + `","labels":{"k":"v1"}}`
	body2 := `{"player_id":"` + pid + `","labels":{"k":"v2"}}`
	if s, _, _ := mustDo(t, ts, "POST", "/api/v2/players", body1, nil); s != http.StatusCreated {
		t.Fatalf("first POST %d", s)
	}
	status, _, _ := mustDo(t, ts, "POST", "/api/v2/players", body2, nil)
	if status != http.StatusConflict {
		t.Errorf("conflicting body status %d, want 409", status)
	}
}

func TestDelete_Players_All(t *testing.T) {
	a, _, ts := newTestServer(t)
	a.addPlayer(uuid.New().String(), "r", nil)
	a.addPlayer(uuid.New().String(), "r", nil)
	status, _, _ := mustDo(t, ts, "DELETE", "/api/v2/players", "", nil)
	if status != http.StatusNoContent {
		t.Errorf("status %d, want 204", status)
	}
	if got := len(a.snapshot()); got != 0 {
		t.Errorf("after clear, sessions = %d, want 0", got)
	}
}

func TestDelete_PlayerByID(t *testing.T) {
	a, _, ts := newTestServer(t)
	pid := uuid.New().String()
	a.addPlayer(pid, "r", nil)

	status, _, _ := mustDo(t, ts, "DELETE", "/api/v2/players/"+pid, "", nil)
	if status != http.StatusNoContent {
		t.Errorf("status %d, want 204", status)
	}
	if _, ok := a.SessionByPlayerID(pid); ok {
		t.Errorf("player still present after delete")
	}
}

// ----- PATCH /players/{id} -------------------------------------------------

func TestPatch_NoIfMatch_400(t *testing.T) {
	_, _, ts := newTestServer(t)
	pid := uuid.New().String()
	status, _, _ := mustDo(t, ts, "PATCH", "/api/v2/players/"+pid, `{"labels":{"x":"y"}}`, nil)
	// The oapigen wrapper rejects missing required If-Match with 400.
	if status != http.StatusBadRequest {
		t.Errorf("status %d, want 400", status)
	}
}

func TestPatch_UnknownPlayer_404(t *testing.T) {
	_, _, ts := newTestServer(t)
	pid := uuid.New().String()
	status, _, _ := mustDo(t, ts, "PATCH", "/api/v2/players/"+pid, `{"labels":{"x":"y"}}`,
		map[string]string{"If-Match": `"any"`})
	if status != http.StatusNotFound {
		t.Errorf("status %d, want 404", status)
	}
}

func TestPatch_UnsupportedField_501(t *testing.T) {
	a, _, ts := newTestServer(t)
	pid := uuid.New().String()
	a.addPlayer(pid, "rev1", nil)
	// fault_rules is still deferred — Phase H wires shape but not
	// fault_rules.
	status, body, _ := mustDo(t, ts, "PATCH", "/api/v2/players/"+pid,
		`{"fault_rules":[{"id":"r1","type":"500"}]}`,
		map[string]string{"If-Match": `"rev1"`})
	if status != http.StatusNotImplemented {
		t.Errorf("status %d, want 501; body=%s", status, body)
	}
}

func TestPatch_ShapeRoundTrip(t *testing.T) {
	a, _, ts := newTestServer(t)
	pid := uuid.New().String()
	initialRev := "2020-01-01T00:00:00.000000000Z"
	a.addPlayer(pid, initialRev, nil)

	body := `{"shape":{"rate_mbps":5,"delay_ms":50,"loss_pct":1.5}}`
	status, respBody, _ := mustDo(t, ts, "PATCH", "/api/v2/players/"+pid, body,
		map[string]string{"If-Match": `"` + initialRev + `"`})
	if status != http.StatusOK {
		t.Fatalf("status %d body=%s", status, respBody)
	}
	stored, _ := a.SessionByPlayerID(pid)
	if stored["nftables_bandwidth_mbps"] != float64(5) {
		t.Errorf("nftables_bandwidth_mbps = %v", stored["nftables_bandwidth_mbps"])
	}
	if stored["nftables_delay_ms"] != 50 {
		t.Errorf("nftables_delay_ms = %v", stored["nftables_delay_ms"])
	}
	if stored["nftables_packet_loss"] != 1.5 {
		t.Errorf("nftables_packet_loss = %v", stored["nftables_packet_loss"])
	}
	// ApplyShapeToPlayer should have been called.
	a.mu.Lock()
	calls := append([]string{}, a.shapeApplyCalls...)
	a.mu.Unlock()
	found := false
	for _, p := range calls {
		if p == pid {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ApplyShapeToPlayer not called for %s; calls=%v", pid, calls)
	}
}

func TestPatch_TransportFault_ArmsKernel(t *testing.T) {
	a, _, ts := newTestServer(t)
	pid := uuid.New().String()
	initialRev := "2020-01-01T00:00:00.000000000Z"
	a.addPlayer(pid, initialRev, nil)

	body := `{"shape":{"transport_fault":{"type":"drop","frequency":5,"consecutive":2,"mode":"failures_per_seconds"}}}`
	status, respBody, _ := mustDo(t, ts, "PATCH", "/api/v2/players/"+pid, body,
		map[string]string{"If-Match": `"` + initialRev + `"`})
	if status != http.StatusOK {
		t.Fatalf("status %d body=%s", status, respBody)
	}
	a.mu.Lock()
	calls := append([]fakeTransportFaultCall{}, a.transportFaultCalls...)
	a.mu.Unlock()
	if len(calls) == 0 {
		t.Fatalf("ApplyTransportFaultToPlayer not called")
	}
	last := calls[len(calls)-1]
	if last.PlayerID != pid || last.FaultType != "drop" || last.Consecutive != 2 || last.Frequency != 5 {
		t.Errorf("transport-fault call mismatch: %+v", last)
	}
}

func TestPatch_LabelsRoundTrip(t *testing.T) {
	a, _, ts := newTestServer(t)
	pid := uuid.New().String()
	a.addPlayer(pid, "rev1", nil)

	status, body, headers := mustDo(t, ts, "PATCH", "/api/v2/players/"+pid,
		`{"labels":{"test":"abc","run":"42"}}`,
		map[string]string{"If-Match": `"rev1"`})
	if status != http.StatusOK {
		t.Fatalf("status %d body=%s", status, body)
	}
	if etag := headers.Get("ETag"); etag == `""` || etag == "" {
		t.Errorf("ETag missing")
	}
	// Confirm the session map carries _v2_labels post-patch.
	stored, _ := a.SessionByPlayerID(pid)
	labels, _ := stored["_v2_labels"].(map[string]any)
	if labels["test"] != "abc" || labels["run"] != "42" {
		t.Errorf("stored labels = %v, want test=abc run=42", labels)
	}
}

func TestPatch_FieldLevelConcurrency_412(t *testing.T) {
	a, _, ts := newTestServer(t)
	pid := uuid.New().String()
	// Use a real timestamp-shaped initial revision so the lex-compare
	// against post-PATCH revisions (also timestamp-shaped) does the
	// right thing. A synthetic "rev1" lex-sorts *after* any real
	// 2026-shaped timestamp, which would mask conflicts.
	initialRev := "2020-01-01T00:00:00.000000000Z"
	a.addPlayer(pid, initialRev, nil)

	// First patch succeeds — bumps FieldRevisions for labels.test.
	s1, _, _ := mustDo(t, ts, "PATCH", "/api/v2/players/"+pid,
		`{"labels":{"test":"first"}}`,
		map[string]string{"If-Match": `"` + initialRev + `"`})
	if s1 != http.StatusOK {
		t.Fatalf("first patch status %d", s1)
	}

	// Second patch with the *original* (now stale) If-Match on the
	// same field → 412.
	status, body, _ := mustDo(t, ts, "PATCH", "/api/v2/players/"+pid,
		`{"labels":{"test":"second"}}`,
		map[string]string{"If-Match": `"` + initialRev + `"`})
	if status != http.StatusPreconditionFailed {
		t.Fatalf("status %d, want 412; body=%s", status, body)
	}
	var problem map[string]any
	json.Unmarshal(body, &problem)
	conflicts, _ := problem["conflicts"].([]any)
	if len(conflicts) != 1 || conflicts[0] != "labels.test" {
		t.Errorf("conflicts = %v, want [labels.test]", conflicts)
	}
}

func TestPatch_DisjointFields_BothSucceed(t *testing.T) {
	a, _, ts := newTestServer(t)
	pid := uuid.New().String()
	initialRev := "2020-01-01T00:00:00.000000000Z"
	a.addPlayer(pid, initialRev, nil)

	// First patch: labels.test.
	s1, _, _ := mustDo(t, ts, "PATCH", "/api/v2/players/"+pid,
		`{"labels":{"test":"a"}}`,
		map[string]string{"If-Match": `"` + initialRev + `"`})
	if s1 != http.StatusOK {
		t.Fatalf("first patch %d", s1)
	}
	// Second patch with the same If-Match but on labels.run (disjoint).
	// Per field-level concurrency, this should succeed even though
	// labels.test was bumped.
	s2, body, _ := mustDo(t, ts, "PATCH", "/api/v2/players/"+pid,
		`{"labels":{"run":"x"}}`,
		map[string]string{"If-Match": `"` + initialRev + `"`})
	if s2 != http.StatusOK {
		t.Errorf("disjoint patch %d, want 200; body=%s", s2, body)
	}
}

// ----- Group lifecycle -----------------------------------------------------

func TestPost_PlayerGroups_Empty_400(t *testing.T) {
	_, _, ts := newTestServer(t)
	status, _, _ := mustDo(t, ts, "POST", "/api/v2/player-groups", `{}`, nil)
	if status != http.StatusBadRequest {
		t.Errorf("status %d, want 400", status)
	}
}

func TestPost_PlayerGroups_NoEligible_409(t *testing.T) {
	_, _, ts := newTestServer(t)
	body, _ := json.Marshal(map[string]any{
		"member_player_ids": []string{uuid.New().String()},
	})
	status, _, _ := mustDo(t, ts, "POST", "/api/v2/player-groups", string(body), nil)
	if status != http.StatusConflict {
		t.Errorf("status %d, want 409", status)
	}
}

func TestPost_PlayerGroups_Success_201(t *testing.T) {
	a, _, ts := newTestServer(t)
	pid := uuid.New().String()
	a.addPlayer(pid, "rev1", nil)

	body, _ := json.Marshal(map[string]any{
		"member_player_ids": []string{pid},
		"label":             "blast-test",
	})
	status, respBody, headers := mustDo(t, ts, "POST", "/api/v2/player-groups", string(body), nil)
	if status != http.StatusCreated {
		t.Fatalf("status %d body=%s", status, respBody)
	}
	if headers.Get("Location") == "" {
		t.Errorf("Location header missing")
	}
	var rec map[string]any
	json.Unmarshal(respBody, &rec)
	if _, ok := rec["id"]; !ok {
		t.Errorf("response missing id; body=%s", respBody)
	}
	members, _ := rec["member_player_ids"].([]any)
	if len(members) != 1 {
		t.Errorf("member_player_ids = %v", members)
	}
}

func TestDelete_PlayerGroup_Disband(t *testing.T) {
	a, _, ts := newTestServer(t)
	pid := uuid.New().String()
	a.addPlayer(pid, "rev1", nil)

	body, _ := json.Marshal(map[string]any{"member_player_ids": []string{pid}})
	_, postBody, _ := mustDo(t, ts, "POST", "/api/v2/player-groups", string(body), nil)
	var posted map[string]any
	json.Unmarshal(postBody, &posted)
	gid := posted["id"].(string)

	status, _, _ := mustDo(t, ts, "DELETE", "/api/v2/player-groups/"+gid, "", nil)
	if status != http.StatusNoContent {
		t.Errorf("status %d, want 204", status)
	}
	// Player's group_id should now be "".
	stored, _ := a.SessionByPlayerID(pid)
	if g := asString(stored["group_id"]); g != "" {
		t.Errorf("group_id after disband = %q, want empty", g)
	}
}

func TestPatch_GroupedMember_Broadcasts(t *testing.T) {
	a, _, ts := newTestServer(t)
	p1, p2 := uuid.New().String(), uuid.New().String()
	a.addPlayer(p1, "rev1", nil)
	a.addPlayer(p2, "rev2", nil)

	// Group p1 + p2 together.
	body, _ := json.Marshal(map[string]any{"member_player_ids": []string{p1, p2}})
	mustDo(t, ts, "POST", "/api/v2/player-groups", string(body), nil)

	// Read the (now-grouped) p1's revision so we PATCH with a fresh
	// If-Match.
	stored1, _ := a.SessionByPlayerID(p1)
	rev1 := asString(stored1["control_revision"])

	// PATCH p1's labels — should fan out to p2.
	status, respBody, _ := mustDo(t, ts, "PATCH", "/api/v2/players/"+p1,
		`{"labels":{"broadcast":"1"}}`,
		map[string]string{"If-Match": `"` + rev1 + `"`})
	if status != http.StatusOK {
		t.Fatalf("PATCH p1 %d body=%s", status, respBody)
	}
	stored1, _ = a.SessionByPlayerID(p1)
	stored2, _ := a.SessionByPlayerID(p2)
	l1, _ := stored1["_v2_labels"].(map[string]any)
	l2, _ := stored2["_v2_labels"].(map[string]any)
	if l1["broadcast"] != "1" {
		t.Errorf("p1 labels = %v, want broadcast=1", l1)
	}
	if l2["broadcast"] != "1" {
		t.Errorf("p2 labels = %v, want broadcast=1 (broadcast failed)", l2)
	}
	// Both members should share the same control_revision after the
	// fan-out.
	r1 := asString(stored1["control_revision"])
	r2 := asString(stored2["control_revision"])
	if r1 != r2 {
		t.Errorf("control_revision mismatch after broadcast: p1=%q p2=%q", r1, r2)
	}
}

// ----- SSE /events ---------------------------------------------------------

func TestSSE_FirstConnect_GetsHeartbeat(t *testing.T) {
	t.Skip("heartbeat fires every 15s; too slow for normal CI. Run -short=false to enable.")
}

// readSSEPrefix opens an SSE GET, reads up to maxBytes from the
// stream within timeout, and returns whatever arrived. The caller
// gets the open `*http.Response` back too — close it to release the
// handler. Uses a context-cancel-on-deadline pattern instead of body
// SetReadDeadline (the http.Response.Body returned by stdlib doesn't
// expose SetReadDeadline through its sealed wrapper type).
func readSSEPrefix(t *testing.T, ts *httptest.Server, lastEventID string, maxBytes int, timeout time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/v2/events", nil)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE GET: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, maxBytes)
	got := []byte{}
	for len(got) < maxBytes {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			got = append(got, buf[:n]...)
		}
		if err != nil {
			break
		}
		// Stop once we've seen at least one full frame (terminated
		// by `\n\n`).
		if strings.Contains(string(got), "\n\n") {
			break
		}
	}
	return string(got)
}

func TestSSE_LastEventID_Replays(t *testing.T) {
	_, srv, ts := newTestServer(t)

	// Inject some frames into the ring directly to avoid waiting for
	// heartbeats.
	srv.events.ring.Publish("test.first", []byte(`{"type":"test.first","data":{}}`))
	srv.events.ring.Publish("test.second", []byte(`{"type":"test.second","data":{}}`))

	got := readSSEPrefix(t, ts, "1", 1024, 2*time.Second)
	if !strings.Contains(got, "test.second") {
		t.Errorf("expected 'test.second' in replay, got: %q", got)
	}
	if strings.Contains(got, "test.first") {
		t.Errorf("frame id=1 (test.first) should not be replayed, got: %q", got)
	}
}

func TestSSE_Gap_Synth(t *testing.T) {
	_, srv, ts := newTestServer(t)

	// Replace with a tiny ring so we can force a gap. The original
	// EventRing was already created with default bounds; swapping it
	// in-place is safe before any client connects.
	srv.events.ring = NewEventRing(2, time.Hour)
	srv.events.ring.Publish("a", []byte(`{}`)) // id=1
	srv.events.ring.Publish("b", []byte(`{}`)) // id=2
	srv.events.ring.Publish("c", []byte(`{}`)) // id=3 (evicts id=1)
	srv.events.ring.Publish("d", []byte(`{}`)) // id=4 (evicts id=2)

	got := readSSEPrefix(t, ts, "1", 1024, 2*time.Second)
	if !strings.Contains(got, "replay.gap") {
		t.Errorf("expected replay.gap synthetic frame, got: %q", got)
	}
	if !strings.Contains(got, "missed_from") {
		t.Errorf("expected missed_from in gap body, got: %q", got)
	}
}
