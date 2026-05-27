// sb_common_test.go holds helpers shared by the server-behavior control
// surface tests (delay, loss, pattern, fault injection, transport faults,
// transfer timeouts). throughput_calibration_test.go owns the lower-level
// primitives (env*, newClient, httpGet, discoverContent, findSession,
// parseMaster, parseMediaPlaylist, setRateLimit, postMetrics, …); this
// file builds the per-test bootstrap on top of those so each sibling test
// is just "make a probe, sweep one control, print a matrix."
package server_behavior

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// probe is one virtual player bound to one proxy session: a stable
// player_id, the session_id + internal/external ports the proxy
// allocated for it, and the top variant of a discovered content item.
// Every control-surface test starts by allocating one of these so the
// run is visible in the dashboard like any real device.
type probe struct {
	c          *http.Client
	host       string
	apiBase    string // host:apiPort — where /api/* lives (nginx)
	shaperBase string // host:shaperPort — bootstrap port for new sessions
	content    string
	playerID   string
	playID     string
	masterURL  string // final master playlist URL (post-redirect, session port)
	sess       sessionInfo
	top        variant
	variants   []variant // all variants, sorted ascending by bandwidth
}

// newProbe runs the same bootstrap as TestRateSweep: discover content,
// fetch the master on the shaper port (proxy 302s to a per-session port),
// look up the session record, and pick the highest-bitrate variant.
// Connection params reuse the THROUGHPUT_* env vars so one config drives
// every server-behavior test.
func newProbe(t *testing.T) *probe {
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
	playID := uuid.New().String()

	masterURL := fmt.Sprintf(
		"https://%s/go-live/%s/master_6s.m3u8?player_id=%s",
		shaperBase, url.PathEscape(content), url.QueryEscape(playerID),
	)
	masterBody, finalMasterURL, err := httpGet(c, masterURL)
	if err != nil {
		t.Fatalf("master fetch: %v", err)
	}
	sess, err := findSession(c, apiBase, playerID)
	if err != nil {
		t.Fatalf("find session: %v", err)
	}
	variants, err := parseMaster(masterBody, finalMasterURL)
	if err != nil {
		t.Fatalf("parse master: %v", err)
	}
	top := pickTopVariant(variants)

	t.Logf("probe player_id=%s session_id=%s internal_port=%d external_port=%d content=%s top=%.2f Mbps",
		playerID, sess.SessionID, sess.InternalPort, sess.ExternalPort,
		content, float64(top.BandwidthBps)/1e6)

	return &probe{
		c: c, host: host, apiBase: apiBase, shaperBase: shaperBase,
		content: content, playerID: playerID, playID: playID,
		masterURL: finalMasterURL.String(),
		sess:      sess, top: top, variants: variants,
	}
}

// heartbeat posts one dashboard-visible metrics frame for this probe.
func (p *probe) heartbeat(instantMbps, avgMbps, positionS float64, state string) {
	_ = postMetrics(p.c, p.apiBase, p.sess.SessionID, p.playerID, p.playID,
		instantMbps, avgMbps, positionS, state)
}

