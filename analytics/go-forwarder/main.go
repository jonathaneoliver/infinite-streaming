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
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

type config struct {
	sseURL        string
	clickhouseURL string
	chDatabase    string
	chTable       string
	chUser        string
	chPassword    string
	flushEvery    time.Duration
	flushBatch    int
	httpListen    string
}

func loadConfig() config {
	c := config{
		sseURL:        getenv("FORWARDER_SSE_URL", "http://go-server:30081/api/sessions/stream"),
		clickhouseURL: getenv("FORWARDER_CLICKHOUSE_URL", "http://clickhouse:8123"),
		chDatabase:    getenv("FORWARDER_CLICKHOUSE_DB", "infinite_streaming"),
		chTable:       getenv("FORWARDER_CLICKHOUSE_TABLE", "session_snapshots"),
		chUser:        getenv("FORWARDER_CLICKHOUSE_USER", "default"),
		chPassword:    getenv("FORWARDER_CLICKHOUSE_PASSWORD", ""),
		// flushEvery is the upper bound on per-row visibility lag — the
		// inserter empties whichever happens first (timer or batch
		// fills). 250ms keeps the picker's "x seconds ago" honest
		// without significantly raising ClickHouse insert pressure.
		flushEvery:    250 * time.Millisecond,
		flushBatch:    500,
		httpListen:    getenv("FORWARDER_HTTP_LISTEN", ":8080"),
	}
	return c
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
	PlayerID                 string  `json:"player_id"`
	GroupID                  string  `json:"group_id"`
	UserAgent                string  `json:"user_agent"`
	ManifestURL              string  `json:"manifest_url"`
	ManifestVariants         string  `json:"manifest_variants"`
	LastRequestURL           string  `json:"last_request_url"`
	ContentID                string  `json:"content_id"`
	PlayerState              string  `json:"player_state"`
	WaitingReason            string  `json:"waiting_reason"`
	BufferDepthS             float32 `json:"buffer_depth_s"`
	NetworkBitrateMbps       float32 `json:"network_bitrate_mbps"`
	VideoBitrateMbps         float32 `json:"video_bitrate_mbps"`
	MeasuredMbps             float32 `json:"measured_mbps"`
	MbpsShaperRate           float32 `json:"mbps_shaper_rate"`
	MbpsShaperAvg            float32 `json:"mbps_shaper_avg"`
	DisplayResolution        string  `json:"display_resolution"`
	VideoResolution          string  `json:"video_resolution"`
	FramesDisplayed          uint64  `json:"frames_displayed"`
	DroppedFrames            uint32  `json:"dropped_frames"`
	StallCount               uint32  `json:"stall_count"`
	StallTimeS               float32 `json:"stall_time_s"`
	PositionS                float32 `json:"position_s"`
	LiveEdgeS                float32 `json:"live_edge_s"`
	TrueOffsetS              float32 `json:"true_offset_s"`
	PlaybackRate             float32 `json:"playback_rate"`
	LoopCountPlayer          uint32  `json:"loop_count_player"`
	LoopCountServer          uint32  `json:"loop_count_server"`
	LastEvent                string  `json:"last_event"`
	LastEventAt              string  `json:"last_event_at"`
	TriggerType              string  `json:"trigger_type"`
	EventTime                string  `json:"event_time"`
	PlayerError              string  `json:"player_error"`

	AvgNetworkBitrateMbps    float32 `json:"avg_network_bitrate_mbps"`
	BufferEndS               float32 `json:"buffer_end_s"`
	LastStallTimeS           float32 `json:"last_stall_time_s"`
	LiveOffsetS              float32 `json:"live_offset_s"`
	PlayheadWallclockMs      int64   `json:"playhead_wallclock_ms"`
	SeekableEndS             float32 `json:"seekable_end_s"`
	MetricsSource            string  `json:"metrics_source"`
	VideoFirstFrameTimeS     float32 `json:"video_first_frame_time_s"`
	VideoQualityPct          float32 `json:"video_quality_pct"`
	VideoStartTimeS          float32 `json:"video_start_time_s"`

	MbpsTransferComplete     float32 `json:"mbps_transfer_complete"`
	MbpsTransferRate         float32 `json:"mbps_transfer_rate"`
	PlayerIP                 string  `json:"player_ip"`
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
	ControlRevision  uint64 `json:"control_revision"`
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("shutdown signal received")
		cancel()
	}()

	rows := make(chan row, 4096)
	go batchInserter(ctx, cfg, rows)
	go serveHTTP(ctx, cfg)

	// Network log archival: subscribe to go-proxy's /api/network/stream
	// SSE endpoint and forward each per-request event to ClickHouse so
	// the session-viewer can replay them after the session is gone.
	netRows := make(chan netRow, 8192)
	netSeenSet := newNetSeen(50000)
	go batchInsertNet(ctx, cfg, netRows)
	go runNetworkStream(ctx, cfg, netSeenSet, netRows)

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
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	active := make(map[string]struct{}, len(payload.Sessions))
	for _, s := range payload.Sessions {
		sessionID, _ := s["session_id"].(string)
		if sessionID == "" {
			continue
		}
		active[sessionID] = struct{}{}
		fp := fingerprint(s)
		if !cache.changed(sessionID, fp) {
			continue
		}
		out <- toRow(now, payload.Revision, sessionID, s)
		// Queue auto-classifier if this snapshot carries any of the
		// "really bad things" signals. Debounced — repeated marks
		// for the same (session,play) coalesce to one mutation.
		if classifyQ != nil && hasInterestingSignal(s) {
			classifyQ.mark(sessionID, getStr(s, "play_id"))
		}
	}
	cache.prune(active)
	// Free network-log fingerprint memory for sessions that have aged
	// out of the SSE stream.
	if netSeen != nil {
		netSeen.prune(active)
	}
}

