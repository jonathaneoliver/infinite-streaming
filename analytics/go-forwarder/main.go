// Subscribes to go-proxy's session SSE stream and forwards each changed
// session snapshot to ClickHouse. Standalone, no dependency on go-proxy
// internals; if this binary dies the live UI keeps working — we just
// stop archiving until it restarts.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jonathaneoliver/infinite-streaming/go-proxy/pkg/v2translate"
)

// init: relax TLS verification on every outgoing request. The
// forwarder only talks to go-server (Docker-internal) and ClickHouse,
// both reachable on private interfaces with self-signed certs once
// TS11 flipped go-proxy to TLS. Verification would require shipping
// the in-container CA into the forwarder image; trusting self-signed
// for these internal hops is safe and matches `proxy_ssl_verify off`
// on the nginx side.
func init() {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{}
		}
		transport.TLSClientConfig.InsecureSkipVerify = true
	}
}

// canonicalV2ID normalises a raw v1 identifier (player_id / play_id) to
// the same canonical v2 form `v2translate` produces on read: parse as
// UUID when possible, fall back to the stable v5 derivation otherwise.
// Empty input → empty output (don't synthesise an id for missing data).
//
// CRITICAL — every forwarder ingest path that writes a row keyed by
// player_id / play_id MUST run those ids through THIS function before
// the INSERT. ClickHouse string comparisons are case-sensitive; iOS
// emits uppercase UUIDs ("DC08D893-…"); the v3 dashboard canonicalises
// to lowercase for every WHERE clause. Skipping this step lands rows in
// CH that look fine in a `SELECT *`, but the dashboard filter silently
// matches zero — there's no error surface, just empty panels.
//
// Issue history:
//   - First seen pre-#474: snapshots stored as raw short-form
//     "427a6bf3" while reads produced the v5-derived UUID — Focus
//     Window came up empty for any non-UUID player.
//   - Issue #474 control_events repeat: the new ingest path skipped
//     this step, control_events.player_id was uppercase, PlayLog's
//     Control bucket stayed empty until canonicalV2ID was wired into
//     entryToCtrlRow.
//
// Future ingest paths: a quick sanity check is `SELECT DISTINCT
// player_id FROM your_new_table` next to the same query against
// session_events — they should agree on case.
func canonicalV2ID(raw string) string {
	if raw == "" {
		return ""
	}
	if u, err := uuid.Parse(raw); err == nil {
		return u.String()
	}
	return v2translate.PlayerUUIDForRawID(raw).String()
}

// canonicalIDsFor returns the canonical player_id and play_id strings a
// v3 client will use to look this row up. Routes through
// v2translate.PlayerFromSession so the forwarder's stored ids stay in
// lockstep with what `/api/v2/players` emits — including the fallback
// play_id derivation for sessions whose raw v1 payload doesn't carry
// `play_id` (web / hls.js / non-Apple devices).
//
// Returns empty strings when the session can't be projected (no
// player_id at all); the caller still inserts the row but the play_id
// column will be blank and the client won't be able to filter to that
// row by play_id.
func canonicalIDsFor(s map[string]interface{}) (string, string) {
	rec, ok := v2translate.PlayerFromSession(s)
	if !ok {
		return canonicalV2ID(getStr(s, "player_id")), canonicalV2ID(getStr(s, "play_id"))
	}
	playerID := rec.Id.String()
	playID := ""
	if rec.CurrentPlay != nil {
		playID = rec.CurrentPlay.Id.String()
	}
	return playerID, playID
}

type config struct {
	sseURL         string
	clickhouseURL  string
	chDatabase     string
	chTable        string
	chUser         string
	chPassword     string
	flushEvery     time.Duration
	flushBatch     int
	httpListen     string
	ringWindowSecs int // FORWARDER_LIVE_RING_SECONDS; see ring.go

	// AI chat backend (#497). All optional — leave llmProfilesPath
	// empty to disable the /api/v2/chat endpoint entirely.
	llmProfilesPath    string  // FORWARDER_LLM_PROFILES_PATH
	llmPromptPath      string  // FORWARDER_LLM_PROMPT_PATH (system prompt markdown)
	llmReaderUser      string  // FORWARDER_CLICKHOUSE_LLM_USER (default "llm_reader")
	llmReaderPassword  string  // FORWARDER_CLICKHOUSE_LLM_PASSWORD
	llmBudgetUSD       float64 // FORWARDER_LLM_BUDGET_USD (default 5.00)
	llmMaxToolCalls    int     // FORWARDER_LLM_MAX_TOOL_CALLS (default 20)
	llmMaxInputTokens  int     // FORWARDER_LLM_MAX_INPUT_TOKENS (default 80000)
	claudeDir          string  // FORWARDER_CLAUDE_DIR — mount of the project's .claude/ dir
}

func loadConfig() config {
	c := config{
		sseURL:        getenv("FORWARDER_SSE_URL", "http://go-server:30081/api/sessions/stream"),
		clickhouseURL: getenv("FORWARDER_CLICKHOUSE_URL", "http://clickhouse:8123"),
		chDatabase:    getenv("FORWARDER_CLICKHOUSE_DB", "infinite_streaming"),
		chTable:       getenv("FORWARDER_CLICKHOUSE_TABLE", "session_events"),
		chUser:        getenv("FORWARDER_CLICKHOUSE_USER", "default"),
		chPassword:    getenv("FORWARDER_CLICKHOUSE_PASSWORD", ""),
		// flushEvery is the upper bound on per-row visibility lag — the
		// inserter empties whichever happens first (timer or batch
		// fills). 250ms keeps the picker's "x seconds ago" honest
		// without significantly raising ClickHouse insert pressure.
		flushEvery:    250 * time.Millisecond,
		flushBatch:    500,
		httpListen:    getenv("FORWARDER_HTTP_LISTEN", ":8080"),
		// ring window default 600s (10 min) covers the dashboard's
		// default focus window so most v3 queries skip ClickHouse
		// entirely. Tunable down for memory-constrained deployments
		// or up for "live tail covers more history" use cases.
		ringWindowSecs: getenvInt("FORWARDER_LIVE_RING_SECONDS", 600),

		// AI chat backend. Empty llmProfilesPath disables the
		// endpoint entirely; non-empty turns it on with the catalog
		// loaded at startup.
		llmProfilesPath:   getenv("FORWARDER_LLM_PROFILES_PATH", ""),
		llmPromptPath:     getenv("FORWARDER_LLM_PROMPT_PATH", ""),
		llmReaderUser:     getenv("FORWARDER_CLICKHOUSE_LLM_USER", "llm_reader"),
		llmReaderPassword: getenv("FORWARDER_CLICKHOUSE_LLM_PASSWORD", ""),
		llmBudgetUSD:      getenvFloat("FORWARDER_LLM_BUDGET_USD", 5.00),
		llmMaxToolCalls:   getenvInt("FORWARDER_LLM_MAX_TOOL_CALLS", 20),
		llmMaxInputTokens: getenvInt("FORWARDER_LLM_MAX_INPUT_TOKENS", 80000),
		claudeDir:         getenv("FORWARDER_CLAUDE_DIR", ""),
	}
	return c
}

func getenvFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		var f float64
		if _, err := fmt.Sscanf(v, "%f", &f); err == nil {
			return f
		}
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// row mirrors the ClickHouse session_snapshots column set. Field names
// are the JSONEachRow keys; the schema accepts these directly.
type row struct {
	Ts                       string  `json:"ts"`
	Revision                 uint64  `json:"revision"`
	SessionID                string  `json:"session_id"`
	PlayID                   string  `json:"play_id"`
	AttemptID                uint32  `json:"attempt_id"`
	PlayerID                 string  `json:"player_id"`
	GroupID                  string  `json:"group_id"`
	UserAgent                string  `json:"user_agent"`
	ManifestURL              string  `json:"manifest_url"`
	ManifestVariants         string  `json:"manifest_variants"`
	LastRequestURL           string  `json:"last_request_url"`
	ContentID                string  `json:"content_id"`
	PlayerState              string  `json:"player_state"`
	WaitingReason            string  `json:"waiting_reason"`
	StateFrom                string  `json:"state_from"`
	StateTo                  string  `json:"state_to"`
	ContentName              string  `json:"content_name"`
	UserMarkedAt             string  `json:"user_marked_at"`
	BufferDepthS             float32 `json:"buffer_depth_s"`
	NetworkBitrateMbps       float32 `json:"network_bitrate_mbps"`
	VideoBitrateMbps         float32 `json:"video_bitrate_mbps"`
	// Player-supplied from/to bitrate on rate_shift_down /
	// rate_shift_up POSTs (issue #470). Authoritative transition
	// values — eventclass.snapshotRateClassifier prefers these to
	// the (prev.VideoBitrate, cur.VideoBitrate) inference because
	// iOS emits a parallel `video_bitrate_change` POST in the same
	// handler with the same new bitrate, briefly aliasing the
	// inferred prev. `json:"-"` keeps these out of the CH insert
	// body (no column in session_snapshots) — they're transient,
	// read only at classify time.
	RateFromMbps             float32 `json:"-"`
	RateToMbps               float32 `json:"-"`
	MeasuredMbps             float32 `json:"measured_mbps"`
	MbpsShaperRate           float32 `json:"mbps_shaper_rate"`
	MbpsShaperAvg            float32 `json:"mbps_shaper_avg"`
	// Server-side TCP_INFO RTT (issue #401).
	ClientRTTMs             float32 `json:"client_rtt_ms"`
	ClientRTTMaxMs          float32 `json:"client_rtt_max_ms"`
	ClientRTTMinMs          float32 `json:"client_rtt_min_ms"`
	ClientRTTMinLifetimeMs  float32 `json:"client_rtt_min_lifetime_ms"`
	ClientRTTVarMs          float32 `json:"client_rtt_var_ms"`
	ClientRTOMs             float32 `json:"client_rto_ms"`
	// Out-of-band ICMP path-ping (issue #404).
	ClientPathPingRTTMs     float32 `json:"client_path_ping_rtt_ms"`
	DisplayResolution        string  `json:"display_resolution"`
	FetchingResolution       string  `json:"fetching_resolution"`
	VideoResolution          string  `json:"video_resolution"`
	FramesDisplayed          uint64  `json:"frames_displayed"`
	FramesDropped            uint32  `json:"frames_dropped"`
	StallCount               uint32  `json:"stall_count"`
	StallTimeS               float32 `json:"stall_time_s"`
	// ── #550 Phase 1: residency accumulators (UInt32 ms) + deltas ──
	// Cumulative-on-the-wire since play start; forwarder fills the
	// paired *Delta fields from a per-play state cache in toRow().
	// Gerund-named to match the schema column names.
	PlayingTimeMs            uint32 `json:"playing_time_ms"`
	PlayingTimeMsDelta       uint32 `json:"playing_time_ms_delta"`
	PlayingCount             uint32 `json:"playing_count"`
	PlayingCountDelta        uint32 `json:"playing_count_delta"`
	PausingTimeMs            uint32 `json:"pausing_time_ms"`
	PausingTimeMsDelta       uint32 `json:"pausing_time_ms_delta"`
	PausingCount             uint32 `json:"pausing_count"`
	PausingCountDelta        uint32 `json:"pausing_count_delta"`
	BufferingTimeMs          uint32 `json:"buffering_time_ms"`
	BufferingTimeMsDelta     uint32 `json:"buffering_time_ms_delta"`
	BufferingCount           uint32 `json:"buffering_count"`
	BufferingCountDelta      uint32 `json:"buffering_count_delta"`
	StallingTimeMs           uint32 `json:"stalling_time_ms"`
	StallingTimeMsDelta      uint32 `json:"stalling_time_ms_delta"`
	StallingCount            uint32 `json:"stalling_count"`
	StallingCountDelta       uint32 `json:"stalling_count_delta"`
	IdlingTimeMs             uint32 `json:"idling_time_ms"`
	IdlingTimeMsDelta        uint32 `json:"idling_time_ms_delta"`
	IdlingCount              uint32 `json:"idling_count"`
	IdlingCountDelta         uint32 `json:"idling_count_delta"`
	SeekingTimeMs            uint32 `json:"seeking_time_ms"`
	SeekingTimeMsDelta       uint32 `json:"seeking_time_ms_delta"`
	SeekingCount             uint32 `json:"seeking_count"`
	SeekingCountDelta        uint32 `json:"seeking_count_delta"`
	TrickplayingTimeMs       uint32 `json:"trickplaying_time_ms"`
	TrickplayingTimeMsDelta  uint32 `json:"trickplaying_time_ms_delta"`
	TrickplayingCount        uint32 `json:"trickplaying_count"`
	TrickplayingCountDelta   uint32 `json:"trickplaying_count_delta"`
	// Per-event sticky durations (Phase 1 ancillary).
	StallDurationMs          uint32 `json:"stall_duration_ms"`
	BufferingDurationMs      uint32 `json:"buffering_duration_ms"`
	// Orthogonal "this stall won't auto-recover" discriminator.
	// True from the moment AVPlayer transitions stalled → .paused
	// (give-up) until next .playing transition. State stays "stalled"
	// for residency continuity; dashboards key on this flag to surface
	// operator-actionable stalls.
	StallStuck               bool   `json:"stall_stuck"`
	// ── #550 Phase 2: outcome status + structured error fields ─────
	PlaybackStatus           string `json:"playback_status"`
	PlaybackReason           string `json:"playback_reason"`
	ErrorCode                int32  `json:"error_code"`
	ErrorDomain              string `json:"error_domain"`
	ErrorDetails             string `json:"error_details"`
	TerminalErrorCode        int32  `json:"terminal_error_code"`
	TerminalErrorDomain      string `json:"terminal_error_domain"`
	TerminalErrorDetails     string `json:"terminal_error_details"`
	ErrorCount               uint32 `json:"error_count"`
	ErrorCountDelta          uint32 `json:"error_count_delta"`
	// ── #550 Phase 4: device / platform / version taxonomy ─────────
	OsVersionMajor           uint16  `json:"os_version_major"`
	OsVersionMinor           uint16  `json:"os_version_minor"`
	AppVersion               string  `json:"app_version"`
	DeviceClass              string  `json:"device_class"`
	DeviceModel              string  `json:"device_model"`
	PlayerTech               string  `json:"player_tech"`
	// Orientation-aware "WxH" — supersedes the three screen_* fields
	// dropped on 2026-05-30.
	DeviceResolution         string  `json:"device_resolution"`
	// ── #550 Phase 1 video-startup ms migrations ───────────────────
	// New canonical names alongside deprecated _s variants below
	// (mirror-written by toRow during deprecation window).
	VideoFirstFrameTimeMs    uint32 `json:"video_first_frame_time_ms"`
	VideoStartTimeMs         uint32 `json:"video_start_time_ms"`
	PositionS                float32 `json:"position_s"`
	LiveEdgeS                float32 `json:"live_edge_s"`
	TrueOffsetS              float32 `json:"true_offset_s"`
	PlaybackRate             float32 `json:"playback_rate"`
	LoopCountPlayer          uint32  `json:"loop_count_player"`
	LoopCountDelta       uint32  `json:"loop_count_delta"`
	LoopCountServer          uint32  `json:"loop_count_server"`
	PlayerRestarts           uint32  `json:"player_restarts"`
	ProfileShiftCount        uint32  `json:"profile_shift_count"`
	// EffectiveRateLimitMbps is the kernel-enforced cap at write time:
	// max(operator override, deployment baseline). 0 means uncapped.
	// Distinct from nftables_bandwidth_mbps (operator intent). Stamped
	// by the proxy in normalizeSessionsForResponse. Issue #480.
	EffectiveRateLimitMbps   float32 `json:"effective_rate_limit_mbps"`
	LastEvent                string  `json:"last_event"`
	TriggerType              string  `json:"trigger_type"`
	EventTime                string  `json:"event_time"`
	PlayerError              string  `json:"player_error"`

	AvgNetworkBitrateMbps    float32 `json:"avg_network_bitrate_mbps"`
	BufferEndS               float32 `json:"buffer_end_s"`
	LiveOffsetS              float32 `json:"live_offset_s"`
	PlayheadWallclockMs      int64   `json:"playhead_wallclock_ms"`
	SeekableEndS             float32 `json:"seekable_end_s"`
	MetricsSource            string  `json:"metrics_source"`
	VideoFirstFrameTimeS     float32 `json:"video_first_frame_time_s"`
	VideoQualityPct          float32 `json:"video_quality_pct"`
	VideoQuality60sPct       float32 `json:"video_quality_60s_pct"`
	VideoQualityAvgPct       float32 `json:"video_quality_avg_pct"`
	VideoStartTimeS          float32 `json:"video_start_time_s"`
	// iOS-published per-variant watch time (issue #486). JSON-object
	// string keyed by `<resolution>@<kbps>kbps` with seconds-watched
	// values. Stored verbatim so the dashboard can parse + expand it
	// into one chip per variant without per-variant column proliferation.
	TimePerVariantS          string  `json:"time_per_variant_s"`
	// Client-side RTT proxy from AVMetrics (issue #486). Median TTFB
	// over the most recent MediaResourceRequest events. Read by the
	// RTT chart in lockstep with the server-side `client_rtt_ms`.
	ClientRttAvmetricsMs     float32 `json:"client_rtt_avmetrics_ms"`
	// HOLD-BACK / PART-HOLD-BACK from the manifest (issue #486
	// follow-up). AVFoundation parses EXT-X-SERVER-CONTROL and
	// surfaces the result via AVPlayerItem.recommendedTimeOffsetFromLive.
	RecommendedOffsetS       float32 `json:"recommended_offset_s"`
	ConfiguredOffsetS        float32 `json:"configured_offset_s"`
	// Active variant's nominal frame rate (issue #486 follow-up).
	FramesRate        float32 `json:"frames_rate"`

	MbpsTransferComplete     float32 `json:"mbps_transfer_complete"`
	MbpsTransferRate         float32 `json:"mbps_transfer_rate"`
	PlayerIP                 string  `json:"player_ip"`
	// OriginationIP is the proxy-observed client IP from before the
	// load-balancer chain (X-Forwarded-For first hop). Surfaced
	// separately from PlayerIP so the dashboard can show both the
	// edge-observed IP and the next-hop after any LB rewrites.
	// Lost in the v1→CH pipeline pre-#550; restored here so the
	// session viewer's "Origination IP" tile populates from archived
	// rows the same way the live Testing dashboard does.
	OriginationIP            string  `json:"origination_ip"`
	// SessionNumber is the proxy's short numeric ID per session (port-
	// derived; surfaced as `display_id` in v2 + dashboard tiles).
	// Restored via #550 dashboard-parity fix — was missing from CH so
	// archived sessions showed "—" for Display ID.
	SessionNumber            uint32  `json:"session_number"`
	ServerReceivedAtMs       int64   `json:"server_received_at_ms"`
	XForwardedPort           uint16  `json:"x_forwarded_port"`
	XForwardedPortExternal   uint16  `json:"x_forwarded_port_external"`

	MasterManifestURL                  string  `json:"master_manifest_url"`
	MasterManifestFailureType          string  `json:"master_manifest_failure_type"`
	MasterManifestFailureMode          string  `json:"master_manifest_failure_mode"`
	MasterManifestFailureFrequency     float32 `json:"master_manifest_failure_frequency"`
	MasterManifestConsecutiveFailures  uint32  `json:"master_manifest_consecutive_failures"`
	MasterManifestRequestsCount        uint32  `json:"master_manifest_requests_count"`

	ManifestFailureFrequency     float32 `json:"manifest_failure_frequency"`
	ManifestFailureURLs          string  `json:"manifest_failure_urls"`
	ManifestConsecutiveFailures  uint32  `json:"manifest_consecutive_failures"`
	ManifestRequestsCount        uint32  `json:"manifest_requests_count"`

	SegmentFailureFrequency     float32 `json:"segment_failure_frequency"`
	SegmentFailureURLs          string  `json:"segment_failure_urls"`
	SegmentConsecutiveFailures  uint32  `json:"segment_consecutive_failures"`
	SegmentsCount               uint32  `json:"segments_count"`

	AllFailureType            string  `json:"all_failure_type"`
	AllFailureMode            string  `json:"all_failure_mode"`
	AllFailureFrequency       float32 `json:"all_failure_frequency"`
	AllFailureURLs            string  `json:"all_failure_urls"`
	AllConsecutiveFailures    uint32  `json:"all_consecutive_failures"`

	TransportFailureFrequency    float32 `json:"transport_failure_frequency"`
	TransportFailureMode         string  `json:"transport_failure_mode"`
	TransportFailureUnits        string  `json:"transport_failure_units"`
	TransportConsecutiveFailures uint32  `json:"transport_consecutive_failures"`
	TransportConsecutiveSeconds  float32 `json:"transport_consecutive_seconds"`
	TransportConsecutiveUnits    uint32  `json:"transport_consecutive_units"`
	TransportFrequencySeconds    float32 `json:"transport_frequency_seconds"`
	TransportFaultDropPackets    uint8   `json:"transport_fault_drop_packets"`
	TransportFaultRejectPackets  uint8   `json:"transport_fault_reject_packets"`
	TransportFaultOffSeconds     float32 `json:"transport_fault_off_seconds"`
	TransportFaultOnSeconds      float32 `json:"transport_fault_on_seconds"`
	TransportFaultType           string  `json:"transport_fault_type"`
	FaultCountTransferActiveTimeout uint32 `json:"fault_count_transfer_active_timeout"`
	FaultCountTransferIdleTimeout   uint32 `json:"fault_count_transfer_idle_timeout"`

	TransferActiveTimeoutSeconds   float32 `json:"transfer_active_timeout_seconds"`
	TransferIdleTimeoutSeconds     float32 `json:"transfer_idle_timeout_seconds"`
	TransferTimeoutAppliesManifests uint8  `json:"transfer_timeout_applies_manifests"`
	TransferTimeoutAppliesMaster    uint8  `json:"transfer_timeout_applies_master"`
	TransferTimeoutAppliesSegments  uint8  `json:"transfer_timeout_applies_segments"`

	NftablesPatternStep             uint32  `json:"nftables_pattern_step"`
	NftablesPatternStepRuntime      uint32  `json:"nftables_pattern_step_runtime"`
	NftablesPatternSteps            string  `json:"nftables_pattern_steps"`
	NftablesPatternRateRuntimeMbps  float32 `json:"nftables_pattern_rate_runtime_mbps"`
	NftablesPatternMarginPct        float32 `json:"nftables_pattern_margin_pct"`
	NftablesPatternTemplateMode     string  `json:"nftables_pattern_template_mode"`

	ContentAllowedVariants string  `json:"content_allowed_variants"`
	ContentLiveOffset      float32 `json:"content_live_offset"`
	ContentStripCodecs     string  `json:"content_strip_codecs"`

	AbrcharRunLock   uint8  `json:"abrchar_run_lock"`
	// ControlRevision is go-proxy's RFC3339Nano "ETag" for optimistic
	// concurrency on session mutations. Originally stored as UInt64
	// in CH (truncated by Sscanf("%d") to just the leading year);
	// type fixed in-place via DROP UInt64 + RENAME control_revision_str
	// → control_revision in a follow-up PR.
	ControlRevision  string `json:"control_revision"`
	ServerVideoRendition     string  `json:"server_video_rendition"`
	ServerVideoRenditionMbps float32 `json:"server_video_rendition_mbps"`
	ManifestFailureType      string  `json:"manifest_failure_type"`
	ManifestFailureMode      string  `json:"manifest_failure_mode"`
	SegmentFailureType       string  `json:"segment_failure_type"`
	SegmentFailureMode       string  `json:"segment_failure_mode"`
	TransportFailureType     string  `json:"transport_failure_type"`
	TransportFaultActive     uint8   `json:"transport_fault_active"`
	NftablesBandwidthMbps    float32 `json:"nftables_bandwidth_mbps"`
	NftablesDelayMs          uint32  `json:"nftables_delay_ms"`
	NftablesPacketLoss       float32 `json:"nftables_packet_loss"`
	NftablesPatternEnabled   uint8   `json:"nftables_pattern_enabled"`
	FirstRequestTime         string  `json:"first_request_time"`
	LastRequest              string  `json:"last_request"`
	SessionDuration          float32 `json:"session_duration"`
	SessionJSON              string  `json:"session_json"`
	// Labels are the row's <severity>=<event> tags computed at ingest
	// time (issue #473). Source for the dashboard's severity-based
	// row tint + chips and the auto-classification tier bump in
	// classification.go's runClassifyLoop. Bucket-A markers (the ones
	// that were pure re-labels of one source row) were retired in
	// favor of these labels. CH type is Array(LowCardinality(String)).
	Labels                   []string `json:"labels,omitempty"`
}