// setShapeFull POSTs rate + delay + loss in one call to the v1 shape
// endpoint (the same one setRateLimit uses, but exposing delay/loss).
func setShapeFull(c *http.Client, apiBase string, internalPort, rateMbps, delayMs int, lossPct float64) error {
	u := fmt.Sprintf("https://%s/api/nftables/shape/%d", apiBase, internalPort)
	body := fmt.Sprintf(`{"rate_mbps":%d,"delay_ms":%d,"loss_pct":%g}`, rateMbps, delayMs, lossPct)
	req, err := http.NewRequest(http.MethodPost, u, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("setShapeFull %s: %d: %s", u, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// patchSession sends a PATCH /api/session/{id} with {"set": set}. The
// proxy merges every key in `set` into the session map and bumps
// control_revision, so this is the general-purpose way to drive the
// fault / transport / transfer-timeout controls that aren't exposed
// by the dedicated nftables endpoints.
func patchSession(c *http.Client, apiBase, sessionID string, set map[string]any) error {
	body, _ := json.Marshal(map[string]any{"set": set})
	u := fmt.Sprintf("https://%s/api/session/%s", apiBase, url.PathEscape(sessionID))
	req, err := http.NewRequest(http.MethodPatch, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("patchSession %s: %d: %s", u, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// getSessionMap returns the raw /api/sessions record for playerID so a
// test can read computed fields (RTT, pattern step, fault counters).
func getSessionMap(c *http.Client, apiBase, playerID string) (map[string]any, error) {
	body, _, err := httpGet(c, fmt.Sprintf("https://%s/api/sessions", apiBase))
	if err != nil {
		return nil, err
	}
	var list []map[string]any
	if err := json.Unmarshal(body, &list); err != nil {
		var wrapped struct {
			Sessions []map[string]any `json:"sessions"`
		}
		if jerr := json.Unmarshal(body, &wrapped); jerr != nil {
			return nil, err
		}
		list = wrapped.Sessions
	}
	for _, s := range list {
		if pid, _ := s["player_id"].(string); strings.EqualFold(pid, playerID) {
			return s, nil
		}
	}
	return nil, fmt.Errorf("no session for player_id %s", playerID)
}

// mapFloat reads a numeric session-map field as float64, tolerating the
// JSON-number / string / int variants the session map can carry.
func mapFloat(m map[string]any, key string) (float64, bool) {
	if m == nil {
		return 0, false
	}
	switch v := m[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	}
	return 0, false
}

// rawStatus performs a GET and returns the final status code + body
// length WITHOUT treating 4xx/5xx as a transport error — the opposite
// of httpGet. Fault-injection tests need the status of an injected
// failure, not an error. Only the status code matters, so the body is
// drained up to a small cap (a 200 here is a full multi-MB segment —
// reading all of it across hundreds of samples would pull gigabytes);
// the connection is closed rather than reused past the cap.
func rawStatus(c *http.Client, u string) (int, int64, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("User-Agent", "server-behavior-probe/1.0")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := c.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	n, _ := io.CopyN(io.Discard, resp.Body, 64*1024)
	return resp.StatusCode, n, nil
}

// boundedTouch fetches at most maxBytes of a URL with a hard per-request
// deadline, then discards the body. It's for generating a little live
// traffic (so TCP_INFO RTT stays fresh) WITHOUT pulling a whole multi-MB
// segment — a full-segment pull stalls past the test deadline once the
// proxy is adding latency or loss. Range-requests the first slice;
// tolerates a 200 (full) response by capping the read at maxBytes.
// Errors (including deadline) are returned so callers can choose to
// refetch the playlist, but they never block the sweep.
func boundedTouch(c *http.Client, u string, maxBytes int64, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", maxBytes-1))
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := io.CopyN(io.Discard, resp.Body, maxBytes); err != nil && err != io.EOF {
		return err
	}
	return nil
}

// getBytes does a GET and returns the full body + status, without treating
// 4xx/5xx as an error (content-manipulation tests need to compare served
// bytes, including from a "corrupted" 200 response).
func getBytes(c *http.Client, u string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", "server-behavior-probe/1.0")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

// rangeGet fetches at most maxBytes of u via a Range request with a hard
// deadline, discards the body, and returns how many bytes were read. Like
// boundedTouch but reports the byte count (the rate sweep needs it to
// compute throughput). Tolerates a 200 (full) response by capping the read.
func rangeGet(c *http.Client, u string, maxBytes int64, timeout time.Duration) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", maxBytes-1))
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		io.Copy(io.Discard, resp.Body)
		return 0, fmt.Errorf("rangeGet %s: %d", u, resp.StatusCode)
	}
	n, err := io.CopyN(io.Discard, resp.Body, maxBytes)
	if err == io.EOF {
		err = nil
	}
	return n, err
}

// parseFloatList parses "0,1,5,10" → [0 1 5 10]. Mirrors parseRates but
// for the fractional loss/delay sweeps.
func parseFloatList(csv string) ([]float64, error) {
	parts := strings.Split(csv, ",")
	out := make([]float64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		f, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return nil, fmt.Errorf("value %q: %w", p, err)
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no values parsed from %q", csv)
	}
	return out, nil
}

// serverRunID groups every server_* test in one `go test` invocation under
// a single run on the Automated Testing page (the page groups by run_id).
// Override with SERVER_RUN_ID to fold a CLI run into an existing run.
var serverRunID = env("SERVER_RUN_ID", time.Now().UTC().Format("20060102T150405Z"))

// serverMatrix is the generic, test-type-agnostic results table the
// Automated Testing page's detail view renders for platform=server runs
// (delay/loss/fault/etc. don't have ABR ramp steps, so they ship a plain
// labeled table instead).
type serverMatrix struct {
	Title   string     `json:"title"`
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
}

// postServerReport uploads a characterization-run row so a server_* test
// surfaces on the Automated Testing page under platform=server with a
// drillable detail view. Mirrors the player runner's path (POST to the
// forwarder's /api/v2/characterization-runs), carrying the fields the
// forwarder indexes (player_id, play_ids, started_at, ended_at,
// summary.total_stalls) plus the generic matrix for the detail view.
// Non-fatal: a failed upload logs but never fails the test.
func (p *probe) postServerReport(t *testing.T, testName, headline string, startedAt time.Time, passed bool, m serverMatrix) {
	t.Helper()
	stalls := 0
	if !passed {
		stalls = 1 // forwarder derives passed = (total_stalls == 0)
	}
	report := map[string]any{
		"mode":       testName,
		"platform":   "server",
		"player_id":  p.playerID,
		"play_ids":   []string{p.playID},
		"started_at": startedAt.UTC(),
		"ended_at":   time.Now().UTC(),
		"summary": map[string]any{
			"total_stalls": stalls,
			"headline":     headline,
		},
		"server_matrix": m,
	}
	body, _ := json.Marshal(map[string]any{
		"run_id":    serverRunID,
		"test_name": testName,
		"platform":  "server",
		"report":    report,
	})
	u := fmt.Sprintf("https://%s/analytics/api/v2/characterization-runs", p.apiBase)
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		t.Logf("server report build: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.c.Do(req)
	if err != nil {
		t.Logf("server report upload: %v", err)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 400 {
		t.Logf("server report upload %s: %d: %s", testName, resp.StatusCode, strings.TrimSpace(string(respBody)))
		return
	}
	t.Logf("server report posted: run_id=%s test=%s", serverRunID, testName)
}

// settleKernel is the standard pause after a shape/fault PATCH so the
// kernel applies the rule + the tc/nftables verifier settles before we
// start measuring. Matches the 1.5s used by TestRateSweep.
const settleKernel = 1500 * time.Millisecond
