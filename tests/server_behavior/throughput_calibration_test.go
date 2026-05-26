// Package server_behavior hosts tests that verify the proxy server's
// implementation honours its declared behaviour for each operator
// control surface (rate cap, delay, loss, patterns, fault injection,
// timeouts, …). Each test sets a control to a known value and
// measures whether the server actually delivers what it claimed.
//
// throughput_calibration_test.go is the first such test: an HLS-aware
// bandwidth probe that pulls
// video segments through go-proxy as fast as it can across a sweep of
// configured rate caps. Output is a calibration matrix: configured cap
// vs observed avg/peak Mbps. Useful for verifying that the proxy's
// nftables/tc rate-limiter is actually enforcing what it claims at
// every interesting bitrate, and for separating "kernel cap accuracy"
// from "iOS player measurement quirks" by removing the player-side
// idle gap between segments (issue #480).
//
// End-to-end flow (mirrors what the web / iOS clients do):
//
//   1. Generate a stable player_id (UUID) so every dashboard chart
//      on the session shows ONE player across the whole sweep.
//   2. Discover content — env override or first entry from /api/content.
//   3. Derive the bootstrap shaper port from the UI port using the
//      same rule the dashboard's normalizeStreamUrl uses:
//      uiPort[:-3] + "081"  (e.g. 21000 → 21081, 30000 → 30081).
//   4. GET master_6s.m3u8 on the shaper port with ?player_id=X. The
//      proxy 302-redirects to the session-bound port the proxy
//      allocates for that player (e.g. 21181); Go's http.Client
//      follows the redirect by default. The final response URL tells
//      us the per-session port for subsequent fetches.
//   5. Look up the session_id by player_id via /api/sessions (so
//      later /api/session/{id}/metrics POSTs can target our row).
//   6. Parse the master playlist → pick the highest-bandwidth variant.
//   7. For each rate in the sweep:
//        - PATCH the session's INTERNAL port via
//          POST /api/nftables/shape/{port} {rate_mbps,delay_ms,loss_pct}.
//          rate_mbps=0 means "no operator override" — the proxy falls
//          back to the deployment baseline (INFINITE_STREAM_DEFAULT_RATE_MBPS).
//        - Wait briefly for the kernel to apply.
//        - Refetch variant playlist; pull every segment back-to-back
//          for DURATION_PER_RATE seconds.
//        - Every ~1s, POST a heartbeat-shaped metrics event with
//          player_metrics_network_bitrate_mbps (last segment) +
//          player_metrics_avg_network_bitrate_mbps (EWMA window).
//          This is what makes the puller visible in the dashboard's
//          bitrate chart so the operator can watch the sweep live.
//   8. Print a calibration matrix.
//
// Configuration is via env vars only; defaults target test-dev.
//
//   THROUGHPUT_HOST=jonathanoliver-ubuntu.local
//   THROUGHPUT_API_PORT=21000           UI / API port; bootstrap is uiPort[:-3]+"081"
//   THROUGHPUT_CONTENT=...              omit to auto-pick from /api/content
//   THROUGHPUT_DURATION_S=15
//   THROUGHPUT_RATES=1,2,5,10,20,50,100,0   0 = baseline
//   THROUGHPUT_INSECURE=1               skip TLS verify (default 1 for test-dev)
//
// Skipped in short mode. Run:
//
//   cd tests/throughput && go test -v -run TestRateSweep -timeout 5m
//
// Sibling: tests/hls_speed_probe.py is a one-shot Python probe.
package server_behavior

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

const (
	defaultHost          = "jonathanoliver-ubuntu.local"
	defaultAPIPort       = "21000"
	defaultDurationS     = 15
	defaultRatesString   = "1,2,5,10,20,50,100,0"
	defaultInsecure      = true
	heartbeatPeriod      = 1 * time.Second
)