type ssePayload struct {
	Revision uint64                   `json:"revision"`
	Sessions []map[string]interface{} `json:"sessions"`
}

// fingerprintCache tracks the last-seen fingerprint per session so we
// only insert when something changed (the SSE stream re-emits unchanged
// sessions on every tick).
type fingerprintCache struct {
	mu sync.Mutex
	m  map[string]string
}

func newFingerprintCache() *fingerprintCache {
	return &fingerprintCache{m: make(map[string]string)}
}

func (c *fingerprintCache) changed(sessionID, fp string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if prev, ok := c.m[sessionID]; ok && prev == fp {
		return false
	}
	c.m[sessionID] = fp
	return true
}

func (c *fingerprintCache) prune(activeIDs map[string]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.m {
		if _, ok := activeIDs[id]; !ok {
			delete(c.m, id)
		}
	}
}

// setupLogFile mirrors stdlib log output to a file under $CONTENT_DIR
// (typically /media/logs/forwarder.log) in addition to stderr, matching
// the pattern the go-server backends use after #377. Best-effort: if
// FORWARDER_LOG_FILE is unset or unopenable (perms, missing dir), we
// silently fall back to stderr-only — forwarder is a sidecar, never
// blocking the live path on log-file availability.
func setupLogFile() {
	path := os.Getenv("FORWARDER_LOG_FILE")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("FORWARDER_LOG_FILE=%q: open failed (%v); logging to stderr only", path, err)
		return
	}
	log.SetOutput(io.MultiWriter(os.Stderr, f))
	log.Printf("forwarder log file: %s", path)
}