func fingerprint(s map[string]interface{}) string {
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func toRow(ts string, revision uint64, sessionID string, s map[string]interface{}) row {
	full, _ := json.Marshal(s)
	return row{
		Ts:                       ts,
		Revision:                 revision,
		SessionID:                sessionID,
		PlayID:                   getStr(s, "play_id"),
		PlayerID:                 getStr(s, "player_id"),
		GroupID:                  getStr(s, "group_id"),
		UserAgent:                getStr(s, "user_agent"),
		ManifestURL:              getStr(s, "manifest_url"),
		ManifestVariants:         getJSON(s, "manifest_variants"),
		LastRequestURL:           getStr(s, "last_request_url"),
		ContentID:                contentIDFromURL(getStr(s, "manifest_url"), getStr(s, "last_request_url")),
		PlayerState:              getStr(s, "player_metrics_state"),
		WaitingReason:            getStr(s, "player_metrics_waiting_reason"),
		BufferDepthS:             getF32(s, "player_metrics_buffer_depth_s"),
		NetworkBitrateMbps:       getF32(s, "player_metrics_network_bitrate_mbps"),
		VideoBitrateMbps:         getF32(s, "player_metrics_video_bitrate_mbps"),
		MeasuredMbps:             getF32(s, "measured_mbps"),
		MbpsShaperRate:           getF32(s, "mbps_shaper_rate"),
		MbpsShaperAvg:            getF32(s, "mbps_shaper_avg"),
		DisplayResolution:        getStr(s, "player_metrics_display_resolution"),
		VideoResolution:          getStr(s, "player_metrics_video_resolution"),
		FramesDisplayed:          getU64(s, "player_metrics_frames_displayed"),
		DroppedFrames:            uint32(getU64(s, "player_metrics_dropped_frames")),
		StallCount:               uint32(getU64(s, "player_metrics_stall_count")),
		StallTimeS:               getF32(s, "player_metrics_stall_time_s"),
		PositionS:                getF32(s, "player_metrics_position_s"),
		LiveEdgeS:                getF32(s, "player_metrics_live_edge_s"),
		TrueOffsetS:              getF32(s, "player_metrics_true_offset_s"),
		PlaybackRate:             getF32(s, "player_metrics_playback_rate"),
		LoopCountPlayer:          uint32(getU64(s, "loop_count_player")),
		LoopCountServer:          uint32(getU64(s, "loop_count_server")),
		LastEvent:                getStr(s, "player_metrics_last_event"),
		LastEventAt:              getStr(s, "player_metrics_last_event_at"),
		TriggerType:              getStr(s, "player_metrics_trigger_type"),
		EventTime:                getStr(s, "player_metrics_event_time"),
		PlayerError:              getStr(s, "player_metrics_error"),

		AvgNetworkBitrateMbps: getF32(s, "player_metrics_avg_network_bitrate_mbps"),
		BufferEndS:            getF32(s, "player_metrics_buffer_end_s"),
		LastStallTimeS:        getF32(s, "player_metrics_last_stall_time_s"),
		LiveOffsetS:           getF32(s, "player_metrics_live_offset_s"),
		PlayheadWallclockMs:   int64(getU64(s, "player_metrics_playhead_wallclock_ms")),
		SeekableEndS:          getF32(s, "player_metrics_seekable_end_s"),
		MetricsSource:         getStr(s, "player_metrics_source"),
		VideoFirstFrameTimeS:  getF32(s, "player_metrics_video_first_frame_time_s"),
		VideoQualityPct:       getF32(s, "player_metrics_video_quality_pct"),
		VideoStartTimeS:       getF32(s, "player_metrics_video_start_time_s"),

		MbpsTransferComplete:   getF32(s, "mbps_transfer_complete"),
		MbpsTransferRate:       getF32(s, "mbps_transfer_rate"),
		PlayerIP:               getStr(s, "player_ip"),
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
		ControlRevision: getU64(s, "control_revision"),
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

func batchInserter(ctx context.Context, cfg config, in <-chan row) {
	buf := make([]row, 0, cfg.flushBatch)
	tick := time.NewTicker(cfg.flushEvery)
	defer tick.Stop()
	flush := func() {
		if len(buf) == 0 {
			return
		}
		if err := insert(ctx, cfg, buf); err != nil {
			log.Printf("insert failed (%d rows dropped): %v", len(buf), err)
		}
		buf = buf[:0]
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
			buf = append(buf, r)
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
//	GET /api/sessions?since=<rfc3339>
//	GET /api/snapshots?session=<id>&from=<rfc3339>&to=<rfc3339>&limit=<n>
//	GET /healthz
func serveHTTP(ctx context.Context, cfg config) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		since := r.URL.Query().Get("since")
		until := r.URL.Query().Get("until")
		params := map[string]string{}
		var clauses []string
		if since != "" {
			clauses = append(clauses, "ts >= parseDateTime64BestEffort({since:String})")
			params["since"] = since
		} else {
			clauses = append(clauses, "ts >= now() - INTERVAL 24 HOUR")
		}
		if until != "" {
			clauses = append(clauses, "ts <= parseDateTime64BestEffort({until:String})")
			params["until"] = until
		}
		where := "WHERE " + strings.Join(clauses, " AND ")
		// Per-(session_id, play_id) row so the Session Viewer picker
		// can offer per-playback granularity. A session that hosted
		// three loadStream() calls produces three rows here, one per
		// fresh play_id. Empty play_ids (rows ingested before go-proxy
		// started stamping the field) are surfaced as "—" so they
		// remain selectable rather than collapsing into one bucket.
		// Per-(session_id, play_id) summary. We pre-window the rows so we
		// can count ABR shifts (bitrate / resolution changes) using
		// lagInFrame — the *count* of shifts isn't stored directly but
		// the per-snapshot value is, and a transition between adjacent
		// snapshots is a shift event.
		query := fmt.Sprintf(`
			WITH base AS (
			  SELECT
			    session_id, play_id, ts,
			    player_id, group_id, content_id,
			    player_state, player_error, last_event,
			    stall_count, dropped_frames, frames_displayed,
			    video_bitrate_mbps, video_resolution, video_quality_pct,
			    video_first_frame_time_s,
			    master_manifest_consecutive_failures,
			    manifest_consecutive_failures,
			    segment_consecutive_failures,
			    all_consecutive_failures,
			    transport_consecutive_failures,
			    fault_count_transfer_active_timeout,
			    fault_count_transfer_idle_timeout,
			    classification,
			    lagInFrame(video_bitrate_mbps, 1, video_bitrate_mbps) OVER w AS prev_bitrate,
			    lagInFrame(video_resolution,   1, video_resolution)   OVER w AS prev_resolution
			  FROM %s.%s
			  %s
			  WINDOW w AS (PARTITION BY session_id, play_id ORDER BY ts)
			),
			net_counts AS (
			  SELECT session_id, play_id, count() AS net_rows,
			         countIf(status >= 400) AS net_errors,
			         countIf(faulted = 1)  AS net_faults
			  FROM %s.network_requests
			  %s
			  GROUP BY session_id, play_id
			),
			agg AS (
			  SELECT
			    session_id,
			    play_id AS raw_play_id,
			    any(player_id) AS player_id,
			    any(group_id) AS group_id,
			    any(content_id) AS content_id,
			    min(ts) AS started,
			    max(ts) AS last_seen,
			    count() AS metric_events,
			    max(stall_count) AS stalls,
			    max(dropped_frames) AS dropped_frames,
			    argMax(player_state, ts) AS last_state,
			    argMax(player_error, ts) AS last_player_error,
			    max(master_manifest_consecutive_failures) AS master_manifest_failures,
			    max(manifest_consecutive_failures) AS manifest_failures,
			    max(segment_consecutive_failures) AS segment_failures,
			    max(all_consecutive_failures) AS all_failures,
			    max(transport_consecutive_failures) AS transport_failures,
			    max(fault_count_transfer_active_timeout) AS active_timeouts,
			    max(fault_count_transfer_idle_timeout) AS idle_timeouts,
			    countIf(video_bitrate_mbps != prev_bitrate AND video_bitrate_mbps > 0 AND prev_bitrate > 0)                       AS bitrate_shifts,
			    countIf(video_bitrate_mbps < prev_bitrate AND prev_bitrate > 0 AND video_bitrate_mbps > 0)                         AS downshifts,
			    countIf(video_bitrate_mbps > prev_bitrate AND prev_bitrate > 0 AND video_bitrate_mbps > 0)                         AS upshifts,
			    countIf(video_resolution != prev_resolution AND video_resolution != '' AND prev_resolution != '')                  AS resolution_changes,
			    round(avgIf(video_quality_pct, video_quality_pct > 0), 1) AS avg_quality_pct,
			    round(minIf(video_quality_pct, video_quality_pct > 0), 1) AS min_quality_pct,
			    max(frames_displayed) AS frames_displayed,
			    round(max(video_first_frame_time_s), 2) AS first_frame_s,
			    -- Per-event-type counts of "really bad things" so the
			    -- session picker can flag rows distinctly.
			    --   user_marked  → operator-pressed 911 button
			    --   frozen       → picture frozen (≠ stall: stall is
			    --                  buffer-empty, frozen is renderer
			    --                  hung)
			    --   segment_stall → stall waiting for a segment fetch
			    --   restart      → mid-session restart (player-side
			    --                  recovery attempt)
			    --   error        → explicit player_error event
			    countIf(last_event = 'user_marked')   AS user_marked_count,
			    countIf(last_event = 'frozen')         AS frozen_count,
			    countIf(last_event = 'segment_stall')  AS segment_stall_count,
			    countIf(last_event = 'restart')        AS restart_count,
			    countIf(last_event = 'error')          AS error_event_count,
			    -- Tiered retention class (issue #342). Same value on
			    -- every row of (session_id, play_id) once the
			    -- forwarder's session-end / star path stamps it; before
			    -- that, default 'other'. any() is fine here because
			    -- once the mutation has settled all rows agree.
			    any(classification) AS classification
			  FROM base
			  GROUP BY session_id, play_id
			)
			SELECT
			  agg.session_id AS session_id,
			  if(agg.raw_play_id = '', '—', agg.raw_play_id) AS play_id,
			  agg.player_id, agg.group_id, agg.content_id,
			  agg.started, agg.last_seen,
			  agg.metric_events,
			  agg.metric_events AS rows,
			  ifNull(net_counts.net_rows,   0) AS net_events,
			  ifNull(net_counts.net_errors, 0) AS net_errors,
			  ifNull(net_counts.net_faults, 0) AS net_faults,
			  agg.stalls, agg.dropped_frames, agg.last_state, agg.last_player_error,
			  agg.master_manifest_failures, agg.manifest_failures, agg.segment_failures,
			  agg.all_failures, agg.transport_failures, agg.active_timeouts, agg.idle_timeouts,
			  agg.bitrate_shifts, agg.downshifts, agg.upshifts, agg.resolution_changes,
			  agg.avg_quality_pct, agg.min_quality_pct,
			  agg.frames_displayed, agg.first_frame_s,
			  agg.user_marked_count, agg.frozen_count, agg.segment_stall_count,
			  agg.restart_count, agg.error_event_count,
			  agg.classification
			FROM agg
			LEFT JOIN net_counts
			  ON agg.session_id = net_counts.session_id
			 AND agg.raw_play_id = net_counts.play_id
			ORDER BY agg.started DESC
			LIMIT 1000
			FORMAT JSONEachRow`, cfg.chDatabase, cfg.chTable, where, cfg.chDatabase, where)
		proxyClickHouseJSON(w, r, cfg, query, params)
	})
	mux.HandleFunc("/api/snapshot_count", func(w http.ResponseWriter, r *http.Request) {
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
				params["play"] = v
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
		query := fmt.Sprintf(`
			SELECT
			  count() AS count,
			  toString(min(ts)) AS first_ts,
			  toString(max(ts)) AS last_ts
			FROM %s.%s
			WHERE %s
			FORMAT JSONEachRow`, cfg.chDatabase, cfg.chTable, strings.Join(clauses, " AND "))
		proxyClickHouseJSON(w, r, cfg, query, params)
	})
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
				params["play"] = v
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
				params["play"] = v
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
			    stall_count, dropped_frames, player_error, transport_fault_active,
			    manifest_failure_type, segment_failure_type, all_failure_type,
			    video_bitrate_mbps,
			    lagInFrame(stall_count, 1, stall_count)             OVER w AS prev_stall,
			    lagInFrame(dropped_frames, 1, dropped_frames)       OVER w AS prev_drops,
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
			  max(dropped_frames) - min(dropped_frames)                                          AS drops
			FROM base
			GROUP BY bucket
			ORDER BY bucket
			FORMAT JSONEachRow`,
			cfg.chDatabase, cfg.chTable, strings.Join(clauses, " AND "),
			buckets,
			cfg.chDatabase, cfg.chTable, strings.Join(clauses, " AND "))
		proxyClickHouseJSON(w, r, cfg, query, params)
	})

	// Notable session events for the jump-list. Returns rows like
	//   { ts, type: 'stall'|'error'|'fault_on'|'downshift', info }
	// sorted by ts so the UI can render them as a chronological list.
	// GET /api/session_events?session=<id>&play_id=<id>&limit=<n>
	mux.HandleFunc("/api/session_events", func(w http.ResponseWriter, r *http.Request) {
		session := r.URL.Query().Get("session")
		if session == "" {
			http.Error(w, "missing session", http.StatusBadRequest)
			return
		}
		params := map[string]string{"session": session}
		clauses := []string{"session_id = {session:String}"}
		// HAR events come from network_requests, which on iOS often
		// has play_id='' on variant/segment URLs (the iOS player
		// strips the query param). Filter HAR by time range derived
		// from session_snapshots instead of play_id directly.
		// We qualify ts as `nr.ts` because the inner sub-selects
		// alias `toString(ts) AS ts` and that String alias would
		// otherwise shadow the column in the WHERE clause.
		harClauses := []string{"session_id = {session:String}"}
		if v := r.URL.Query().Get("play_id"); v != "" {
			var pidPred string
			if v == "—" {
				pidPred = "play_id = ''"
			} else {
				pidPred = "play_id = {play:String}"
				params["play"] = v
			}
			clauses = append(clauses, pidPred)
			harClauses = append(harClauses, fmt.Sprintf(
				"nr.ts BETWEEN (SELECT min(ts) FROM %s.%s WHERE session_id = {session:String} AND %s) "+
					"AND (SELECT max(ts) FROM %s.%s WHERE session_id = {session:String} AND %s)",
				cfg.chDatabase, cfg.chTable, pidPred,
				cfg.chDatabase, cfg.chTable, pidPred))
		}
		limit := 500
		if v := r.URL.Query().Get("limit"); v != "" {
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 && n <= 5000 {
				limit = n
			}
		}
		where := strings.Join(clauses, " AND ")
		harWhere := strings.Join(harClauses, " AND ")
		// Event taxonomy:
		//   - Player-emitted events come from the last_event column directly
		//     (the player tells us "buffering_start", "rate_shift_down", etc.)
		//   - Stall vs buffering are kept distinct: AVPlayer (iOS) emits both
		//     and they mean different things; Chrome/HLS.js only emits stall_*
		//   - Stall and buffering durations are computed by pairing onsets
		//     with the next end event in the same family (stall vs buffer)
		//   - Server-side counters (faults, manifest/segment failures) are
		//     not surfaced by the player, so we keep lagInFrame for those
		//
		// Priority (computed in outer SELECT):
		//   P1 Critical : error, master/all failure, stall ≥ 3s
		//   P2 High     : stall < 3s, restart, manifest/segment/transport_failure,
		//                 transfer_*_timeout
		//   P3 Medium   : downshift, fault_on, timejump, buffering
		//   P4 Low      : upshift, fault_off, playback_start
		query := fmt.Sprintf(`
			WITH stall_or_buffer_pairs AS (
			  SELECT start_ts, start_event, duration_s
			  FROM (
			    SELECT
			      ts AS start_ts,
			      last_event AS start_event,
			      multiIf(
			        last_event IN ('stall_start','stall_end'),         'stall',
			        last_event IN ('buffering_start','buffering_end'), 'buffer',
			        ''
			      ) AS family,
			      leadInFrame(last_event, 1, '') OVER w AS next_event,
			      dateDiff('millisecond', ts, leadInFrame(ts, 1, ts) OVER w) / 1000.0 AS duration_s
			    FROM %s.%s
			    WHERE %s
			      AND last_event IN ('stall_start','stall_end','buffering_start','buffering_end')
			    WINDOW w AS (PARTITION BY family ORDER BY ts
			                 ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)
			  )
			  -- Only emit when the lead landed on the matching close event;
			  -- otherwise we'd surface the gap to the NEXT start (which can
			  -- be hours away — happens when an _end was never received).
			  WHERE (start_event = 'stall_start'     AND next_event = 'stall_end')
			     OR (start_event = 'buffering_start' AND next_event = 'buffering_end')
			),
			rate_shifts AS (
			  SELECT ts, last_event, video_bitrate_mbps,
			         lagInFrame(video_bitrate_mbps, 1, video_bitrate_mbps) OVER w AS prev_bitrate
			  FROM %s.%s
			  WHERE %s
			  WINDOW w AS (ORDER BY ts)
			),
			base AS (
			  SELECT
			    ts,
			    player_error, transport_fault_active,
			    manifest_consecutive_failures,
			    segment_consecutive_failures,
			    master_manifest_consecutive_failures,
			    all_consecutive_failures,
			    transport_consecutive_failures,
			    fault_count_transfer_active_timeout,
			    fault_count_transfer_idle_timeout,
			    loop_count_server,
			    -- row_number used to suppress first-snapshot artifacts: the
			    -- first row per play has no real prior row, so the lag-default
			    -- would otherwise spurious-fire if a counter is non-zero on
			    -- entry (e.g. transport_consecutive_failures = 1 from a prior
			    -- session leaving state behind).
			    row_number() OVER w AS rn,
			    lagInFrame(player_error, 1, '')                       OVER w AS prev_error,
			    lagInFrame(transport_fault_active, 1, 0)              OVER w AS prev_fault,
			    lagInFrame(manifest_consecutive_failures, 1, 0)       OVER w AS prev_manifest_fail,
			    lagInFrame(segment_consecutive_failures, 1, 0)        OVER w AS prev_segment_fail,
			    lagInFrame(master_manifest_consecutive_failures, 1, 0) OVER w AS prev_master_fail,
			    lagInFrame(all_consecutive_failures, 1, 0)            OVER w AS prev_all_fail,
			    lagInFrame(transport_consecutive_failures, 1, 0)      OVER w AS prev_transport_fail,
			    lagInFrame(fault_count_transfer_active_timeout, 1, 0) OVER w AS prev_active_to,
			    lagInFrame(fault_count_transfer_idle_timeout, 1, 0)   OVER w AS prev_idle_to,
			    lagInFrame(loop_count_server, 1, 0)                   OVER w AS prev_loop_server
			  FROM %s.%s
			  WHERE %s
			  WINDOW w AS (PARTITION BY play_id ORDER BY ts)
			)
			SELECT
			  ts, type, info,
			  -- 'cause' = proxy / system action that *might* produce a user-visible
			  -- effect (fault injection, server-side failure counters, timeouts).
			  -- 'effect' = something the player or user actually saw (stall, error,
			  -- bitrate change, restart). Causes default to P3 (diagnostic context)
			  -- unless they are the kill-switch types that by design break playback.
			  multiIf(
			    -- Inverted classification: explicitly enumerate KNOWN
			    -- causes (proxy/system actions); everything else —
			    -- including unrecognised player-emitted events from
			    -- the catch-all UNION ALL — defaults to 'effect'.
			    type IN ('master_manifest_failure', 'all_failure',
			             'manifest_failure', 'segment_failure',
			             'transport_failure',
			             'transfer_active_timeout', 'transfer_idle_timeout',
			             'fault_on', 'fault_off',
			             'http_5xx', 'http_4xx',
			             'request_timeout', 'request_incomplete', 'request_faulted',
			             'slow_request', 'slow_segment', 'request_retry',
			             'loop_server'), 'cause',
			    'effect'
			  ) AS kind,
			  multiIf(
			    -- User-marked moments are explicit "this matters" flags
			    -- from a human watching live — always P1 so they don't
			    -- get hidden by the default Critical+High+Medium chip
			    -- selection.
			    type = 'user_marked', 1,
			    type = 'error', 1,
			    -- These two causes break playback by design — stay Critical.
			    type IN ('master_manifest_failure', 'all_failure'), 1,
			    type = 'stall' AND duration_s >= 3, 1,
			    type = 'stall', 2,
			    type = 'restart', 2,
			    -- Other causes are diagnostic context, not Critical/High by default.
			    type IN ('manifest_failure', 'segment_failure',
			             'transport_failure',
			             'transfer_active_timeout', 'transfer_idle_timeout',
			             'fault_on', 'fault_off'), 3,
			    type IN ('downshift', 'timejump', 'buffering'), 3,
			    -- HAR-derived events. http_5xx & request_timeout are
			    -- High because they indicate real failures that often
			    -- precede a stall. The rest are diagnostic Medium /
			    -- Low (retries especially are very chatty).
			    type IN ('http_5xx', 'request_timeout'), 2,
			    type IN ('http_4xx', 'request_incomplete', 'request_faulted',
			             'slow_request', 'slow_segment'), 3,
			    type = 'request_retry', 4,
			    type IN ('upshift', 'playback_start'), 4,
			    -- Server loop boundaries are routine in this testing
			    -- platform (the stream loops the source repeatedly).
			    -- P4 keeps them available for triage but hidden by
			    -- default so they don't drown out real signals.
			    type = 'loop_server', 4,
			    -- Default for unrecognised types: Medium so they're
			    -- visible (vs P4 Low which is off by default).
			    3
			  ) AS priority
			FROM (
			  -- Stalls (paired stall_start → stall_end)
			  SELECT toString(start_ts) AS ts, 'stall' AS type,
			         concat(toString(round(duration_s, 2)), 's') AS info,
			         duration_s
			  FROM stall_or_buffer_pairs
			  WHERE start_event = 'stall_start' AND duration_s > 0

			  UNION ALL
			  -- Buffering (paired buffering_start → buffering_end). Distinct
			  -- from stall: AVPlayer emits both as separate notifications.
			  SELECT toString(start_ts) AS ts, 'buffering' AS type,
			         concat(toString(round(duration_s, 2)), 's') AS info,
			         duration_s
			  FROM stall_or_buffer_pairs
			  WHERE start_event = 'buffering_start' AND duration_s > 0

			  UNION ALL
			  -- frozen / segment_stall: no explicit *_end emitted, so duration
			  -- is unknown — emit as zero-duration stall events. frozen is
			  -- always Critical via a hard-coded subtype hint in info.
			  SELECT toString(ts) AS ts, 'stall' AS type,
			         if(last_event = 'frozen', '(frozen)', '(segment)') AS info,
			         0 AS duration_s
			  FROM %s.%s WHERE %s AND last_event IN ('frozen', 'segment_stall')

			  UNION ALL
			  -- Restart, playback_start, downshift, upshift, timejump, error
			  -- (player-emitted directly via last_event)
			  SELECT toString(ts) AS ts, 'restart' AS type, '' AS info, 0 AS duration_s
			  FROM %s.%s WHERE %s AND last_event = 'restart'
			  UNION ALL
			  SELECT toString(ts) AS ts, 'playback_start' AS type, '' AS info, 0
			  FROM %s.%s WHERE %s AND last_event IN ('video_start_time', 'video_first_frame')
			  UNION ALL
			  SELECT toString(ts) AS ts, 'downshift' AS type,
			         concat(toString(round(prev_bitrate, 2)), '→', toString(round(video_bitrate_mbps, 2)), ' Mbps') AS info,
			         0
			  FROM rate_shifts WHERE last_event = 'rate_shift_down' AND prev_bitrate > 0 AND video_bitrate_mbps > 0
			  UNION ALL
			  SELECT toString(ts) AS ts, 'upshift' AS type,
			         concat(toString(round(prev_bitrate, 2)), '→', toString(round(video_bitrate_mbps, 2)), ' Mbps') AS info,
			         0
			  FROM rate_shifts WHERE last_event = 'rate_shift_up' AND prev_bitrate > 0 AND video_bitrate_mbps > 0
			  UNION ALL
			  SELECT toString(ts) AS ts, 'timejump' AS type, '' AS info, 0
			  FROM %s.%s WHERE %s AND last_event = 'timejump'
			  UNION ALL
			  SELECT toString(ts) AS ts, 'error' AS type, player_error AS info, 0
			  FROM %s.%s WHERE %s AND last_event = 'error'
			  UNION ALL
			  -- User-marked moments — the iOS / iPadOS player has a
			  -- "911" button that fires last_event='user_marked' so an
			  -- operator viewing live can flag "something interesting
			  -- just happened" without us having to detect what. Always
			  -- surface as Critical so it's never hidden by chip filter.
			  SELECT toString(ts) AS ts, 'user_marked' AS type, '' AS info, 0
			  FROM %s.%s WHERE %s AND last_event = 'user_marked'
			  UNION ALL
			  -- Catch-all: any last_event value we haven't explicitly
			  -- mapped above shows up here verbatim. Lets new player
			  -- events appear in the dropdown without a forwarder
			  -- redeploy. 'heartbeat' / 'state_change' / 'playing' /
			  -- 'video_bitrate_change' are intentionally excluded as
			  -- noise. The explicitly-handled types are also excluded
			  -- so we don't double-emit.
			  SELECT toString(ts) AS ts, last_event AS type, '' AS info, 0
			  FROM %s.%s WHERE %s
			    AND last_event != ''
			    AND last_event NOT IN (
			      'heartbeat', 'state_change', 'playing', 'video_bitrate_change',
			      'stall_start', 'stall_end',
			      'buffering_start', 'buffering_end',
			      'frozen', 'segment_stall',
			      'restart', 'video_first_frame', 'video_start_time',
			      'rate_shift_down', 'rate_shift_up',
			      'timejump', 'error', 'user_marked'
			    )

			  -- Server-side state-change events (lag-based — player can't
			  -- tell us about these). All filters require rn > 1 so the
			  -- first snapshot per play (where the lag-default of 0 / ''
			  -- would falsely trigger if the counter started non-zero) is
			  -- suppressed.
			  UNION ALL
			  SELECT toString(ts) AS ts, 'error' AS type, player_error AS info, 0
			  FROM base WHERE rn > 1 AND player_error != '' AND prev_error != player_error
			  UNION ALL
			  SELECT toString(ts) AS ts, 'master_manifest_failure' AS type, '' AS info, 0
			  FROM base WHERE rn > 1 AND master_manifest_consecutive_failures > prev_master_fail
			  UNION ALL
			  SELECT toString(ts) AS ts, 'all_failure' AS type, '' AS info, 0
			  FROM base WHERE rn > 1 AND all_consecutive_failures > prev_all_fail
			  UNION ALL
			  SELECT toString(ts) AS ts, 'manifest_failure' AS type, '' AS info, 0
			  FROM base WHERE rn > 1 AND manifest_consecutive_failures > prev_manifest_fail
			  UNION ALL
			  SELECT toString(ts) AS ts, 'segment_failure' AS type, '' AS info, 0
			  FROM base WHERE rn > 1 AND segment_consecutive_failures > prev_segment_fail
			  UNION ALL
			  SELECT toString(ts) AS ts, 'transport_failure' AS type, '' AS info, 0
			  FROM base WHERE rn > 1 AND transport_consecutive_failures > prev_transport_fail
			  UNION ALL
			  SELECT toString(ts) AS ts, 'transfer_active_timeout' AS type, '' AS info, 0
			  FROM base WHERE rn > 1 AND fault_count_transfer_active_timeout > prev_active_to
			  UNION ALL
			  SELECT toString(ts) AS ts, 'transfer_idle_timeout' AS type, '' AS info, 0
			  FROM base WHERE rn > 1 AND fault_count_transfer_idle_timeout > prev_idle_to
			  UNION ALL
			  SELECT toString(ts) AS ts, 'fault_on' AS type, '' AS info, 0
			  FROM base WHERE rn > 1 AND transport_fault_active = 1 AND prev_fault = 0
			  UNION ALL
			  SELECT toString(ts) AS ts, 'fault_off' AS type, '' AS info, 0
			  FROM base WHERE rn > 1 AND transport_fault_active = 0 AND prev_fault = 1
			  UNION ALL
			  -- Server-loop boundary: loop_count_server increments each
			  -- time the live LL-HLS/DASH worker rotates back to loop 0
			  -- of the source. Useful as a periodic gridline for long
			  -- testing sessions ("did the stall straddle a loop?").
			  SELECT toString(ts) AS ts, 'loop_server' AS type,
			         concat('loop ', toString(loop_count_server)) AS info, 0
			  FROM base WHERE rn > 1 AND loop_count_server > prev_loop_server

			  -- Network-request-derived events. These come from the
			  -- network_requests table (HAR archive). Mirrors the
			  -- waterfall's noteworthy classes: status-4xx/5xx,
			  -- fault-timeout, fault-incomplete, is-retry, slow-transfer.
			  -- Each row produces exactly one event via mutually-
			  -- exclusive WHEREs; nothing double-counts.
			  -- HAR sub-selects use a table alias 'nr' so the harWhere
			  -- clause's nr.ts BETWEEN ... resolves to the DateTime64
			  -- column. Without it, the projection's toString(ts) AS ts
			  -- would shadow ts as a String and break the comparison.
			  UNION ALL
			  SELECT toString(nr.ts) AS ts, 'http_5xx' AS type,
			         concat(toString(status), ' ', method, ' ', path) AS info, 0 AS duration_s
			  FROM %s.network_requests AS nr WHERE %s AND status >= 500
			  UNION ALL
			  SELECT toString(nr.ts) AS ts, 'http_4xx' AS type,
			         concat(toString(status), ' ', method, ' ', path) AS info, 0
			  FROM %s.network_requests AS nr WHERE %s AND status >= 400 AND status < 500
			  UNION ALL
			  -- Faulted requests, categorised by fault_type:
			  --   * 'timeout'  → request_timeout   (P2: picture may stop)
			  --   * corrupt/partial/abandon, OR faulted 2xx → request_incomplete
			  --   * everything else faulted → request_faulted
			  SELECT toString(nr.ts) AS ts,
			         multiIf(
			           positionCaseInsensitive(fault_type, 'timeout') > 0, 'request_timeout',
			           positionCaseInsensitive(fault_type, 'corrupt') > 0
			             OR positionCaseInsensitive(fault_type, 'partial') > 0
			             OR positionCaseInsensitive(fault_type, 'abandon') > 0
			             OR (status >= 200 AND status < 300), 'request_incomplete',
			           'request_faulted'
			         ) AS type,
			         concat(fault_type, ' ', method, ' ', path) AS info, 0
			  FROM %s.network_requests AS nr WHERE %s AND faulted = 1
			  UNION ALL
			  -- Slow non-error requests: client_wait_ms > 2 s.
			  SELECT toString(nr.ts) AS ts, 'slow_request' AS type,
			         concat(toString(round(client_wait_ms, 0)), 'ms ', method, ' ', path) AS info, 0
			  FROM %s.network_requests AS nr
			  WHERE %s AND client_wait_ms > 2000 AND status < 400 AND faulted = 0
			  UNION ALL
			  -- Slow media-segment transfers: distinct from slow_request,
			  -- this is about long actual transfer time on a segment-
			  -- shaped URL. Mirrors the waterfall's slow-transfer class.
			  SELECT toString(nr.ts) AS ts, 'slow_segment' AS type,
			         concat(toString(round(transfer_ms, 0)), 'ms ', method, ' ', path) AS info, 0
			  FROM %s.network_requests AS nr
			  WHERE %s AND transfer_ms > 6000
			    AND match(path, '\.(m4s|ts|mp4|m4a|m4v|aac|webm|mp3)($|\?)')
			    AND status < 400 AND faulted = 0
			  UNION ALL
			  -- Retries: same URL fetched again within ~4 s. Detected
			  -- via lagInFrame partitioned by url; the network log uses
			  -- a per-segment-duration threshold but a fixed 4 s catches
			  -- almost every real retry case (segments are 2 s/6 s).
			  SELECT toString(retry_ts) AS ts, 'request_retry' AS type,
			         concat(method, ' ', path) AS info, 0
			  FROM (
			    SELECT nr.ts AS retry_ts, method, url, path,
			           lagInFrame(nr.ts, 1, nr.ts) OVER (PARTITION BY url ORDER BY nr.ts ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING) AS prev_url_ts
			    FROM %s.network_requests AS nr
			    WHERE %s
			  )
			  WHERE prev_url_ts != retry_ts
			    AND dateDiff('millisecond', prev_url_ts, retry_ts) BETWEEN 1 AND 4000
			)
			ORDER BY ts ASC
			LIMIT %d
			FORMAT JSONEachRow`,
			// stall_or_buffer_pairs / rate_shifts / base CTEs
			cfg.chDatabase, cfg.chTable, where,
			cfg.chDatabase, cfg.chTable, where,
			cfg.chDatabase, cfg.chTable, where,
			// frozen/segment_stall, restart, playback_start, timejump, error (5)
			cfg.chDatabase, cfg.chTable, where,
			cfg.chDatabase, cfg.chTable, where,
			cfg.chDatabase, cfg.chTable, where,
			cfg.chDatabase, cfg.chTable, where,
			cfg.chDatabase, cfg.chTable, where,
			// user_marked + catch-all (2)
			cfg.chDatabase, cfg.chTable, where,
			cfg.chDatabase, cfg.chTable, where,
			// HAR events: http_5xx, http_4xx, faulted, slow_request,
			// slow_segment, request_retry — each filtered by harWhere
			// (time range, NOT play_id) since iOS strips play_id from
			// variant/segment URLs.
			cfg.chDatabase, harWhere,
			cfg.chDatabase, harWhere,
			cfg.chDatabase, harWhere,
			cfg.chDatabase, harWhere,
			cfg.chDatabase, harWhere,
			cfg.chDatabase, harWhere,
			limit)
		proxyClickHouseJSON(w, r, cfg, query, params)
	})

	// Per-request HAR-style log for the session-viewer's network log
	// fold. Returns rows in the same shape the live go-proxy endpoint
	// emits ({entries: [...]}) so the existing UI code can consume it
	// without modification.
	mux.HandleFunc("/api/network_requests", func(w http.ResponseWriter, r *http.Request) {
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
				params["play"] = v
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
		// Wrap the rows into the {entries:[...]} envelope go-proxy
		// returns so the browser code can be source-agnostic. We let
		// ClickHouse build the JSON via JSONObjectEachRow + manual
		// envelope rather than a string concat in Go (cheaper, fewer
		// escapes, and the formatter handles types).
		query := fmt.Sprintf(`
			SELECT
			  toString(ts) AS timestamp,
			  method, url, upstream_url, path, request_kind AS request_kind,
			  status, bytes_in, bytes_out, content_type, play_id,
			  request_range, response_content_range,
			  dns_ms, connect_ms, tls_ms, ttfb_ms, transfer_ms, total_ms, client_wait_ms,
			  faulted = 1 AS faulted, fault_type, fault_action, fault_category,
			  request_headers, response_headers, query_string
			FROM %s.network_requests
			WHERE %s
			ORDER BY ts ASC
			LIMIT %d
			FORMAT JSONEachRow`, cfg.chDatabase, strings.Join(clauses, " AND "), limit)
		// We can't directly stream JSONEachRow as the {entries:[...]}
		// envelope without rewriting the body. Buffer + assemble.
		body, err := chQueryBytes(r.Context(), cfg, query, params)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte(`{"entries":[`))
		first := true
		for _, line := range bytes.Split(body, []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			// Each line is one JSONObject; strings stored as JSON-encoded
			// header arrays need to be re-parsed so the consumer sees
			// real arrays rather than escaped strings. Cheap trick: a
			// post-pass per row.
			if !first {
				w.Write([]byte(","))
			}
			w.Write(reinflateNetRowJSON(line))
			first = false
		}
		w.Write([]byte("]}"))
	})

	// /api/session_bundle — ZIP of snapshots + HAR + events for one
	// (session_id, play_id). Defined in bundle.go.
	registerBundleHandler(mux, cfg)

	// /api/sessions/{sid}/{pid}/{star,reclassify} — tiered retention
	// classification (issue #342). Defined in classification.go.
	registerClassificationHandlers(mux, cfg)

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