// shaperPortFromUI implements the same derivation as the dashboard's
// normalizeStreamUrl: replace the last 3 digits of the UI port with
// "081". 21000 → 21081, 30000 → 30081. Bootstrap port for new
// sessions; the proxy redirects from here to a per-session port.
func shaperPortFromUI(uiPort string) string {
	if len(uiPort) < 4 {
		return uiPort
	}
	return uiPort[:len(uiPort)-3] + "081"
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
func envInt(key string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
func envBool(key string, fallback bool) bool {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return fallback
}

func newClient(insecure bool) *http.Client {
	return &http.Client{
		// 10 min ceiling — at 1 Mbps a 14MB 2160p segment takes ~112s
		// to pull, and we want every "must complete at least 1
		// segment" guarantee to land naturally rather than time out.
		Timeout: 10 * time.Minute,
		Transport: &http.Transport{
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: insecure},
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       2 * time.Minute,
			ResponseHeaderTimeout: 30 * time.Second,
		},
		// Default redirect policy follows 302s and preserves the query
		// string — exactly what we need for the master fetch's
		// player_id round-trip to the per-session port.
	}
}

// adaptiveDuration returns the per-rate pull window. baseDur is the
// floor (env-configured); at low rates we extend it so the test pulls
// at least one full segment + change. Formula: 1.5× the time it
// takes to pull one segment at the configured rate. At rate=0
// (baseline) we use baseDur as-is — kernel cap matches whatever the
// baseline is, which is typically large enough that baseDur covers it.
func adaptiveDuration(baseDurS, rateMbps int, segBytesEst int64) time.Duration {
	base := time.Duration(baseDurS) * time.Second
	if rateMbps <= 0 || segBytesEst <= 0 {
		return base
	}
	segSec := float64(segBytesEst) * 8 / 1e6 / float64(rateMbps)
	needed := time.Duration(segSec*1.5*1000) * time.Millisecond
	if needed > base {
		return needed
	}
	return base
}

// httpGet performs a GET and returns body bytes + the FINAL response URL
// (after any redirects). Caller uses the final URL as the base for
// resolving relative variant/segment paths.
func httpGet(c *http.Client, u string) ([]byte, *url.URL, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "throughput-probe/1.0")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := c.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, nil, fmt.Errorf("GET %s: %d: %s", u, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	return body, resp.Request.URL, nil
}

// --- content discovery ---------------------------------------------------

// discoverContent picks an iso-fresh content name. Honors the
// THROUGHPUT_CONTENT env var if set; otherwise picks the first
// entry returned by GET /api/content. /api/content is the dashboard's
// content catalog (served by go-upload via nginx). Errors if no
// content is available — there's no sensible hardcoded default.
func discoverContent(c *http.Client, apiBase string) (string, error) {
	if v := strings.TrimSpace(os.Getenv("THROUGHPUT_CONTENT")); v != "" {
		return v, nil
	}
	body, _, err := httpGet(c, fmt.Sprintf("https://%s/api/content", apiBase))
	if err != nil {
		return "", fmt.Errorf("/api/content: %w", err)
	}
	// /api/content shape: tolerate either []string, []{"name":...},
	// or {"content":[...]}. Pick the first non-empty name.
	var asObjs []map[string]any
	if err := json.Unmarshal(body, &asObjs); err == nil {
		for _, e := range asObjs {
			if name, _ := e["name"].(string); name != "" {
				return name, nil
			}
			if name, _ := e["id"].(string); name != "" {
				return name, nil
			}
		}
	}
	var asStrs []string
	if err := json.Unmarshal(body, &asStrs); err == nil && len(asStrs) > 0 {
		for _, s := range asStrs {
			if s != "" {
				return s, nil
			}
		}
	}
	var wrapped struct {
		Content []map[string]any `json:"content"`
		Items   []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil {
		for _, e := range append(wrapped.Content, wrapped.Items...) {
			if name, _ := e["name"].(string); name != "" {
				return name, nil
			}
		}
	}
	return "", fmt.Errorf("no content found in /api/content response (set THROUGHPUT_CONTENT to override)")
}

// --- session lookup -------------------------------------------------------

// sessionInfo holds the fields the probe needs from /api/sessions.
type sessionInfo struct {
	SessionID     string
	InternalPort  int // e.g. 30181 — used for /api/nftables/shape/{port}
	ExternalPort  int // e.g. 21181 — what we already fetched the redirect to
}

// findSession returns the proxy's session record for `playerID`. Polls
// briefly because /api/sessions can lag the master-fetch redirect by a
// few hundred ms on first allocation.
func findSession(c *http.Client, apiBase, playerID string) (sessionInfo, error) {
	for i := 0; i < 10; i++ {
		body, _, err := httpGet(c, fmt.Sprintf("https://%s/api/sessions", apiBase))
		if err != nil {
			return sessionInfo{}, err
		}
		var list []map[string]any
		if err := json.Unmarshal(body, &list); err != nil {
			var wrapped struct {
				Sessions []map[string]any `json:"sessions"`
			}
			if jerr := json.Unmarshal(body, &wrapped); jerr == nil {
				list = wrapped.Sessions
			} else {
				return sessionInfo{}, err
			}
		}
		for _, s := range list {
			pid, _ := s["player_id"].(string)
			if !strings.EqualFold(pid, playerID) {
				continue
			}
			info := sessionInfo{}
			info.SessionID, _ = s["session_id"].(string)
			if info.SessionID == "" {
				info.SessionID, _ = s["session_number"].(string)
			}
			if portStr, _ := s["x_forwarded_port"].(string); portStr != "" {
				info.InternalPort, _ = strconv.Atoi(portStr)
			}
			if extStr, _ := s["x_forwarded_port_external"].(string); extStr != "" {
				info.ExternalPort, _ = strconv.Atoi(extStr)
			}
			if info.SessionID != "" && info.InternalPort > 0 {
				return info, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return sessionInfo{}, fmt.Errorf("session for %s not visible in /api/sessions after 1s", playerID)
}

// --- playlist parsing -----------------------------------------------------

type variant struct {
	BandwidthBps int
	URL          string // resolved absolute
}

func parseMaster(body []byte, base *url.URL) ([]variant, error) {
	var vs []variant
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var pending *variant
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			pending = &variant{}
			for _, kv := range strings.Split(line[len("#EXT-X-STREAM-INF:"):], ",") {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					continue
				}
				if strings.EqualFold(strings.TrimSpace(k), "BANDWIDTH") {
					n, err := strconv.Atoi(strings.TrimSpace(v))
					if err == nil {
						pending.BandwidthBps = n
					}
				}
			}
			continue
		}
		if pending != nil && line != "" && !strings.HasPrefix(line, "#") {
			ref, err := base.Parse(line)
			if err != nil {
				pending = nil
				continue
			}
			pending.URL = ref.String()
			vs = append(vs, *pending)
			pending = nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(vs) == 0 {
		return nil, fmt.Errorf("no #EXT-X-STREAM-INF variants found in master")
	}
	return vs, nil
}

func pickTopVariant(vs []variant) variant {
	sort.Slice(vs, func(i, j int) bool { return vs[i].BandwidthBps > vs[j].BandwidthBps })
	return vs[0]
}

// parseMediaPlaylist extracts every segment URL (resolved against base).
// Ignores partial-segment hints (#EXT-X-PART) — this probe measures
// bulk-segment throughput, not LL-HLS latency.
func parseMediaPlaylist(body []byte, base *url.URL) ([]string, error) {
	var segs []string
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ref, err := base.Parse(line)
		if err != nil {
			continue
		}
		segs = append(segs, ref.String())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return segs, nil
}

// --- mutations -----------------------------------------------------------

// setRateLimit POSTs to /api/nftables/shape/{internalPort} on the API
// base. internalPort is the docker-container-side port (e.g. 30181);
// /api/sessions exposes it as `x_forwarded_port`.
func setRateLimit(c *http.Client, apiBase string, internalPort int, rateMbps int) error {
	u := fmt.Sprintf("https://%s/api/nftables/shape/%d", apiBase, internalPort)
	body := fmt.Sprintf(`{"rate_mbps":%d,"delay_ms":0,"loss_pct":0}`, rateMbps)
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
		return fmt.Errorf("setRateLimit %s: %d: %s", u, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// postMetrics writes a player_metrics_* heartbeat to the session so
// the dashboard's bitrate chart picks up the virtual player. Mirrors
// the field shape iOS sends so the chart accessors find the same keys.
func postMetrics(
	c *http.Client, apiBase, sessionID, playerID, playID string,
	netInstantMbps, netAvgMbps float64, positionS float64, state string,
) error {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	set := map[string]any{
		"player_id":                                 playerID,
		"player_metrics_state":                      state,
		"player_metrics_last_event":                 "heartbeat",
		"player_metrics_trigger_type":               "heartbeat",
		"player_metrics_event_time":                 now,
		"player_metrics_position_s":                 positionS,
		"player_metrics_playback_rate":              1.0,
		"player_metrics_source":                     "throughput-probe",
		"player_metrics_network_bitrate_mbps":       round2(netInstantMbps),
		"player_metrics_avg_network_bitrate_mbps":   round2(netAvgMbps),
		"player_metrics_buffer_depth_s":             0,
		"player_metrics_dropped_frames":             0,
		"player_metrics_stalls":                     0,
	}
	body, _ := json.Marshal(map[string]any{"set": set})
	u := fmt.Sprintf(
		"https://%s/api/session/%s/metrics?play_id=%s&attempt_id=1",
		apiBase, url.PathEscape(sessionID), url.QueryEscape(playID),
	)
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("postMetrics: %d", resp.StatusCode)
	}
	return nil
}

func round2(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*100) / 100
}

// --- the sweep ------------------------------------------------------------

type rateResult struct {
	configuredMbps int
	avgMbps        float64
	peakMbps       float64
	totalBytes     int64
	durationS      float64
	segments       int
}

func TestRateSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("throughput sweep skipped in short mode")
	}
	host := env("THROUGHPUT_HOST", defaultHost)
	apiPort := env("THROUGHPUT_API_PORT", defaultAPIPort)
	shaperPort := shaperPortFromUI(apiPort)
	durationS := envInt("THROUGHPUT_DURATION_S", defaultDurationS)
	ratesCsv := env("THROUGHPUT_RATES", defaultRatesString)
	insecure := envBool("THROUGHPUT_INSECURE", defaultInsecure)

	rates, err := parseRates(ratesCsv)
	if err != nil {
		t.Fatalf("parse rates: %v", err)
	}

	apiBase := host + ":" + apiPort
	shaperBase := host + ":" + shaperPort
	c := newClient(insecure)

	content, err := discoverContent(c, apiBase)
	if err != nil {
		t.Fatalf("discover content: %v", err)
	}

	playerID := uuid.New().String()
	playID := uuid.New().String()

	t.Logf("probe player_id=%s api=https://%s shaper=https://%s content=%s rates=%v dur_per_rate=%ds",
		playerID, apiBase, shaperBase, content, rates, durationS)

	// --- 1. Master fetch on the shaper bootstrap port. The proxy
	// allocates a session and 302-redirects to the per-session port;
	// Go's http.Client follows by default and preserves the query
	// string (player_id) across the hop.
	masterURL := fmt.Sprintf(
		"https://%s/go-live/%s/master_6s.m3u8?player_id=%s",
		shaperBase, url.PathEscape(content), url.QueryEscape(playerID),
	)
	masterBody, finalMasterURL, err := httpGet(c, masterURL)
	if err != nil {
		t.Fatalf("master fetch: %v", err)
	}
	t.Logf("master fetched; final URL (post-redirect): %s", finalMasterURL)

	// --- 2. Look up the session_id + internal port for this player.
	sess, err := findSession(c, apiBase, playerID)
	if err != nil {
		t.Fatalf("find session: %v", err)
	}
	t.Logf("session_id=%s internal_port=%d external_port=%d",
		sess.SessionID, sess.InternalPort, sess.ExternalPort)

	// --- 3. Parse master, pick top variant.
	variants, err := parseMaster(masterBody, finalMasterURL)
	if err != nil {
		t.Fatalf("parse master: %v", err)
	}
	top := pickTopVariant(variants)
	t.Logf("top variant: %.2f Mbps at %s",
		float64(top.BandwidthBps)/1e6, top.URL)

	// --- 4. Sweep rates.
	// Estimate per-segment size from the top variant's BANDWIDTH * 6s
	// (assumed segment duration). At lower rates one segment can take
	// minutes — extend the per-rate window so we always sample at
	// least one full segment + change. baseDur is the floor.
	segBytesEst := int64(top.BandwidthBps) * 6 / 8
	t.Logf("estimated top-variant segment: %.1f MB", float64(segBytesEst)/(1<<20))
	results := make([]rateResult, 0, len(rates))
	for _, rateMbps := range rates {
		label := fmt.Sprintf("%d Mbps", rateMbps)
		if rateMbps == 0 {
			label = "0 (baseline)"
		}
		dur := adaptiveDuration(durationS, rateMbps, segBytesEst)
		t.Logf("\n=== rate cap %s — pulling for %s ===", label, dur)
		if err := setRateLimit(c, apiBase, sess.InternalPort, rateMbps); err != nil {
			t.Errorf("set rate %d: %v", rateMbps, err)
			continue
		}
		// Wait for the kernel to apply + tc verifier to settle.
		time.Sleep(1500 * time.Millisecond)
		res := runPullWindow(t, c, apiBase, sess.SessionID, playerID, playID,
			top.URL, dur)
		res.configuredMbps = rateMbps
		results = append(results, res)
		t.Logf("rate=%-12s avg=%.2f Mbps peak=%.2f Mbps segs=%d total=%dB",
			label, res.avgMbps, res.peakMbps, res.segments, res.totalBytes)
	}

	// Clear the override at the end so we don't leave the session
	// pinned to whatever the last sweep value was.
	_ = setRateLimit(c, apiBase, sess.InternalPort, 0)

	// --- 5. Summary matrix.
	printMatrix(t, results)
}

func parseRates(csv string) ([]int, error) {
	parts := strings.Split(csv, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("rate %q: %w", p, err)
		}
		if n < 0 {
			return nil, fmt.Errorf("rate %d: negative", n)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no rates parsed from %q", csv)
	}
	return out, nil
}

// runPullWindow pulls segments back-to-back for `dur`, posting a
// heartbeat every ~1s with the running network bitrate. Returns
// aggregate stats.
//
// Pull strategy: refetch the variant playlist on each pass, pull every
// segment listed, then loop. This is "consume as fast as you can
// sustainably" — not "stress the proxy with parallel requests."
// Removes the player-side idle gap between segments that an HLS player
// would naturally have, which is the variable we're trying to isolate.
func runPullWindow(
	t *testing.T, c *http.Client, apiBase, sessionID, playerID, playID string,
	variantURL string, dur time.Duration,
) rateResult {
	t.Helper()
	start := time.Now()
	deadline := start.Add(dur)
	var totalBytes int64
	var segCount int
	var ewmaMbps atomic.Uint64    // float64 bits — written via atomic
	var lastInstantMbps atomic.Uint64
	peak := 0.0

	// Heartbeat goroutine — POSTs metrics every heartbeatPeriod so the
	// dashboard's bitrate chart picks us up as a virtual player.
	stopHB := make(chan struct{})
	doneHB := make(chan struct{})
	go func() {
		defer close(doneHB)
		ticker := time.NewTicker(heartbeatPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-stopHB:
				return
			case <-ticker.C:
				instant := math.Float64frombits(lastInstantMbps.Load())
				avg := math.Float64frombits(ewmaMbps.Load())
				pos := time.Since(start).Seconds()
				_ = postMetrics(c, apiBase, sessionID, playerID, playID,
					instant, avg, pos, "playing")
			}
		}
	}()

	// Loop until deadline passes AND we've completed ≥1 full segment.
	// The "≥1 segment" guarantee matters at low rates where the
	// deadline may fire mid-fetch on the first segment — without
	// this we'd report 0 segments / 0 Mbps and learn nothing about
	// the cap's accuracy. The in-flight fetch always blocks to
	// completion (no mid-segment cancel), so the worst-case overrun
	// is `segBytes * 8 / rate` seconds past the deadline.
	for time.Now().Before(deadline) || segCount == 0 {
		variantBody, variantFinal, err := httpGet(c, variantURL)
		if err != nil {
			t.Logf("variant playlist fetch failed: %v (retrying)", err)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		segs, err := parseMediaPlaylist(variantBody, variantFinal)
		if err != nil || len(segs) == 0 {
			t.Logf("variant playlist parse: %v segs=%d", err, len(segs))
			time.Sleep(200 * time.Millisecond)
			continue
		}
		for _, segURL := range segs {
			// Continue past the deadline only if we haven't completed
			// a segment yet (the ≥1 segment guarantee).
			if !time.Now().Before(deadline) && segCount > 0 {
				break
			}
			t0 := time.Now()
			body, _, err := httpGet(c, segURL)
			if err != nil {
				t.Logf("segment fetch failed: %v", err)
				continue
			}
			dt := time.Since(t0).Seconds()
			n := len(body)
			totalBytes += int64(n)
			segCount++
			if dt > 0 {
				instant := float64(n) * 8 / 1e6 / dt
				if instant > peak {
					peak = instant
				}
				prev := math.Float64frombits(ewmaMbps.Load())
				next := prev*0.7 + instant*0.3
				if prev == 0 {
					next = instant
				}
				ewmaMbps.Store(math.Float64bits(next))
				lastInstantMbps.Store(math.Float64bits(instant))
			}
		}
		// Brief pause between playlist refetches so the live edge
		// advances and we get fresh segments rather than re-pulling
		// the same window forever.
		time.Sleep(50 * time.Millisecond)
	}

	close(stopHB)
	<-doneHB

	elapsed := time.Since(start).Seconds()
	avg := 0.0
	if elapsed > 0 {
		avg = float64(totalBytes) * 8 / 1e6 / elapsed
	}
	return rateResult{
		avgMbps:    avg,
		peakMbps:   peak,
		totalBytes: totalBytes,
		durationS:  elapsed,
		segments:   segCount,
	}
}

func printMatrix(t *testing.T, results []rateResult) {
	t.Logf("\n=== calibration matrix ===")
	t.Logf("%-14s %-12s %-12s %-12s %-10s %-10s",
		"configured", "obs_avg", "obs_peak", "delta_avg", "accuracy", "segs")
	for _, r := range results {
		var deltaStr, accStr, capStr string
		cap := r.configuredMbps
		if cap == 0 {
			capStr = "0 (baseline)"
			deltaStr = "—"
			accStr = "—"
		} else {
			delta := r.avgMbps - float64(cap)
			deltaStr = fmt.Sprintf("%+.2f", delta)
			accStr = fmt.Sprintf("%.0f%%", math.Min(r.avgMbps, float64(cap))/float64(cap)*100)
			capStr = fmt.Sprintf("%d Mbps", cap)
		}
		t.Logf("%-14s %-12.2f %-12.2f %-12s %-10s %-10d",
			capStr, r.avgMbps, r.peakMbps, deltaStr, accStr, r.segments)
	}
}