func main() {
	setupLogFile()
	cfg := loadConfig()
	log.Printf("forwarder starting: sse=%s ch=%s/%s.%s", cfg.sseURL, cfg.clickhouseURL, cfg.chDatabase, cfg.chTable)

	// #553 — load QoE label thresholds (compiled-in defaults, optionally
	// overlaid from FORWARDER_QOE_THRESHOLDS_PATH) and install them into
	// the write-time labeler. loadQoEThresholds logs the resolved tier so
	// operators can audit which thresholds this deployment is running at.
	defaultLabelState.SetThresholds(loadQoEThresholds(os.Getenv("FORWARDER_QOE_THRESHOLDS_PATH")))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("shutdown signal received")
		cancel()
	}()

	// In-memory ring covering the recently-ingested rows for both
	// streams. The /api/v2/timeseries handler reads the ring +
	// CH and dedupes on the boundary so the dashboard sees fresh
	// data with no NDJSON-vs-pushLive merge race. Eviction runs
	// once per second; pending/inserting rows are sticky regardless
	// of age (they exist nowhere else yet).
	ring := NewRing(int64(cfg.ringWindowSecs)*1000, 0)
	ring.StartEvictor(ctx.Done(), time.Second)

	// Snapshot ingest. Labels at write time replace the eventclass
	// classifier registry retired in issue #474 Milestone C.
	rows := make(chan row, 4096)
	go batchInserter(ctx, cfg, ring, rows)
	go serveHTTP(ctx, cfg, ring)

	// Network log archival: subscribe to go-proxy's /api/network/stream
	// SSE endpoint and forward each per-request event to ClickHouse so
	// the session-viewer can replay them after the session is gone.
	netRows := make(chan netRow, 8192)
	netSeenSet := newNetSeen(50000)
	go batchInsertNet(ctx, cfg, ring, netRows)
	go runNetworkStream(ctx, cfg, netSeenSet, netRows)

	// Control events (issue #474 Milestone B): subscribe to go-proxy's
	// /api/control/stream and write into control_events. Mirrors the
	// network log path exactly. Dedupe by event_fingerprint so SSE
	// reconnects don't double-insert.
	ctrlRows := make(chan ctrlRow, 4096)
	ctrlSeenSet := newCtrlSeen(20000)
	go batchInsertControl(ctx, cfg, ctrlRows)
	go runControlStream(ctx, cfg, ctrlSeenSet, ctrlRows)

	// iOS 18 AVMetrics raw event stream (issue #486 spike): subscribe to
	// go-proxy's /api/avmetrics/stream and write into ios_avmetric_events.
	// Same shape as control_events ingest.
	avmRows := make(chan avmRow, 4096)
	avmSeenSet := newAVMSeen(20000)
	go batchInsertAVM(ctx, cfg, avmRows)
	go runAVMStream(ctx, cfg, avmSeenSet, avmRows)

	// Auto-classifier: when a snapshot carries an interesting signal
	// (911 / frozen / hard error / fault counters), queue the
	// (session, play) pair for reclassification. A single background
	// goroutine drains the queue every 30 s and fires one ALTER
	// UPDATE per pair, marking 'interesting' on session_snapshots +
	// network_requests. ClickHouse mutations are async + cheap; the
	// debounce coalesces repeated signals from the same session.
	classifyQ := newClassifyQueue()
	go runClassifyLoop(ctx, cfg, classifyQ, 30*time.Second)

	cache := newFingerprintCache()
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := streamSSE(ctx, cfg, cache, netSeenSet, rows, classifyQ)
		if ctx.Err() != nil {
			return
		}
		log.Printf("sse stream ended: %v (reconnecting in %s)", err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func streamSSE(ctx context.Context, cfg config, cache *fingerprintCache, netSeen *netSeen, out chan<- row, classifyQ *classifyQueue) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.sseURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sse status %d", resp.StatusCode)
	}

	reader := bufio.NewReaderSize(resp.Body, 1<<20)
	var dataBuf bytes.Buffer
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if dataBuf.Len() > 0 {
				handlePayload(dataBuf.Bytes(), cache, netSeen, out, classifyQ)
				dataBuf.Reset()
			}
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			dataBuf.WriteString(line[len("data: "):])
		}
	}
}

func handlePayload(data []byte, cache *fingerprintCache, netSeen *netSeen, out chan<- row, classifyQ *classifyQueue) {
	var payload ssePayload
	if err := json.Unmarshal(data, &payload); err != nil {
		log.Printf("bad sse payload: %v", err)
		return
	}
	fallback := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	active := make(map[string]struct{}, len(payload.Sessions))
	// #550 Phase 1: collect the play_ids we observe this tick so we
	// can prune playResidency at the end. Mirrors the sessionToPlayerID
	// active-set pattern but keyed by play_id (since residency resets
	// at play boundaries, not session boundaries).
	activePlays := make(map[string]struct{}, len(payload.Sessions))
	for _, s := range payload.Sessions {
		sessionID, _ := s["session_id"].(string)
		if sessionID == "" {
			continue
		}
		active[sessionID] = struct{}{}
		if pid := canonicalV2ID(getStr(s, "play_id")); pid != "" {
			activePlays[pid] = struct{}{}
		}
		fp := fingerprint(s)
		if !cache.changed(sessionID, fp) {
			continue
		}
		// Anchor the row's `ts` to the snapshot's `player_metrics_event_time`
		// (proxy/iOS clock) so `session_snapshots.ts` and the in-blob
		// event_time are the same value. This gives the session-viewer's
		// chart x-axis a single time source and lets `ORDER BY ts` produce
		// event-time ordering for free. The forwarder wall clock is only
		// used as a fallback if event_time is missing — which shouldn't
		// happen now that `saveSessionByID` stamps proxy now() on every
		// merge (issue #403 follow-up).
		ts := snapshotEventTimeAsCHTimestamp(s, fallback)
		// Stamp the sessionID→playerID map so the network row writer
		// (runNetworkStream → entryToRow) can carry the v2 player_id
		// onto every HAR row at insert time.
		sessionToPlayerID.set(sessionID, canonicalV2ID(getStr(s, "player_id")))
		out <- toRow(ts, payload.Revision, sessionID, s)
		// Queue auto-classifier if this snapshot carries any of the
		// "really bad things" signals. Debounced — repeated marks
		// for the same (session,play) coalesce to one mutation.
		if classifyQ != nil && hasInterestingSignal(s) {
			classifyQ.mark(sessionID, canonicalV2ID(getStr(s, "play_id")))
		}
	}
	cache.prune(active)
	// Free network-log fingerprint memory for sessions that have aged
	// out of the SSE stream.
	if netSeen != nil {
		netSeen.prune(active)
	}
	sessionToPlayerID.prune(active)
	playResidency.prune(activePlays)
}

func fingerprint(s map[string]interface{}) string {
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// snapshotEventTimeAsCHTimestamp converts the snapshot's
// `player_metrics_event_time` (RFC3339 with optional fractional
// seconds) into the `YYYY-MM-DD hh:mm:ss.SSS` form ClickHouse expects
// for `DateTime64(3, 'UTC')`. Falls back to `fallback` when the field
// is missing or unparseable.
func snapshotEventTimeAsCHTimestamp(s map[string]interface{}, fallback string) string {
	raw := getStr(s, "player_metrics_event_time")
	if raw == "" {
		return fallback
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return fallback
	}
	return t.UTC().Format("2006-01-02 15:04:05.000")
}

func toRow(ts string, revision uint64, sessionID string, s map[string]interface{}) row {
	full, _ := json.Marshal(s)
	// Store the v2-canonical ids the v3 client will use to look this
	// row up. canonicalIDsFor routes through v2translate.PlayerFromSession
	// so the play_id fallback derivation (used when the raw SSE map
	// doesn't carry `play_id`, which is most non-Apple clients) matches
	// what `/api/v2/players` emits. Without that fallback the column
	// stays empty and the v3 client's play_id filter excludes every row.
	playerCanonical, playCanonical := canonicalIDsFor(s)
	r := row{
		Ts:                       ts,
		Revision:                 revision,
		SessionID:                sessionID,
		PlayID:                   playCanonical,
		// attempt_id is player-supplied and sticky on the session map
		// at go-proxy:4517-4519 — pull it from the v1 payload (string
		// form) and parse to uint32 here. 0 (the uint32 zero value)
		// means "unknown" — pre-rename rows or non-iOS clients.
		AttemptID:                parseAttemptID(s),
		PlayerID:                 playerCanonical,
		GroupID:                  getStr(s, "group_id"),
		UserAgent:                getStr(s, "user_agent"),
		ManifestURL:              getStr(s, "manifest_url"),
		ManifestVariants:         getJSON(s, "manifest_variants"),
		LastRequestURL:           getStr(s, "last_request_url"),
		ContentID:                contentIDFromURL(getStr(s, "manifest_url"), getStr(s, "last_request_url")),
		PlayerState:              getStr(s, "player_metrics_state"),
		WaitingReason:            getStr(s, "player_metrics_waiting_reason"),
		StateFrom:                getStr(s, "player_metrics_state_from"),
		StateTo:                  getStr(s, "player_metrics_state_to"),
		ContentName:              getStr(s, "player_metrics_content_name"),
		UserMarkedAt:             getStr(s, "player_metrics_user_marked_at"),
		BufferDepthS:             getF32(s, "player_metrics_buffer_depth_s"),
		NetworkBitrateMbps:       getF32(s, "player_metrics_network_bitrate_mbps"),
		VideoBitrateMbps:         getF32(s, "player_metrics_video_bitrate_mbps"),
		RateFromMbps:             getF32(s, "player_metrics_rate_from_mbps"),
		RateToMbps:               getF32(s, "player_metrics_rate_to_mbps"),
		MeasuredMbps:             getF32(s, "measured_mbps"),
		MbpsShaperRate:           getF32(s, "mbps_shaper_rate"),
		MbpsShaperAvg:            getF32(s, "mbps_shaper_avg"),
		ClientRTTMs:              getF32(s, "client_rtt_ms"),
		ClientRTTMaxMs:           getF32(s, "client_rtt_max_ms"),
		ClientRTTMinMs:           getF32(s, "client_rtt_min_ms"),
		ClientRTTMinLifetimeMs:   getF32(s, "client_rtt_min_lifetime_ms"),
		ClientRTTVarMs:           getF32(s, "client_rtt_var_ms"),
		ClientRTOMs:              getF32(s, "client_rto_ms"),
		ClientPathPingRTTMs:      getF32(s, "client_path_ping_rtt_ms"),
		DisplayResolution:        getStr(s, "player_metrics_display_resolution"),
		FetchingResolution:       getStr(s, "player_metrics_fetching_resolution"),
		VideoResolution:          getStr(s, "player_metrics_video_resolution"),
		FramesDisplayed:          getU64(s, "player_metrics_frames_displayed"),
		FramesDropped:            uint32(getU64(s, "player_metrics_frames_dropped")),
		// Phase 1 residency accumulators — iOS emits ms; forwarder
		// passes through into both the new *_time_ms column AND the
		// deprecated stall_time_s column (mirror-write at the
		// canonical pair below). Deltas are not parsed from payload —
		// they're forwarder-computed in computeResidencyDeltas() after
		// row construction.
		PlayingTimeMs:            uint32(getU64(s, "player_metrics_playing_time_ms")),
		PlayingCount:             uint32(getU64(s, "player_metrics_playing_count")),
		PausingTimeMs:            uint32(getU64(s, "player_metrics_pausing_time_ms")),
		PausingCount:             uint32(getU64(s, "player_metrics_pausing_count")),
		BufferingTimeMs:          uint32(getU64(s, "player_metrics_buffering_time_ms")),
		BufferingCount:           uint32(getU64(s, "player_metrics_buffering_count")),
		StallingTimeMs:           uint32(getU64(s, "player_metrics_stalling_time_ms")),
		StallingCount:            uint32(getU64(s, "player_metrics_stalling_count")),
		IdlingTimeMs:             uint32(getU64(s, "player_metrics_idling_time_ms")),
		IdlingCount:              uint32(getU64(s, "player_metrics_idling_count")),
		SeekingTimeMs:            uint32(getU64(s, "player_metrics_seeking_time_ms")),
		SeekingCount:             uint32(getU64(s, "player_metrics_seeking_count")),
		TrickplayingTimeMs:       uint32(getU64(s, "player_metrics_trickplaying_time_ms")),
		TrickplayingCount:        uint32(getU64(s, "player_metrics_trickplaying_count")),
		StallDurationMs:          uint32(getU64(s, "player_metrics_stall_duration_ms")),
		BufferingDurationMs:      uint32(getU64(s, "player_metrics_buffering_duration_ms")),
		StallStuck:               getBool(s, "player_metrics_stall_stuck"),
		VideoFirstFrameTimeMs:    uint32(getU64(s, "player_metrics_video_first_frame_time_ms")),
		VideoStartTimeMs:         uint32(getU64(s, "player_metrics_video_start_time_ms")),
		// Phase 1 soft cutover: mirror-write the deprecated stall_*
		// columns from stalling_*. Dashboards reading stall_count /
		// stall_time_s keep working until they migrate.
		StallCount:               uint32(getU64(s, "player_metrics_stalling_count")),
		StallTimeS:               float32(getU64(s, "player_metrics_stalling_time_ms")) / 1000.0,
		// Phase 2 outcome + error fields.
		PlaybackStatus:           getStr(s, "player_metrics_playback_status"),
		PlaybackReason:           getStr(s, "player_metrics_playback_reason"),
		ErrorCode:                int32(getI64(s, "player_metrics_error_code")),
		ErrorDomain:              getStr(s, "player_metrics_error_domain"),
		ErrorDetails:             getStr(s, "player_metrics_error_details"),
		TerminalErrorCode:        int32(getI64(s, "player_metrics_terminal_error_code")),
		TerminalErrorDomain:      getStr(s, "player_metrics_terminal_error_domain"),
		TerminalErrorDetails:     getStr(s, "player_metrics_terminal_error_details"),
		ErrorCount:               uint32(getU64(s, "player_metrics_error_count")),
		// Phase 4 device taxonomy.
		OsVersionMajor:           uint16(getU64(s, "player_metrics_os_version_major")),
		OsVersionMinor:           uint16(getU64(s, "player_metrics_os_version_minor")),
		AppVersion:               getStr(s, "player_metrics_app_version"),
		DeviceClass:              getStr(s, "player_metrics_device_class"),
		DeviceModel:              getStr(s, "player_metrics_device_model"),
		PlayerTech:                getStr(s, "player_metrics_player_tech"),
		DeviceResolution:          getStr(s, "player_metrics_device_resolution"),
		PositionS:                getF32(s, "player_metrics_position_s"),
		LiveEdgeS:                getF32(s, "player_metrics_live_edge_s"),
		TrueOffsetS:              getF32(s, "player_metrics_true_offset_s"),
		PlaybackRate:             getF32(s, "player_metrics_playback_rate"),
		LoopCountPlayer:          uint32(getU64(s, "loop_count_player")),
		LoopCountDelta:       uint32(getU64(s, "player_metrics_loop_count_delta")),
		LoopCountServer:          uint32(getU64(s, "loop_count_server")),
		// player_restarts is sent at the top level of iOS POST (not
		// nested under player_metrics_*) — matches the v2translate
		// key. profile_shift_count IS prefixed.
		PlayerRestarts:           uint32(getU64(s, "player_restarts")),
		ProfileShiftCount:        uint32(getU64(s, "player_metrics_profile_shift_count")),
		// proxy stamps this top-level on every session normalize.
		EffectiveRateLimitMbps:   getF32(s, "effective_rate_limit_mbps"),
		LastEvent:                getStr(s, "player_metrics_last_event"),
		TriggerType:              getStr(s, "player_metrics_trigger_type"),
		EventTime:                getStr(s, "player_metrics_event_time"),
		PlayerError:              getStr(s, "player_metrics_error"),

		AvgNetworkBitrateMbps: getF32(s, "player_metrics_avg_network_bitrate_mbps"),
		BufferEndS:            getF32(s, "player_metrics_buffer_end_s"),
		LiveOffsetS:           getF32(s, "player_metrics_live_offset_s"),
		PlayheadWallclockMs:   int64(getU64(s, "player_metrics_playhead_wallclock_ms")),
		SeekableEndS:          getF32(s, "player_metrics_seekable_end_s"),
		MetricsSource:         getStr(s, "player_metrics_source"),
		VideoFirstFrameTimeS:  getF32(s, "player_metrics_video_first_frame_time_s"),
		VideoQualityPct:       getF32(s, "player_metrics_video_quality_pct"),
		VideoQuality60sPct:    getF32(s, "player_metrics_video_quality_60s_pct"),
		VideoQualityAvgPct:    getF32(s, "player_metrics_video_quality_avg_pct"),
		VideoStartTimeS:       getF32(s, "player_metrics_video_start_time_s"),
		TimePerVariantS:       stringField(s, "player_metrics_time_per_variant_s"),
		ClientRttAvmetricsMs:  getF32(s, "player_metrics_client_rtt_avmetrics_ms"),
		RecommendedOffsetS:    getF32(s, "player_metrics_recommended_offset_s"),
		ConfiguredOffsetS:     getF32(s, "player_metrics_configured_offset_s"),
		FramesRate:     getF32(s, "player_metrics_frames_rate"),

		MbpsTransferComplete:   getF32(s, "mbps_transfer_complete"),
		MbpsTransferRate:       getF32(s, "mbps_transfer_rate"),
		PlayerIP:               getStr(s, "player_ip"),
		OriginationIP:          getStr(s, "origination_ip"),
		SessionNumber:          uint32(getU64(s, "session_number")),
		ServerReceivedAtMs:     int64(getU64(s, "server_received_at_ms")),
		XForwardedPort:         uint16(getU64(s, "x_forwarded_port")),
		XForwardedPortExternal: uint16(getU64(s, "x_forwarded_port_external")),

		MasterManifestURL:                 getStr(s, "master_manifest_url"),
		MasterManifestFailureType:         getStr(s, "master_manifest_failure_type"),
		MasterManifestFailureMode:         getStr(s, "master_manifest_failure_mode"),
		MasterManifestFailureFrequency:    getF32(s, "master_manifest_failure_frequency"),
		MasterManifestConsecutiveFailures: uint32(getU64(s, "master_manifest_consecutive_failures")),
		MasterManifestRequestsCount:       uint32(getU64(s, "master_manifest_requests_count")),

		ManifestFailureFrequency:    getF32(s, "manifest_failure_frequency"),
		ManifestFailureURLs:         getJSON(s, "manifest_failure_urls"),
		ManifestConsecutiveFailures: uint32(getU64(s, "manifest_consecutive_failures")),
		ManifestRequestsCount:       uint32(getU64(s, "manifest_requests_count")),

		SegmentFailureFrequency:    getF32(s, "segment_failure_frequency"),
		SegmentFailureURLs:         getJSON(s, "segment_failure_urls"),
		SegmentConsecutiveFailures: uint32(getU64(s, "segment_consecutive_failures")),
		SegmentsCount:              uint32(getU64(s, "segments_count")),

		AllFailureType:         getStr(s, "all_failure_type"),
		AllFailureMode:         getStr(s, "all_failure_mode"),
		AllFailureFrequency:    getF32(s, "all_failure_frequency"),
		AllFailureURLs:         getJSON(s, "all_failure_urls"),
		AllConsecutiveFailures: uint32(getU64(s, "all_consecutive_failures")),

		TransportFailureFrequency:       getF32(s, "transport_failure_frequency"),
		TransportFailureMode:            getStr(s, "transport_failure_mode"),
		TransportFailureUnits:           getStr(s, "transport_failure_units"),
		TransportConsecutiveFailures:    uint32(getU64(s, "transport_consecutive_failures")),
		TransportConsecutiveSeconds:     getF32(s, "transport_consecutive_seconds"),
		TransportConsecutiveUnits:       uint32(getU64(s, "transport_consecutive_units")),
		TransportFrequencySeconds:       getF32(s, "transport_frequency_seconds"),
		TransportFaultDropPackets:       uint8(getU64(s, "transport_fault_drop_packets")),
		TransportFaultRejectPackets:     uint8(getU64(s, "transport_fault_reject_packets")),
		TransportFaultOffSeconds:        getF32(s, "transport_fault_off_seconds"),
		TransportFaultOnSeconds:         getF32(s, "transport_fault_on_seconds"),
		TransportFaultType:              getStr(s, "transport_fault_type"),
		FaultCountTransferActiveTimeout: uint32(getU64(s, "fault_count_transfer_active_timeout")),
		FaultCountTransferIdleTimeout:   uint32(getU64(s, "fault_count_transfer_idle_timeout")),

		TransferActiveTimeoutSeconds:    getF32(s, "transfer_active_timeout_seconds"),
		TransferIdleTimeoutSeconds:      getF32(s, "transfer_idle_timeout_seconds"),
		TransferTimeoutAppliesManifests: uint8(getU64(s, "transfer_timeout_applies_manifests")),
		TransferTimeoutAppliesMaster:    uint8(getU64(s, "transfer_timeout_applies_master")),
		TransferTimeoutAppliesSegments:  uint8(getU64(s, "transfer_timeout_applies_segments")),

		NftablesPatternStep:            uint32(getU64(s, "nftables_pattern_step")),
		NftablesPatternStepRuntime:     uint32(getU64(s, "nftables_pattern_step_runtime")),
		NftablesPatternSteps:           getJSON(s, "nftables_pattern_steps"),
		NftablesPatternRateRuntimeMbps: getF32(s, "nftables_pattern_rate_runtime_mbps"),
		NftablesPatternMarginPct:       getF32(s, "nftables_pattern_margin_pct"),
		NftablesPatternTemplateMode:    getStr(s, "nftables_pattern_template_mode"),

		ContentAllowedVariants: getJSON(s, "content_allowed_variants"),
		ContentLiveOffset:      getF32(s, "content_live_offset"),
		ContentStripCodecs:     getStr(s, "content_strip_codecs"),

		AbrcharRunLock:  uint8(getU64(s, "abrchar_run_lock")),
		ControlRevision: getStr(s, "control_revision"),
		ServerVideoRendition:     getStr(s, "server_video_rendition"),
		ServerVideoRenditionMbps: getF32(s, "server_video_rendition_mbps"),
		ManifestFailureType:      getStr(s, "manifest_failure_type"),
		ManifestFailureMode:      getStr(s, "manifest_failure_mode"),
		SegmentFailureType:       getStr(s, "segment_failure_type"),
		SegmentFailureMode:       getStr(s, "segment_failure_mode"),
		TransportFailureType:     getStr(s, "transport_failure_type"),
		TransportFaultActive:     uint8(getU64(s, "transport_fault_active")),
		NftablesBandwidthMbps:    getF32(s, "nftables_bandwidth_mbps"),
		NftablesDelayMs:          uint32(getU64(s, "nftables_delay_ms")),
		NftablesPacketLoss:       getF32(s, "nftables_packet_loss"),
		NftablesPatternEnabled:   uint8(getU64(s, "nftables_pattern_enabled")),
		FirstRequestTime:         getStr(s, "first_request_time"),
		LastRequest:              getStr(s, "last_request"),
		SessionDuration:          getF32(s, "session_duration"),
		SessionJSON:              string(full),
	}
	_ = playerCanonical // reserved for future cross-cache wiring
	// #550 Phase 1: compute paired _delta columns from the per-play
	// state cache. Empty playCanonical → no caching (returns zero
	// state, deltas equal accumulated). Called after the row struct
	// is populated so all accumulated counters are visible.
	applyResidencyDeltas(&r, playCanonical)
	// Default to in_progress on rows where iOS didn't stamp a status
	// (older clients, non-iOS payloads). Terminal status comes from
	// iOS — including the ended_buffering / ended_stalling refinement
	// of playback_reason on user_stopped rows. We don't reclassify
	// server-side because historical rows should carry the value the
	// client stamped at session_end forever, not the value a later
	// threshold tweak would produce.
	if r.PlaybackStatus == "" {
		r.PlaybackStatus = "in_progress"
	}
	return r
}

// contentIDFromURL pulls the {content} path segment out of a go-live URL.
// nginx routes /go-live/{content}/<file> for both manifests and segments,
// so either manifest_url or last_request_url usually contains it.
func contentIDFromURL(urls ...string) string {
	for _, u := range urls {
		if u == "" {
			continue
		}
		if i := strings.Index(u, "/go-live/"); i >= 0 {
			rest := u[i+len("/go-live/"):]
			if j := strings.Index(rest, "/"); j > 0 {
				return rest[:j]
			}
			if rest != "" {
				return rest
			}
		}
	}
	return ""
}

func getStr(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// parseAttemptID reads the player's attempt_id off the session map.
// Stored as either a string (URL-query path in go-proxy) or directly
// as a number when iOS sends it in the body. 0 means "unknown" —
// covers pre-rename rows and non-iOS clients.
func parseAttemptID(m map[string]interface{}) uint32 {
	switch v := m["attempt_id"].(type) {
	case string:
		if n, err := strconv.ParseUint(v, 10, 32); err == nil {
			return uint32(n)
		}
	case float64: // JSON numbers decode to float64
		if v >= 0 && v <= float64(^uint32(0)) {
			return uint32(v)
		}
	case int:
		if v >= 0 {
			return uint32(v)
		}
	}
	return 0
}

// getJSON returns m[key] re-encoded as a JSON string. Use for fields
// whose SSE shape is an array or object — we keep them queryable via
// JSONExtract*() on the ClickHouse side without committing to a fixed
// schema for nested data.
func getJSON(m map[string]interface{}, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// getBool reads a boolean field tolerantly. Native bool, "true"/"false"
// strings, and numeric 0/1 all parse. Missing or unparseable keys
// return false.
func getBool(m map[string]interface{}, key string) bool {
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(v) {
		case "true", "1", "t", "yes":
			return true
		}
	case float64:
		return v != 0
	case float32:
		return v != 0
	}
	return false
}

func getF32(m map[string]interface{}, key string) float32 {
	switch v := m[key].(type) {
	case float64:
		return float32(v)
	case float32:
		return v
	case string:
		var f float64
		if _, err := fmt.Sscanf(v, "%f", &f); err == nil {
			return float32(f)
		}
	}
	return 0
}

func getU64(m map[string]interface{}, key string) uint64 {
	switch v := m[key].(type) {
	case float64:
		if v < 0 {
			return 0
		}
		return uint64(v)
	case bool:
		if v {
			return 1
		}
		return 0
	case string:
		var u uint64
		if _, err := fmt.Sscanf(v, "%d", &u); err == nil {
			return u
		}
	}
	return 0
}

// getI64 mirrors getU64 but preserves sign — required for Apple
// NSError codes (negative, e.g. CoreMediaErrorDomain -12318) that
// land on the #550 Phase 2 `error_code` / `terminal_error_code`
// columns.
func getI64(m map[string]interface{}, key string) int64 {
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case bool:
		if v {
			return 1
		}
		return 0
	case string:
		var i int64
		if _, err := fmt.Sscanf(v, "%d", &i); err == nil {
			return i
		}
	}
	return 0
}

func batchInserter(ctx context.Context, cfg config, ring *Ring, in <-chan row) {
	buf := make([]row, 0, cfg.flushBatch)
	entries := make([]*ringEntry, 0, cfg.flushBatch)
	tick := time.NewTicker(cfg.flushEvery)
	defer tick.Stop()
	flush := func() {
		if len(buf) == 0 {
			return
		}
		// State transitions: pending → inserting → confirmed.
		// On INSERT failure we revert inserting → pending so the
		// ring's read path keeps surfacing the rows (they exist
		// nowhere else yet). We don't currently retry the failed
		// batch — the rows go to /dev/null for CH, which matches
		// today's behaviour. Follow-up: bounded retry queue.
		ring.MarkInserting(entries)
		if err := insert(ctx, cfg, buf); err != nil {
			log.Printf("insert failed (%d rows dropped): %v", len(buf), err)
			ring.RevertInserting(entries)
		} else {
			ring.MarkConfirmed(entries)
		}
		buf = buf[:0]
		entries = entries[:0]
	}
	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case r, ok := <-in:
			if !ok {
				flush()
				return
			}
			// Stamp severity-tagged labels at write time (issue #473)
			// — replaces bucket-A markers. The dashboard and the
			// auto-classifier both read this column instead of the
			// scattered per-event-type rules we used to maintain.
			r.Labels = computeEventLabels(&r)
			// Pin the row in the ring as `pending` before queueing
			// for INSERT. The pointer we get back lets us flip state
			// in lockstep with the batch's lifecycle.
			rowCopy := r
			e := ring.Add(
				ringKey{PlayerID: rowCopy.PlayerID},
				kindSample, nowMs(), rowCopy.Ts, rowCopy.PlayID, &rowCopy,
			)
			buf = append(buf, rowCopy)
			entries = append(entries, e)
			if len(buf) >= cfg.flushBatch {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

func insert(ctx context.Context, cfg config, rows []row) error {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for i := range rows {
		if err := enc.Encode(&rows[i]); err != nil {
			return err
		}
	}
	q := fmt.Sprintf("INSERT INTO %s.%s FORMAT JSONEachRow", cfg.chDatabase, cfg.chTable)
	u, err := url.Parse(cfg.clickhouseURL)
	if err != nil {
		return err
	}
	qs := u.Query()
	qs.Set("query", q)
	u.RawQuery = qs.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), &body)
	if err != nil {
		return err
	}
	if cfg.chUser != "" {
		req.SetBasicAuth(cfg.chUser, cfg.chPassword)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("clickhouse %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// serveHTTP runs the read-only query API. nginx proxies /analytics/api/*
// here, so the browser never talks to ClickHouse directly. Endpoints:
//
//	GET   /api/v2/plays?from=<rfc3339>&to=<rfc3339>&limit=<n>
//	PATCH /api/v2/plays/{play_id}     {classification: favourite|interesting|other|auto}
//	GET   /api/snapshots?session=<id>&from=<rfc3339>&to=<rfc3339>&limit=<n>
//	GET   /healthz
func serveHTTP(ctx context.Context, cfg config, ring *Ring) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	mountV2Handlers(mux, cfg)
	mountTimeseriesHandlers(mux, cfg, ring)

	// AI chat backend (#497). Default-on with embedded profile
	// catalog + system prompt — operators don't need to mount any
	// files. Set FORWARDER_LLM_DISABLED=1 to turn off. Override the
	// catalog or prompt by setting FORWARDER_LLM_PROFILES_PATH /
	// FORWARDER_LLM_PROMPT_PATH.
	//
	// Failure to load the catalog is logged but doesn't take the
	// whole forwarder down — the chat endpoint is non-critical
	// infra.
	if os.Getenv("FORWARDER_LLM_DISABLED") != "1" {
		if _, err := LoadLLMCatalog(cfg.llmProfilesPath); err != nil {
			log.Printf("llm chat disabled: %v", err)
		} else {
			h, err := newChatHandler(cfg)
			if err != nil {
				log.Printf("llm chat disabled: %v", err)
			} else {
				mountChatHandlers(mux, h)
				src := "embedded defaults"
				if cfg.llmProfilesPath != "" {
					src = cfg.llmProfilesPath
				}
				log.Printf("llm chat enabled at /api/v2/chat (catalog=%s, budget=$%.2f/day)",
					src, cfg.llmBudgetUSD)
			}
		}
	}
	mux.HandleFunc("/api/snapshots", func(w http.ResponseWriter, r *http.Request) {
		session := r.URL.Query().Get("session")
		if session == "" {
			http.Error(w, "missing session", http.StatusBadRequest)
			return
		}
		params := map[string]string{"session": session}
		clauses := []string{"session_id = {session:String}"}
		if v := r.URL.Query().Get("play_id"); v != "" {
			// Sentinel "—" stands for "rows ingested before go-proxy
			// stamped play_id" — translate back to the empty string
			// the column actually stores.
			if v == "—" {
				clauses = append(clauses, "play_id = ''")
			} else {
				clauses = append(clauses, "play_id = {play:String}")
				params["play"] = canonicalV2ID(v)
			}
		}
		if v := r.URL.Query().Get("from"); v != "" {
			clauses = append(clauses, "ts >= parseDateTime64BestEffort({from:String})")
			params["from"] = v
		}
		if v := r.URL.Query().Get("to"); v != "" {
			clauses = append(clauses, "ts <= parseDateTime64BestEffort({to:String})")
			params["to"] = v
		}
		limit := 50000
		if v := r.URL.Query().Get("limit"); v != "" {
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 && n <= 50000 {
				limit = n
			}
		}
		// Default ASC; replay UI passes order=desc so most-recent
		// snapshots arrive first and the end-of-session window paints
		// before older data has finished streaming.
		order := "ASC"
		if strings.EqualFold(r.URL.Query().Get("order"), "desc") {
			order = "DESC"
		}
		// Adaptive downsampling: when stride_ms is set, return one
		// snapshot per N-ms bucket (the latest in each bucket) so wide
		// windows don't blow the browser. Default unset = raw stream.
		strideMs := 0
		if v := r.URL.Query().Get("stride_ms"); v != "" {
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 && n <= 60000 {
				strideMs = n
			}
		}
		var query string
		if strideMs > 0 {
			// argMax(session_json, ts) within each bucket picks the latest
			// row in the bucket, preserving monotonic counters. We name the
			// inner aggregates with a `_max` suffix so the inner `ts` in
			// GROUP BY still refers to the underlying DateTime64 column —
			// otherwise ClickHouse resolves it to the argMax aggregate and
			// rejects the query (ILLEGAL_AGGREGATION).
			query = fmt.Sprintf(`
				SELECT toString(ts_max) AS ts, session_json_max AS session_json FROM (
				  SELECT
				    argMax(ts, ts) AS ts_max,
				    argMax(session_json, ts) AS session_json_max
				  FROM %s.%s
				  WHERE %s
				  GROUP BY intDiv(toUnixTimestamp64Milli(ts), %d)
				)
				ORDER BY ts_max %s
				LIMIT %d
				FORMAT JSONEachRow`, cfg.chDatabase, cfg.chTable, strings.Join(clauses, " AND "), strideMs, order, limit)
		} else {
			// Don't alias the projection as `ts` — that would shadow the
			// real DateTime64 column for the WHERE clause's reference,
			// breaking the from/to comparison ("No operation greaterOrEquals
			// between String and DateTime64(3)"). Wrap in a subquery and
			// rename in the outer SELECT instead.
			query = fmt.Sprintf(`
				SELECT toString(ts_raw) AS ts, session_json FROM (
				  SELECT
				    ts AS ts_raw,
				    session_json
				  FROM %s.%s
				  WHERE %s
				  ORDER BY ts %s
				  LIMIT %d
				)
				FORMAT JSONEachRow`, cfg.chDatabase, cfg.chTable, strings.Join(clauses, " AND "), order, limit)
		}
		proxyClickHouseJSON(w, r, cfg, query, params)
	})

	// Health heatmap: returns N time buckets with bad-event counts so
	// the session-viewer can paint a colored mini-map above the scrubber.
	// GET /api/session_heatmap?session=<id>&play_id=<id>&buckets=<N>
	mux.HandleFunc("/api/session_heatmap", func(w http.ResponseWriter, r *http.Request) {
		session := r.URL.Query().Get("session")
		if session == "" {
			http.Error(w, "missing session", http.StatusBadRequest)
			return
		}
		params := map[string]string{"session": session}
		clauses := []string{"session_id = {session:String}"}
		if v := r.URL.Query().Get("play_id"); v != "" {
			if v == "—" {
				clauses = append(clauses, "play_id = ''")
			} else {
				clauses = append(clauses, "play_id = {play:String}")
				params["play"] = canonicalV2ID(v)
			}
		}
		buckets := 120
		if v := r.URL.Query().Get("buckets"); v != "" {
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 10 && n <= 1000 {
				buckets = n
			}
		}
		// Compute bucket size from session bounds so the strip always
		// has exactly `buckets` cells regardless of session duration.
		query := fmt.Sprintf(`
			WITH bounds AS (
			  SELECT
			    toUnixTimestamp64Milli(min(ts)) AS start_ms,
			    toUnixTimestamp64Milli(max(ts)) AS end_ms
			  FROM %s.%s WHERE %s
			),
			sized AS (
			  SELECT start_ms, end_ms, greatest(1, intDiv(end_ms - start_ms, %d)) AS bucket_ms FROM bounds
			),
			base AS (
			  SELECT
			    toUnixTimestamp64Milli(ts) AS ts_ms,
			    intDiv(toUnixTimestamp64Milli(ts) - (SELECT start_ms FROM sized),
			           (SELECT bucket_ms FROM sized)) AS bucket,
			    stall_count, frames_dropped, player_error, transport_fault_active,
			    manifest_failure_type, segment_failure_type, all_failure_type,
			    video_bitrate_mbps,
			    lagInFrame(stall_count, 1, stall_count)             OVER w AS prev_stall,
			    lagInFrame(frames_dropped, 1, frames_dropped)       OVER w AS prev_drops,
			    lagInFrame(video_bitrate_mbps, 1, video_bitrate_mbps) OVER w AS prev_bitrate
			  FROM %s.%s
			  WHERE %s
			  WINDOW w AS (ORDER BY ts)
			)
			SELECT
			  (SELECT start_ms FROM sized) + bucket * (SELECT bucket_ms FROM sized) AS bucket_start_ms,
			  (SELECT bucket_ms FROM sized) AS bucket_size_ms,
			  countIf(stall_count > prev_stall)                                                  AS stalls,
			  countIf(player_error != '')                                                        AS error_rows,
			  countIf(transport_fault_active = 1
			          OR (manifest_failure_type != '' AND manifest_failure_type != 'none')
			          OR (segment_failure_type  != '' AND segment_failure_type  != 'none')
			          OR (all_failure_type      != '' AND all_failure_type      != 'none'))     AS fault_rows,
			  countIf(video_bitrate_mbps < prev_bitrate AND prev_bitrate > 0 AND video_bitrate_mbps > 0) AS downshifts,
			  max(frames_dropped) - min(frames_dropped)                                          AS drops
			FROM base
			GROUP BY bucket
			ORDER BY bucket
			FORMAT JSONEachRow`,
			cfg.chDatabase, cfg.chTable, strings.Join(clauses, " AND "),
			buckets,
			cfg.chDatabase, cfg.chTable, strings.Join(clauses, " AND "))
		proxyClickHouseJSON(w, r, cfg, query, params)
	})

	// session_markers retired in issue #474 Milestone C. Notable
	// events now live as `labels[]` on session_events / network_requests
	// rows and as discrete rows on control_events; consumers iterate
	// those three tables instead of the derived markers table.

	// /api/session_bundle — ZIP of snapshots + HAR + events for one
	// (session_id, play_id). Defined in bundle.go.
	registerBundleHandler(mux, cfg)

	// /api/v2/characterization-runs — POST ingest + GET list/detail for
	// reports the Go test framework uploads at end-of-sweep. Defined
	// in characterization.go.
	registerCharacterizationHandlers(mux, cfg)

	srv := &http.Server{
		Addr:              cfg.httpListen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()
	log.Printf("http api listening on %s", cfg.httpListen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("http server: %v", err)
	}
}

// proxyClickHouseJSON forwards a SQL query to ClickHouse's HTTP
// interface and streams the JSONEachRow response back to the caller.
// User-supplied values must be passed via `params` (a map of name →
// stringified value) and referenced in the SQL with `{name:Type}`
// placeholders — never interpolated into the query string. ClickHouse
// binds the parameters server-side, eliminating injection risk.
func proxyClickHouseJSON(w http.ResponseWriter, r *http.Request, cfg config, query string, params map[string]string) {
	u, err := url.Parse(cfg.clickhouseURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	qs := u.Query()
	qs.Set("query", query)
	qs.Set("default_format", "JSONEachRow")
	for k, v := range params {
		qs.Set("param_"+k, v)
	}
	u.RawQuery = qs.Encode()
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if cfg.chUser != "" {
		req.SetBasicAuth(cfg.chUser, cfg.chPassword)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		http.Error(w, strings.TrimSpace(string(body)), resp.StatusCode)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no") // tell nginx not to buffer
	// Flush as ClickHouse delivers rows so the browser sees lines as
	// they're produced, not at end-of-response. ClickHouse already emits
	// JSONEachRow incrementally; without flushing the std http.Server
	// would buffer up to its default chunk size before sending.
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
