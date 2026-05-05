package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/mux"
	"github.com/grafov/m3u8"
	"github.com/vishvananda/netlink"
	_ "modernc.org/sqlite"

)

//go:embed templates/index.html
var indexHTML string

var versionString = "unknown"
var segmentSequenceDigitsRegex = regexp.MustCompile(`\d+`)
var netemDelayRegex = regexp.MustCompile(`delay ([0-9.]+)ms`)
var netemLossRegex = regexp.MustCompile(`loss ([0-9.]+)%`)
var tcSentBytesRegex = regexp.MustCompile(`Sent (\d+) bytes`)
var tcBacklogRegex = regexp.MustCompile(`backlog\s+(\d+)b`)
var nftHandleRegex = regexp.MustCompile(`handle\s+([0-9]+)`)
var nftCommentPortRegex = regexp.MustCompile(`comment\s+"go_proxy_transport_port_([0-9]+)"`)
var nftCounterRegex = regexp.MustCompile(`counter packets ([0-9]+) bytes ([0-9]+)`)
var segmentGroupRegex = regexp.MustCompile(`_G(\d+)$`)

type SessionData map[string]interface{}

// segmentFlightInfo tracks an active segment download for throughput measurement.
type segmentFlightInfo struct {
	startTime time.Time
	id        uint64 // generation counter; markSegmentFlightEnd only fires if id matches
}

// segmentRunRecord holds the precise start/end timestamps and TC byte counter
// values captured by awaitSocketDrain at TC-backlog transition points.
// startTime/startBytes are set when backlog first goes non-zero (Phase 1 end).
// endTime/endBytes are set when backlog returns to zero (Phase 2 end).
type segmentRunRecord struct {
	startTime  time.Time
	startBytes int64
	endTime    time.Time
	endBytes   int64
}

// tcSample holds a single 10ms TC poll result from awaitSocketDrain.
type tcSample struct {
	at      time.Time
	bytes   int64
	backlog int64
}

// wireRateSample holds a byte-change-gated throughput measurement computed in
// awaitSocketDrain. Rate is only computed when bytes change AND ≥100ms has
// elapsed since the previous report — this eliminates HTB burst aliasing.
type wireRateSample struct {
	at    time.Time
	mbps  float64
	bytes int64 // delta bytes in this measurement window
}

// tcStatsCache holds the latest TC stats for a port, shared across concurrent
// awaitSocketDrain goroutines. Only one netlink call is made per refresh interval.
type tcStatsCache struct {
	mu      sync.Mutex
	at      time.Time
	bytes   int64
	backlog int64
}



// HeaderPair is a single name/value pair, used to carry HTTP request /
// response headers and query parameters in NetworkLogEntry. Mirrors the
// HAR 1.2 NameValue shape so a HAR consumer can drop these straight into
// request.headers / response.headers without conversion.
type HeaderPair struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// NetworkLogEntry represents a single network request/response in the session
//
// The URL field stores the *player-facing* URL (what the client requested
// from go-proxy) so HAR entries reflect the player's view. The
// UpstreamURL field carries the URL the proxy used to reach the origin —
// useful forensics ("did the proxy rewrite the variant?") that lands
// under HAR's _extensions.upstream.url.
type NetworkLogEntry struct {
	Timestamp   time.Time `json:"timestamp"`
	Method      string    `json:"method"`
	URL         string    `json:"url"`
	UpstreamURL string    `json:"upstream_url,omitempty"`
	Path        string    `json:"path"`
	RequestKind string    `json:"request_kind"` // "segment", "manifest", "master_manifest"
	Status      int       `json:"status"`
	BytesIn     int64     `json:"bytes_in"`
	BytesOut    int64     `json:"bytes_out"`
	ContentType string    `json:"content_type"`

	// PlayID identifies the playback episode this request belongs to.
	// The player generates a fresh UUID at every loadStream / reload
	// and passes it as `?play_id=...` on every URL. HAR snapshots
	// filter by the current play_id by default, so a freeze 8 minutes
	// into a session shows just that play's network log instead of
	// the whole ring buffer. Issue #280.
	PlayID string `json:"play_id,omitempty"`

	// HTTP-level metadata captured per-request. Sensitive headers
	// (Cookie / Authorization / Set-Cookie) are filtered before they
	// land here — see capturedHeaders.
	RequestHeaders  []HeaderPair `json:"request_headers,omitempty"`
	ResponseHeaders []HeaderPair `json:"response_headers,omitempty"`
	QueryString     []HeaderPair `json:"query_string,omitempty"`

	// Timing phases (milliseconds).
	//
	// The DNSMs/ConnectMs/TLSMs/TTFBMs fields measure the *upstream*
	// connection (proxy → origin) — captured by httptrace during
	// doRequestWithTracing. They're useful forensics ("was the origin
	// slow?") but NOT what the player perceived. ClientWaitMs and
	// TransferMs measure the player-perceived (downstream) view; see
	// the explicit ClientWaitMs field below.
	DNSMs      float64 `json:"dns_ms"`
	ConnectMs  float64 `json:"connect_ms"`
	TLSMs      float64 `json:"tls_ms"`
	TTFBMs     float64 `json:"ttfb_ms"`     // Upstream time to first byte
	TransferMs float64 `json:"transfer_ms"` // Downstream write+flush time to client (= client-perceived `receive`)
	TotalMs    float64 `json:"total_ms"`

	// ClientWaitMs is the time from when the proxy received the request
	// to when it sent the first response byte back to the client. It IS
	// what the player perceived as `wait` (HAR's TTFB), modulo the
	// network RTT in both directions which we don't capture server-side
	// (issue #283).
	ClientWaitMs float64 `json:"client_wait_ms"`

	// Fault injection metadata
	Faulted       bool   `json:"faulted"`
	FaultType     string `json:"fault_type,omitempty"`
	FaultAction   string `json:"fault_action,omitempty"`
	FaultCategory string `json:"fault_category,omitempty"` // "http", "socket", "transport", "corruption"

	// Range-request metadata. RequestRange is the client's `Range:`
	// header (e.g. "bytes=0-1023"); ResponseContentRange is the
	// origin's `Content-Range:` header (e.g. "bytes 0-1023/5242880").
	// Useful for telling apart partial-content fetches and continuation
	// requests in the dashboard.
	RequestRange         string `json:"request_range,omitempty"`
	ResponseContentRange string `json:"response_content_range,omitempty"`
}

// sensitiveHeaderNames are excluded from HAR captures regardless of source.
// Lower-cased for canonical comparison.
var sensitiveHeaderNames = map[string]bool{
	"cookie":               true,
	"set-cookie":           true,
	"authorization":        true,
	"proxy-authorization":  true,
	"x-amz-security-token": true,
}

// capturedHeaders converts an http.Header map to a sorted []HeaderPair,
// dropping sensitive entries. Stable ordering keeps HAR diffs readable.
func capturedHeaders(h http.Header) []HeaderPair {
	if len(h) == 0 {
		return nil
	}
	out := make([]HeaderPair, 0, len(h))
	for name, values := range h {
		if sensitiveHeaderNames[strings.ToLower(name)] {
			continue
		}
		for _, v := range values {
			out = append(out, HeaderPair{Name: name, Value: v})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Value < out[j].Value
	})
	return out
}

// stampNetMeta attaches captured request headers, query string, and (if
// available) response headers onto a NetworkLogEntry. Idempotent — won't
// overwrite already-set fields, so callers can pre-populate any of them
// at the entry-construction site.
func stampNetMeta(entry *NetworkLogEntry, requestHeaders, queryString []HeaderPair, resp *http.Response) {
	if entry == nil {
		return
	}
	if entry.RequestHeaders == nil && len(requestHeaders) > 0 {
		entry.RequestHeaders = requestHeaders
	}
	if entry.QueryString == nil && len(queryString) > 0 {
		entry.QueryString = queryString
	}
	if entry.ResponseHeaders == nil && resp != nil {
		entry.ResponseHeaders = capturedHeaders(resp.Header)
	}
}

// capturedQueryString converts the URL's query into []HeaderPair preserving
// the parameter order from the URL. Sensitive values aren't filtered here —
// the dashboard already exposes player_id query params; if that ever changes
// the privacy filter goes here.
func capturedQueryString(u *url.URL) []HeaderPair {
	if u == nil || u.RawQuery == "" {
		return nil
	}
	pairs := strings.Split(u.RawQuery, "&")
	out := make([]HeaderPair, 0, len(pairs))
	for _, p := range pairs {
		if p == "" {
			continue
		}
		var name, value string
		if eq := strings.IndexByte(p, '='); eq >= 0 {
			name, value = p[:eq], p[eq+1:]
		} else {
			name = p
		}
		// URL-decode each side; ignore errors and keep the raw bytes.
		if decoded, err := url.QueryUnescape(name); err == nil {
			name = decoded
		}
		if decoded, err := url.QueryUnescape(value); err == nil {
			value = decoded
		}
		out = append(out, HeaderPair{Name: name, Value: value})
	}
	return out
}

// NetworkLogRingBuffer maintains a bounded list of recent network entries
type NetworkLogRingBuffer struct {
	mu      sync.RWMutex
	entries []NetworkLogEntry
	maxSize int
	index   int
}

// NewNetworkLogRingBuffer creates a new ring buffer with the specified capacity
func NewNetworkLogRingBuffer(maxSize int) *NetworkLogRingBuffer {
	return &NetworkLogRingBuffer{
		entries: make([]NetworkLogEntry, 0, maxSize),
		maxSize: maxSize,
	}
}

// Add appends a new entry to the ring buffer
func (rb *NetworkLogRingBuffer) Add(entry NetworkLogEntry) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	if len(rb.entries) < rb.maxSize {
		rb.entries = append(rb.entries, entry)
	} else {
		rb.entries[rb.index] = entry
	}
	rb.index = (rb.index + 1) % rb.maxSize
}

// GetAll returns all entries in chronological order (oldest first)
func (rb *NetworkLogRingBuffer) GetAll() []NetworkLogEntry {
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	if len(rb.entries) == 0 {
		return []NetworkLogEntry{}
	}

	// If buffer is not full, return in order
	if len(rb.entries) < rb.maxSize {
		result := make([]NetworkLogEntry, len(rb.entries))
		copy(result, rb.entries)
		return result
	}

	// Buffer is full, reconstruct chronological order
	result := make([]NetworkLogEntry, rb.maxSize)
	copy(result, rb.entries[rb.index:])
	copy(result[rb.maxSize-rb.index:], rb.entries[:rb.index])
	return result
}

type App struct {
	sessionsMu               sync.Mutex
	sessionsSnap             atomic.Pointer[[]SessionData]
	throughputMu             sync.RWMutex
	throughputData           map[int]map[string]interface{}
	sessionEvents            *SessionEventStore
	traffic                  *TcTrafficManager
	upstreamHost             string
	upstreamPort             string
	maxSessions              int
	client                   *http.Client
	portMap                  PortMapping
	shapeMu                  sync.Mutex
	shapeLoops               map[int]context.CancelFunc
	shapeStates              map[int]NftShapePattern
	shapeApplyMu             sync.Mutex
	shapeApply               map[int]ShapeApplyState
	faultMu                  sync.Mutex
	faultLoops               map[int]context.CancelFunc
	networkLogsMu            sync.RWMutex
	networkLogs              map[string]*NetworkLogRingBuffer // sessionId -> ring buffer
	loopStateMu              sync.Mutex
	loopStateBySession       map[string]ServerLoopState
	sessionsHub              *SessionEventHub
	sessionsBroadcastMu      sync.Mutex
	sessionsBroadcastPending bool
	sessionsBroadcastLatest  []SessionData
	sessionsBroadcastSeq     uint64
	networkHub               *NetworkEventHub
	uiStateVersionSeq        uint64
	segmentFlightMu          sync.Mutex
	segmentFlight            map[int]segmentFlightInfo // internal port -> segment transfer info
	segmentFlightSeq         uint64                    // atomic generation counter for flight IDs
	segmentRunMu             sync.Mutex
	segmentRun               map[int]segmentRunRecord // internal port -> last completed run record
	drainActiveMu            sync.Mutex
	drainActive              map[int]bool // per-port: true while awaitSocketDrain is running
	tcSamplesMu              sync.Mutex
	tcSamples                map[int][]tcSample
	wireRateMu               sync.Mutex
	wireRate                 map[int]wireRateSample // latest byte-change-gated rate per port
	tcCacheMu                sync.Mutex
	tcCache                  map[int]*tcStatsCache // per-port TC stats cache
	transferCompleteMu           sync.Mutex
	transferCompleteMbps         map[int]float64   // latest completed segment Mbps per port
	transferCompleteAt           map[int]time.Time // when the drain completed
}

// sessionStateMu serialises read-modify-write on the session map
// (`SessionData`). Multiple goroutines can hit handleProxy for the
// same session simultaneously — one for the video segment, one for
// the audio segment, etc. — and any helper that does
// `session[k] = getInt(session, k) + 1` will lose updates and (in
// the case of fault-decision logic) double-fire faults.
//
// Package-level rather than an App field so free helpers like
// bumpFaultCounter and updateSessionTraffic can grab the same lock
// without being converted to methods. Tradeoff: a single global
// mutex serialises all session-counter mutations across all
// sessions. The critical section is microseconds and holds no I/O,
// so contention is negligible at our request rates.
var sessionStateMu sync.Mutex

type ServerLoopState struct {
	LastSegmentSeq int
	MaxSegmentSeq  int
}

type SessionEventHub struct {
	mu      sync.Mutex
	nextID  int
	clients map[int]*SessionClient
}

type SessionClient struct {
	ch             chan SessionsEvent
	dropped        uint64
	playerIDFilter string
}

type SessionsEvent struct {
	Sessions     []SessionData
	Revision     uint64
	Dropped      uint64
	PreMarshaled string
}

type SessionsStreamPayload struct {
	Revision       uint64              `json:"revision"`
	Dropped        uint64              `json:"dropped"`
	Sessions       []SessionData       `json:"sessions"`
	ActiveSessions []ActiveSessionInfo `json:"active_sessions,omitempty"`
}

type ActiveSessionInfo struct {
	SessionID string `json:"session_id"`
	PlayerID  string `json:"player_id"`
	GroupID   string `json:"group_id"`
	Port      string `json:"port"`
}

type SessionEventStore struct {
	db *sql.DB
}

type SessionPatchRequest struct {
	Set          map[string]interface{} `json:"set"`
	Fields       []string               `json:"fields"`
	BaseRevision string                 `json:"base_revision"`
}

type PortMapping struct {
	externalBase int
	internalBase int
	count        int
}

func NewSessionEventHub() *SessionEventHub {
	return &SessionEventHub{clients: map[int]*SessionClient{}}
}

// NetworkEventHub fans out per-request network log entries to subscribed
// SSE clients (currently: the analytics forwarder). Each Add() call
// produces one event with {session_id, entry}. Slow clients lose old
// events on overflow rather than blocking the proxy hot path.
type NetworkEventHub struct {
	mu      sync.Mutex
	nextID  int
	clients map[int]*NetworkClient
}

type NetworkClient struct {
	ch      chan NetworkEvent
	dropped uint64
}

type NetworkEvent struct {
	SessionID string
	Entry     NetworkLogEntry
}

func NewNetworkEventHub() *NetworkEventHub {
	return &NetworkEventHub{clients: map[int]*NetworkClient{}}
}

func (h *NetworkEventHub) AddClient(buffer int) (int, <-chan NetworkEvent) {
	if buffer <= 0 {
		buffer = 256
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	id := h.nextID
	c := &NetworkClient{ch: make(chan NetworkEvent, buffer)}
	h.clients[id] = c
	return id, c.ch
}

func (h *NetworkEventHub) RemoveClient(id int) {
	h.mu.Lock()
	c, ok := h.clients[id]
	if ok {
		delete(h.clients, id)
	}
	h.mu.Unlock()
	if ok {
		close(c.ch)
	}
}

func (h *NetworkEventHub) Broadcast(sessionID string, entry NetworkLogEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) == 0 {
		return
	}
	ev := NetworkEvent{SessionID: sessionID, Entry: entry}
	for _, c := range h.clients {
		select {
		case c.ch <- ev:
		default:
			// Buffer full — drop oldest, log occasionally.
			select {
			case <-c.ch:
				c.dropped++
			default:
			}
			select {
			case c.ch <- ev:
			default:
				c.dropped++
			}
		}
	}
}

func (h *SessionEventHub) AddClient(playerIDFilter string) (int, <-chan SessionsEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	id := h.nextID
	client := &SessionClient{ch: make(chan SessionsEvent, 1), playerIDFilter: playerIDFilter}
	h.clients[id] = client
	return id, client.ch
}

func (h *SessionEventHub) RemoveClient(id int) {
	h.mu.Lock()
	client, ok := h.clients[id]
	if ok {
		delete(h.clients, id)
	}
	h.mu.Unlock()
	if ok {
		close(client.ch)
	}
}

func (h *SessionEventHub) Broadcast(sessions []SessionData, revision uint64, preMarshaled string) {
	h.mu.Lock()
	for id, client := range h.clients {
		if len(client.ch) == cap(client.ch) {
			select {
			case <-client.ch:
				client.dropped++
			default:
			}
			log.Printf("SSE drop client=%d dropped=%d", id, client.dropped)
		}
		dropped := client.dropped
		pm := preMarshaled
		if dropped > 0 || client.playerIDFilter != "" {
			pm = ""
		}
		event := SessionsEvent{Sessions: sessions, Revision: revision, Dropped: dropped, PreMarshaled: pm}
		select {
		case client.ch <- event:
			client.dropped = 0
		default:
			client.dropped = dropped + 1
		}
	}
	h.mu.Unlock()
}

type PlaylistInfo struct {
	URL        string `json:"url"`
	Bandwidth  int    `json:"bandwidth"`
	Resolution string `json:"resolution"`
}

type TcTrafficManager struct {
	interfaceName string
	debug         bool
	nlMu          sync.Mutex
	nlHandle      *netlink.Handle // persistent netlink handle, created lazily
	nlLink        netlink.Link    // resolved once from interfaceName
}

type ShapeApplyState struct {
	rate  float64
	delay int
	loss  float64
}

func (a *App) getShapeApplyState(port int) (ShapeApplyState, bool) {
	a.shapeApplyMu.Lock()
	state, ok := a.shapeApply[port]
	a.shapeApplyMu.Unlock()
	return state, ok
}

func (a *App) setShapeApplyState(port int, state ShapeApplyState) {
	a.shapeApplyMu.Lock()
	a.shapeApply[port] = state
	a.shapeApplyMu.Unlock()
}

type NftShapeStep struct {
	RateMbps        float64 `json:"rate_mbps"`
	DurationSeconds float64 `json:"duration_seconds"`
	Enabled         bool    `json:"enabled"`
}

type NftShapePattern struct {
	Steps          []NftShapeStep `json:"steps"`
	ActiveStep     int            `json:"active_step"`
	ActiveRateMbps float64        `json:"active_rate_mbps"`
	ActiveAt       string         `json:"active_at"`
}

type TransportFaultRuleCounters struct {
	DropPackets   int64
	DropBytes     int64
	RejectPackets int64
	RejectBytes   int64
}

const (
	transportFaultTableName = "go_proxy_faults"
	transportFaultChainName = "transport_faults"
	transportUnitsSeconds   = "seconds"
	transportUnitsPackets   = "packets"
	socketMidBodyBytes      = 64 * 1024
	socketHangDuration      = 90 * time.Second
	socketDelayDuration     = 12 * time.Second
	externalWANSessionLimit = 2
	defaultSessionEventsDB  = "/tmp/go-proxy-session-events.sqlite"
)

func newSessionEventStore(path string) (*SessionEventStore, error) {
	if strings.TrimSpace(path) == "" {
		path = defaultSessionEventsDB
	}
	// SQLite cannot create the file in a non-existent directory; the
	// default /tmp path is always present, but operators overriding
	// GO_PROXY_SESSION_EVENTS_DB to a custom path otherwise hit the
	// same db.Ping() failure that PR #343 fixed for go-upload.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create session-events parent dir %s: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	schema := `
		CREATE TABLE IF NOT EXISTS session_lifecycle_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			player_id TEXT,
			origination_ip TEXT,
			external_port TEXT,
			internal_port TEXT,
			manifest_url TEXT,
			started_at TEXT NOT NULL,
			ended_at TEXT,
			duration_seconds REAL,
			end_reason TEXT,
			created_at TEXT NOT NULL DEFAULT (STRFTIME('%Y-%m-%dT%H:%M:%fZ', 'now')),
			updated_at TEXT NOT NULL DEFAULT (STRFTIME('%Y-%m-%dT%H:%M:%fZ', 'now'))
		);
		CREATE INDEX IF NOT EXISTS idx_session_lifecycle_events_session ON session_lifecycle_events(session_id);
		CREATE INDEX IF NOT EXISTS idx_session_lifecycle_events_started_at ON session_lifecycle_events(started_at DESC);
	`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &SessionEventStore{db: db}, nil
}

func (s *SessionEventStore) RecordStart(session SessionData, manifestURL string, startedAt time.Time) error {
	if s == nil || s.db == nil {
		return nil
	}
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(
		`INSERT INTO session_lifecycle_events (
			session_id, player_id, origination_ip, external_port, internal_port, manifest_url, started_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		getString(session, "session_id"),
		getString(session, "player_id"),
		getString(session, "origination_ip"),
		getString(session, "x_forwarded_port_external"),
		getString(session, "x_forwarded_port"),
		manifestURL,
		startedAt.UTC().Format(time.RFC3339Nano),
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	return err
}

func (s *SessionEventStore) RecordEnd(session SessionData, endedAt time.Time, reason string) error {
	if s == nil || s.db == nil {
		return nil
	}
	if endedAt.IsZero() {
		endedAt = time.Now().UTC()
	}
	startAt := timeFromInterface(session["session_start_time"])
	if startAt.IsZero() {
		startAt = timeFromInterface(session["first_request_time"])
	}
	if startAt.IsZero() {
		startAt = endedAt
	}
	durationSeconds := endedAt.Sub(startAt).Seconds()
	if durationSeconds < 0 {
		durationSeconds = 0
	}
	_, err := s.db.Exec(
		`UPDATE session_lifecycle_events
		SET ended_at = ?, duration_seconds = ?, end_reason = ?, updated_at = ?
		WHERE id = (
			SELECT id FROM session_lifecycle_events
			WHERE session_id = ? AND ended_at IS NULL
			ORDER BY started_at DESC
			LIMIT 1
		)`,
		endedAt.UTC().Format(time.RFC3339Nano),
		durationSeconds,
		reason,
		time.Now().UTC().Format(time.RFC3339Nano),
		getString(session, "session_id"),
	)
	return err
}

func (a *App) recordSessionStart(session SessionData, manifestURL string) {
	if a == nil || a.sessionEvents == nil {
		return
	}
	startAt := timeFromInterface(session["session_start_time"])
	if startAt.IsZero() {
		startAt = timeFromInterface(session["first_request_time"])
	}
	if err := a.sessionEvents.RecordStart(session, manifestURL, startAt); err != nil {
		log.Printf("session event start failed session_id=%s err=%v", getString(session, "session_id"), err)
	}
}

func (a *App) recordSessionEnd(session SessionData, reason string) {
	if a == nil || a.sessionEvents == nil {
		return
	}
	if err := a.sessionEvents.RecordEnd(session, time.Now().UTC(), reason); err != nil {
		log.Printf("session event end failed session_id=%s reason=%s err=%v", getString(session, "session_id"), reason, err)
	}
}

func (s *NftShapeStep) UnmarshalJSON(data []byte) error {
	type alias struct {
		RateMbps        float64 `json:"rate_mbps"`
		DurationSeconds float64 `json:"duration_seconds"`
		Enabled         *bool   `json:"enabled"`
	}
	var raw alias
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.RateMbps = raw.RateMbps
	s.DurationSeconds = raw.DurationSeconds
	s.Enabled = true
	if raw.Enabled != nil {
		s.Enabled = *raw.Enabled
	}
	return nil
}

func NewTcTrafficManager(interfaceName string, debug bool) *TcTrafficManager {
	return &TcTrafficManager{interfaceName: interfaceName, debug: debug}
}

func (t *TcTrafficManager) IsActive() bool {
	cmd := exec.Command("tc", "qdisc", "show", "dev", t.interfaceName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), "htb")
}

func (t *TcTrafficManager) EnsureRootQdisc() error {
	show := exec.Command("tc", "qdisc", "show", "dev", t.interfaceName)
	if out, err := show.CombinedOutput(); err == nil {
		if strings.Contains(string(out), "qdisc htb 1:") || strings.Contains(string(out), "root htb") {
			return nil
		}
	}
	_ = exec.Command("tc", "qdisc", "del", "dev", t.interfaceName, "root").Run()
	cmd := exec.Command("tc", "qdisc", "add", "dev", t.interfaceName, "root", "handle", "1:", "htb", "default", "999")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tc qdisc add failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (t *TcTrafficManager) EnsureRootClass() error {
	cmd := exec.Command("tc", "class", "show", "dev", t.interfaceName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return err
	}
	if strings.Contains(string(output), "class htb 1:1") {
		return nil
	}
	addCmd := exec.Command(
		"tc", "class", "add", "dev", t.interfaceName, "parent", "1:",
		"classid", "1:1", "htb", "rate", "10000mbit", "ceil", "10000mbit",
		"burst", "16k", "cburst", "16k", "quantum", "1514",
	)
	if out, err := addCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tc root class add failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func htbClassParams(rateMbps float64) (burstBytes, cburstBytes, quantumBytes int) {
	const (
		mtuBytes            = 1514
		minBurstBytes       = mtuBytes
		maxBurstBytes       = 4 * 1024
		targetBurstSeconds  = 0.004
		mediumRateMbps      = 20.0
		highRateMbps        = 100.0
		mediumQuantumFactor = 2
		highQuantumFactor   = 4
	)

	rateBps := math.Max(0, rateMbps) * 1_000_000.0
	computedBurst := int(math.Round((rateBps / 8.0) * targetBurstSeconds))
	if computedBurst < minBurstBytes {
		computedBurst = minBurstBytes
	}
	if computedBurst > maxBurstBytes {
		computedBurst = maxBurstBytes
	}
	burstBytes = computedBurst
	cburstBytes = computedBurst

	quantumBytes = mtuBytes
	if rateMbps >= highRateMbps {
		quantumBytes = highQuantumFactor * mtuBytes
	} else if rateMbps >= mediumRateMbps {
		quantumBytes = mediumQuantumFactor * mtuBytes
	}
	return burstBytes, cburstBytes, quantumBytes
}

func (t *TcTrafficManager) GetPortConfig(port int) (map[string]interface{}, error) {
	cmd := exec.Command("tc", "class", "show", "dev", t.interfaceName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}
	config := map[string]interface{}{
		"port":            port,
		"bandwidth_limit": nil,
		"bandwidth_mbps":  nil,
		"packet_loss":     nil,
		"delay_ms":        nil,
	}
	portSuffix := fmt.Sprintf("%03d", port%1000)
	classid := fmt.Sprintf("1:%s", portSuffix)
	pattern := fmt.Sprintf("class htb %s", classid)
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, pattern) && strings.Contains(line, "rate") {
			fields := strings.Fields(line)
			for i := 0; i < len(fields)-1; i++ {
				if fields[i] == "rate" {
					rateStr := fields[i+1]
					config["bandwidth_limit"] = rateStr
					config["bandwidth_mbps"] = rateToMbps(rateStr)
					break
				}
			}
		}
	}
	if netem, err := t.GetNetemConfig(port); err == nil && netem != nil {
		if val, ok := netem["packet_loss"]; ok {
			config["packet_loss"] = val
		}
		if val, ok := netem["delay_ms"]; ok {
			config["delay_ms"] = val
		}
	}
	return config, nil
}

func (t *TcTrafficManager) UpdateRateLimit(port int, rateMbps float64) error {
	if err := t.EnsureRootQdisc(); err != nil {
		return err
	}
	if err := t.EnsureRootClass(); err != nil {
		return err
	}
	if rateMbps <= 0 {
		log.Printf(
			"NETSHAPE throughput_set ts=%s port=%d rate_mbps=0 action=clear",
			time.Now().UTC().Format(time.RFC3339Nano),
			port,
		)
		_ = t.UpdateNetem(port, 0, 0)
		_ = t.RemoveFilter(port)
		_ = t.RemoveClass(port)
		t.logTcState("rate_clear", port)
		t.scheduleRateLimitVerification(port, 0, 3*time.Second)
		return nil
	}
	portSuffix := fmt.Sprintf("%03d", port%1000)
	classid := fmt.Sprintf("1:%s", portSuffix)
	burstBytes, cburstBytes, quantumBytes := htbClassParams(rateMbps)
	burstArg := fmt.Sprintf("%db", burstBytes)
	cburstArg := fmt.Sprintf("%db", cburstBytes)
	quantumArg := strconv.Itoa(quantumBytes)
	changeCmd := exec.Command(
		"tc", "class", "change", "dev", t.interfaceName, "parent", "1:1",
		"classid", classid, "htb", "rate", fmt.Sprintf("%gmbit", rateMbps), "ceil", fmt.Sprintf("%gmbit", rateMbps),
		"burst", burstArg, "cburst", cburstArg, "quantum", quantumArg,
	)
	log.Printf(
		"NETSHAPE throughput_set ts=%s port=%d rate_mbps=%.3f action=apply classid=%s iface=%s burst_bytes=%d cburst_bytes=%d quantum_bytes=%d",
		time.Now().UTC().Format(time.RFC3339Nano),
		port,
		rateMbps,
		classid,
		t.interfaceName,
		burstBytes,
		cburstBytes,
		quantumBytes,
	)
	if out, err := changeCmd.CombinedOutput(); err != nil {
		log.Printf("NETSHAPE tc class change failed port=%d: %s", port, strings.TrimSpace(string(out)))
		addCmd := exec.Command(
			"tc", "class", "add", "dev", t.interfaceName, "parent", "1:1",
			"classid", classid, "htb", "rate", fmt.Sprintf("%gmbit", rateMbps), "ceil", fmt.Sprintf("%gmbit", rateMbps),
			"burst", burstArg, "cburst", cburstArg, "quantum", quantumArg,
		)
		if outAdd, errAdd := addCmd.CombinedOutput(); errAdd != nil {
			return fmt.Errorf("tc class change failed: %s; add failed: %s", strings.TrimSpace(string(out)), strings.TrimSpace(string(outAdd)))
		}
	}

	if err := t.ensurePortFilter(port, classid); err != nil {
		return err
	}
	verifyCmd := exec.Command("tc", "class", "show", "dev", t.interfaceName)
	verifyOut, _ := verifyCmd.CombinedOutput()
	afterFilterCmd := exec.Command("tc", "filter", "show", "dev", t.interfaceName)
	afterFilterOut, _ := afterFilterCmd.CombinedOutput()
	log.Printf("NETSHAPE tc class show dev %s: %s", t.interfaceName, strings.TrimSpace(string(verifyOut)))
	log.Printf("NETSHAPE tc filter show dev %s: %s", t.interfaceName, strings.TrimSpace(string(afterFilterOut)))
	verifyText := string(verifyOut)
	classToken := fmt.Sprintf("class htb %s", classid)
	trimmedClassToken := fmt.Sprintf("class htb 1:%d", port%1000)
	if !strings.Contains(verifyText, classToken) && !strings.Contains(verifyText, trimmedClassToken) {
		return fmt.Errorf("tc class not present after update: %s", strings.TrimSpace(verifyText))
	}
	t.logTcState("rate_apply", port)
	t.scheduleRateLimitVerification(port, rateMbps, 3*time.Second)
	return nil
}

func (t *TcTrafficManager) RemoveClass(port int) error {
	portSuffix := fmt.Sprintf("%03d", port%1000)
	classid := fmt.Sprintf("1:%s", portSuffix)
	_ = exec.Command("tc", "class", "del", "dev", t.interfaceName, "classid", classid).Run()
	return nil
}

// ClearPortShaping is the one-shot session-start sweep that closes
// the leftover-tc-rule leak (issue #352): drop any tc class + filter
// for `port` regardless of whether the proxy thinks one is configured.
// Idempotent and safe to call on a clean port — `tc class del` on a
// non-existent class returns a non-zero exit which we ignore.
//
// Why session-start instead of process-start or session-end:
//   - Session-end cleanup misses leaks that survive a proxy crash.
//   - Process-start cleanup needs a list of active sessions, which is
//     empty at startup, so it'd nuke ALL classes — risky for
//     concurrent sessions on a hot reload.
//   - Session-start cleanup runs exactly when we know the port is
//     about to belong to a fresh playback episode; whatever was
//     there before is by definition leftover from a prior session.
func (t *TcTrafficManager) ClearPortShaping(port int) {
	portSuffix := fmt.Sprintf("%03d", port%1000)
	classid := fmt.Sprintf("1:%s", portSuffix)
	// First check if there's actually a class to clear — keeps the
	// logs quiet on a fresh port.
	show := exec.Command("tc", "class", "show", "dev", t.interfaceName)
	out, _ := show.CombinedOutput()
	if !strings.Contains(string(out), "class htb "+classid) {
		return
	}
	log.Printf("NETSHAPE port_shaping_clear port=%d classid=%s reason=session_start", port, classid)
	_ = exec.Command("tc", "filter", "del", "dev", t.interfaceName, "protocol", "ip",
		"parent", "1:0", "prio", "1", "u32",
		"match", "ip", "sport", fmt.Sprintf("%d", port), "0xffff").Run()
	_ = exec.Command("tc", "class", "del", "dev", t.interfaceName, "classid", classid).Run()
}

// scheduleRateLimitVerification fires a one-shot check `delay` after
// a rate limit was applied to confirm it stuck. Catches the case
// where the apply command reports success but the kernel state
// diverges within seconds — e.g. another process clears the rule,
// the kernel drops it, a follow-up tc operation racing on the same
// port wins. Logs `NETSHAPE LOST` if the kernel rate diverges from
// expected, otherwise quietly logs `NETSHAPE VERIFIED` at info level
// so a `grep "VERIFIED|LOST"` pass surfaces every apply.
//
// expectedMbps==0 means the apply was a CLEAR; the verification then
// asserts the class is gone (ReadActualRateMbps returns -1).
func (t *TcTrafficManager) scheduleRateLimitVerification(port int, expectedMbps float64, delay time.Duration) {
	go func() {
		time.Sleep(delay)
		actualMbps := t.ReadActualRateMbps(port)
		if expectedMbps == 0 {
			if actualMbps >= 0 {
				log.Printf("NETSHAPE LOST port=%d expected=clear kernel_mbps=%.3f delay=%s",
					port, actualMbps, delay)
			} else {
				log.Printf("NETSHAPE VERIFIED port=%d cleared delay=%s", port, delay)
			}
			return
		}
		if actualMbps < 0 {
			log.Printf("NETSHAPE LOST port=%d expected_mbps=%.3f kernel=no_class delay=%s",
				port, expectedMbps, delay)
			return
		}
		if math.Abs(actualMbps-expectedMbps) > 0.5 {
			log.Printf("NETSHAPE LOST port=%d expected_mbps=%.3f kernel_mbps=%.3f delay=%s",
				port, expectedMbps, actualMbps, delay)
			return
		}
		log.Printf("NETSHAPE VERIFIED port=%d mbps=%.3f delay=%s", port, actualMbps, delay)
	}()
}

// ReadActualRateMbps queries the kernel for the live rate of the
// per-port class, returning -1 if no class is installed. Used by the
// session-list API to surface kernel state instead of in-memory state
// (issue #352 layer 3) so any divergence is visible to operators.
//
// `tc class show` output for an htb class looks like:
//   class htb 1:181 parent 1:1 prio 0 rate 15414Kbit ceil 15414Kbit ...
// We parse the "rate" token. Kbit and Mbit are the only units the
// proxy ever installs, so the parser handles both.
func (t *TcTrafficManager) ReadActualRateMbps(port int) float64 {
	classid := fmt.Sprintf("1:%d", port%1000)
	show := exec.Command("tc", "class", "show", "dev", t.interfaceName)
	out, err := show.CombinedOutput()
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "class htb "+classid) {
			continue
		}
		// Look for "rate <N>Kbit" or "rate <N>Mbit"
		fields := strings.Fields(line)
		for i, f := range fields {
			if f != "rate" || i+1 >= len(fields) {
				continue
			}
			tok := fields[i+1]
			if strings.HasSuffix(tok, "Mbit") {
				v, err := strconv.ParseFloat(strings.TrimSuffix(tok, "Mbit"), 64)
				if err == nil {
					return v
				}
			}
			if strings.HasSuffix(tok, "Kbit") {
				v, err := strconv.ParseFloat(strings.TrimSuffix(tok, "Kbit"), 64)
				if err == nil {
					return v / 1000.0
				}
			}
		}
	}
	return -1
}

func (t *TcTrafficManager) RemoveFilter(port int) error {
	cmd := exec.Command(
		"tc", "filter", "del", "dev", t.interfaceName, "protocol", "ip", "parent", "1:0", "prio", "1", "u32",
		"match", "ip", "sport", fmt.Sprintf("%d", port), "0xffff",
	)
	_ = cmd.Run()
	return nil
}

func (t *TcTrafficManager) ensurePortFilter(port int, classid string) error {
	showFilters := exec.Command("tc", "filter", "show", "dev", t.interfaceName)
	filterOut, _ := showFilters.CombinedOutput()
	desiredHex := fmt.Sprintf("%04x0000/ffff0000", port)
	if strings.Contains(string(filterOut), desiredHex) {
		return nil
	}
	filterCmd := exec.Command(
		"tc", "filter", "add", "dev", t.interfaceName, "protocol", "ip", "parent", "1:0", "prio", "1", "u32",
		"match", "ip", "sport", fmt.Sprintf("%d", port), "0xffff", "flowid", classid,
	)
	if out, err := filterCmd.CombinedOutput(); err != nil {
		log.Printf("NETSHAPE tc filter add (sport) failed port=%d: %s", port, strings.TrimSpace(string(out)))
		fallbackCmd := exec.Command(
			"tc", "filter", "add", "dev", t.interfaceName, "protocol", "ip", "parent", "1:0", "prio", "1", "u32",
			"match", "ip", "dport", fmt.Sprintf("%d", port), "0xffff", "flowid", classid,
		)
		if out2, err2 := fallbackCmd.CombinedOutput(); err2 != nil {
			return fmt.Errorf("tc filter add failed: %s; fallback failed: %s", strings.TrimSpace(string(out)), strings.TrimSpace(string(out2)))
		}
	}
	return nil
}

func (t *TcTrafficManager) EnsureClass(port int, rateMbps float64) error {
	if err := t.EnsureRootQdisc(); err != nil {
		return err
	}
	if err := t.EnsureRootClass(); err != nil {
		return err
	}
	cmd := exec.Command("tc", "class", "show", "dev", t.interfaceName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return err
	}
	portSuffix := fmt.Sprintf("%03d", port%1000)
	classid := fmt.Sprintf("1:%s", portSuffix)
	if strings.Contains(string(output), classid) {
		return t.ensurePortFilter(port, classid)
	}
	return t.UpdateRateLimit(port, rateMbps)
}

func (t *TcTrafficManager) UpdateNetem(port int, delayMs int, lossPct float64) error {
	if err := t.EnsureRootQdisc(); err != nil {
		return err
	}
	if err := t.EnsureRootClass(); err != nil {
		return err
	}
	if delayMs > 0 || lossPct > 0 {
		if err := t.EnsureClass(port, 10000); err != nil {
			return err
		}
	}
	portSuffix := fmt.Sprintf("%03d", port%1000)
	classid := fmt.Sprintf("1:%s", portSuffix)
	handle := fmt.Sprintf("%s0:", portSuffix)
	if delayMs <= 0 && lossPct <= 0 {
		_ = exec.Command("tc", "qdisc", "del", "dev", t.interfaceName, "parent", classid, "handle", handle, "netem").Run()
		t.logTcState("netem_clear", port)
		return nil
	}
	jitter := delayMs / 2
	args := []string{"qdisc", "replace", "dev", t.interfaceName, "parent", classid, "handle", handle, "netem"}
	if delayMs > 0 {
		if jitter > 0 {
			args = append(args, "delay", fmt.Sprintf("%dms", delayMs), fmt.Sprintf("%dms", jitter), "distribution", "normal")
		} else {
			args = append(args, "delay", fmt.Sprintf("%dms", delayMs))
		}
	}
	if lossPct > 0 {
		args = append(args, "loss", fmt.Sprintf("%.2f%%", lossPct))
	}
	cmd := exec.Command("tc", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tc netem failed: %s", strings.TrimSpace(string(out)))
	}
	t.logTcState("netem_apply", port)
	return nil
}

func (t *TcTrafficManager) logTcState(reason string, port int) {
	if !t.debug {
		return
	}
	type tcCmd struct {
		label string
		args  []string
	}
	cmds := []tcCmd{
		{label: "qdisc", args: []string{"qdisc", "show", "dev", t.interfaceName}},
		{label: "class", args: []string{"class", "show", "dev", t.interfaceName}},
		{label: "filter", args: []string{"filter", "show", "dev", t.interfaceName}},
	}
	for _, cmd := range cmds {
		out, err := exec.Command("tc", cmd.args...).CombinedOutput()
		if err != nil {
			log.Printf("NETSHAPE_TC_STATE tc_%s dev=%s reason=%s port=%d error=%v output=%s",
				cmd.label,
				t.interfaceName,
				reason,
				port,
				err,
				strings.TrimSpace(string(out)),
			)
			continue
		}
		log.Printf("NETSHAPE_TC_STATE tc_%s dev=%s reason=%s port=%d output=%s",
			cmd.label,
			t.interfaceName,
			reason,
			port,
			strings.TrimSpace(string(out)),
		)
	}
}

func (t *TcTrafficManager) GetNetemConfig(port int) (map[string]interface{}, error) {
	cmd := exec.Command("tc", "qdisc", "show", "dev", t.interfaceName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}
	portSuffix := fmt.Sprintf("%03d", port%1000)
	parent := fmt.Sprintf("parent 1:%s", portSuffix)
	config := map[string]interface{}{
		"packet_loss": nil,
		"delay_ms":    nil,
	}
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, parent) && strings.Contains(line, "netem") {
			delayMs := parseNetemDelay(line)
			if delayMs > 0 {
				config["delay_ms"] = delayMs
			}
			loss := parseNetemLoss(line)
			if loss > 0 {
				config["packet_loss"] = loss
			}
			return config, nil
		}
	}
	return config, nil
}

// ensureNL initialises the persistent netlink handle and resolves the interface
// on first call. Subsequent calls are a fast mutex-check and return. The caller
// must hold no locks.
func (t *TcTrafficManager) ensureNL() (*netlink.Handle, netlink.Link, error) {
	t.nlMu.Lock()
	defer t.nlMu.Unlock()
	if t.nlHandle != nil {
		return t.nlHandle, t.nlLink, nil
	}
	h, err := netlink.NewHandle()
	if err != nil {
		return nil, nil, fmt.Errorf("netlink: new handle: %w", err)
	}
	link, err := h.LinkByName(t.interfaceName)
	if err != nil {
		h.Delete()
		return nil, nil, fmt.Errorf("netlink: link %s: %w", t.interfaceName, err)
	}
	t.nlHandle = h
	t.nlLink = link
	return h, link, nil
}

// GetPortStats returns the cumulative bytes sent and the current TC queue backlog
// for the HTB class associated with port. Uses a persistent netlink handle
// (no fork/exec); backlog returns -1 when queue stats are absent.
func (t *TcTrafficManager) GetPortStats(port int) (bytes int64, backlog int64, err error) {
	h, link, err := t.ensureNL()
	if err != nil {
		return 0, -1, err
	}

	// TC classids are written as "1:<portSuffix>" where portSuffix is the decimal
	// port%1000 formatted as a string — but TC interprets the minor part as hex.
	// e.g. port 30181 → classid "1:181" → TC reads minor as 0x181 = 385 decimal.
	portSuffix := port % 1000
	minorHex, _ := strconv.ParseUint(fmt.Sprintf("%03d", portSuffix), 16, 16)
	targetHandle := netlink.MakeHandle(1, uint16(minorHex))

	// Pass HANDLE_NONE (0) to dump all classes on the interface, then filter
	// by handle. Kernel-side parent filtering in RTM_GETTCLASS|NLM_F_DUMP is
	// unreliable across kernel versions — iproute2's tc does the same.
	t.nlMu.Lock()
	classes, classErr := h.ClassList(link, netlink.HANDLE_NONE)
	t.nlMu.Unlock()
	if classErr != nil {
		return 0, -1, classErr
	}

	for _, class := range classes {
		attrs := class.Attrs()
		if attrs.Handle != targetHandle {
			continue
		}
		if attrs.Statistics == nil {
			return 0, -1, nil
		}
		if attrs.Statistics.Basic != nil {
			bytes = int64(attrs.Statistics.Basic.Bytes)
		}
		backlog = -1
		if attrs.Statistics.Queue != nil {
			backlog = int64(attrs.Statistics.Queue.Backlog)
		}
		return bytes, backlog, nil
	}
	if t.debug {
		log.Printf("NL_GET_PORT_STATS port=%d handle=0x%08x class_not_found total_classes=%d", port, targetHandle, len(classes))
	}
	return 0, -1, nil
}

// GetPortBytes is a convenience wrapper kept for callers that only need byte counters.
func (t *TcTrafficManager) GetPortBytes(port int) (int64, error) {
	b, _, err := t.GetPortStats(port)
	return b, err
}

func rateToMbps(rateStr string) interface{} {
	if strings.HasSuffix(rateStr, "Kbit") {
		value := strings.TrimSuffix(rateStr, "Kbit")
		v, _ := strconv.ParseFloat(value, 64)
		return v / 1000
	}
	if strings.HasSuffix(rateStr, "Mbit") {
		value := strings.TrimSuffix(rateStr, "Mbit")
		v, _ := strconv.ParseFloat(value, 64)
		return v
	}
	if strings.HasSuffix(rateStr, "Gbit") {
		value := strings.TrimSuffix(rateStr, "Gbit")
		v, _ := strconv.ParseFloat(value, 64)
		return v * 1000
	}
	return nil
}

func parseNetemDelay(line string) int {
	match := netemDelayRegex.FindStringSubmatch(line)
	if len(match) == 2 {
		val, _ := strconv.ParseFloat(match[1], 64)
		return int(math.Round(val))
	}
	return 0
}

func parseNetemLoss(line string) float64 {
	match := netemLossRegex.FindStringSubmatch(line)
	if len(match) == 2 {
		val, _ := strconv.ParseFloat(match[1], 64)
		return val
	}
	return 0
}

func parseTcBytes(line string) int64 {
	match := tcSentBytesRegex.FindStringSubmatch(line)
	if len(match) == 2 {
		val, _ := strconv.ParseInt(match[1], 10, 64)
		return val
	}
	return 0
}

// parseTcBacklog parses the TC queue backlog bytes from a line like:
//
//	rate 1.99Mbit 123pps backlog 45678b 20p requeues 0
//
// Returns -1 if the pattern is not found (so callers can distinguish zero-backlog from absent).
func parseTcBacklog(line string) int64 {
	match := tcBacklogRegex.FindStringSubmatch(line)
	if len(match) == 2 {
		val, _ := strconv.ParseInt(match[1], 10, 64)
		return val
	}
	return -1
}

func loadPortMapping() PortMapping {
	externalBase := getenvIntAny([]string{"EXTERNAL_PORT_BASE"}, 0)
	internalBase := getenvIntAny([]string{"INTERNAL_PORT_BASE"}, 0)
	count := getenvIntAny([]string{"PORT_RANGE_COUNT", "PORT_MAP_COUNT"}, 0)
	if externalBase <= 0 || internalBase <= 0 || count <= 0 {
		return PortMapping{}
	}
	return PortMapping{
		externalBase: externalBase,
		internalBase: internalBase,
		count:        count,
	}
}

func (m PortMapping) MapExternalPort(port string) (string, bool) {
	if m.count <= 0 || m.externalBase <= 0 || m.internalBase <= 0 {
		return port, false
	}
	value, err := strconv.Atoi(port)
	if err != nil {
		return port, false
	}
	externalGroup := m.externalBase / 1000
	internalGroup := m.internalBase / 1000
	if externalGroup <= 0 || internalGroup <= 0 {
		return port, false
	}
	if value/1000 != externalGroup {
		return port, false
	}
	if m.count > 0 {
		digit := thirdFromLastDigit(strconv.Itoa(value))
		if digit == "" {
			return port, false
		}
		sessionDigit := int(digit[0] - '0')
		if sessionDigit < 0 || sessionDigit > m.count {
			return port, false
		}
	}
	mapped := (internalGroup * 1000) + (value % 1000)
	return strconv.Itoa(mapped), true
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	upstreamHost := getenvAny([]string{"INFINITE_STREAM_UPSTREAM_HOST", "INFINITE_UPSTREAM_HOST", "ISM_UPSTREAM_HOST"}, "127.0.0.1")
	upstreamPort := getenvAny([]string{"INFINITE_STREAM_UPSTREAM_PORT", "INFINITE_UPSTREAM_PORT", "ISM_UPSTREAM_PORT"}, "30000")
	maxSessions := getenvIntAny([]string{"INFINITE_STREAM_MAX_SESSIONS", "INFINITE_MAX_SESSIONS", "ISM_MAX_SESSIONS"}, 8)
	interfaceName := getenvAny([]string{"INFINITE_STREAM_TC_INTERFACE", "INFINITE_TC_INTERFACE", "TC_INTERFACE"}, "eth0")
	tcDebug := getenvBoolAny([]string{"INFINITE_STREAM_TC_DEBUG", "INFINITE_TC_DEBUG", "TC_DEBUG"}, false)
	eventStore, eventStoreErr := newSessionEventStore(getenv("GO_PROXY_SESSION_EVENTS_DB", defaultSessionEventsDB))
	if eventStoreErr != nil {
		log.Printf("session event store disabled: %v", eventStoreErr)
	}

	emptySessions := []SessionData{}
	app := &App{
		throughputData: map[int]map[string]interface{}{},
		sessionEvents: eventStore,
		traffic:       NewTcTrafficManager(interfaceName, tcDebug),
		upstreamHost:  upstreamHost,
		upstreamPort:  upstreamPort,
		maxSessions:   maxSessions,
		portMap:       loadPortMapping(),
		client: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 6 * time.Second}).DialContext,
				ResponseHeaderTimeout: 6 * time.Second,
			},
		},
		shapeLoops:         map[int]context.CancelFunc{},
		shapeStates:        map[int]NftShapePattern{},
		shapeApply:         map[int]ShapeApplyState{},
		faultLoops:         map[int]context.CancelFunc{},
		sessionsHub:        NewSessionEventHub(),
		networkHub:         NewNetworkEventHub(),
		networkLogs:        map[string]*NetworkLogRingBuffer{},
		loopStateBySession: map[string]ServerLoopState{},
		segmentFlight:      map[int]segmentFlightInfo{},
		segmentRun:         map[int]segmentRunRecord{},
		drainActive:        map[int]bool{},
		tcSamples:          map[int][]tcSample{},
		wireRate:            map[int]wireRateSample{},
		tcCache:             map[int]*tcStatsCache{},
		transferCompleteMbps:    map[int]float64{},
		transferCompleteAt:      map[int]time.Time{},
	}

	app.sessionsSnap.Store(&emptySessions)

	go app.trackPortThroughput()
	app.restoreTransportFaultSchedules()

	router := mux.NewRouter()
	router.Use(corsMiddleware)

	router.HandleFunc("/index.html", app.handleIndex).Methods(http.MethodGet)
	router.HandleFunc("/api/sessions", app.handleGetSessions).Methods(http.MethodGet)
	router.HandleFunc("/api/sessions/stream", app.handleSessionStream).Methods(http.MethodGet)
	router.HandleFunc("/api/failure-settings/{id}", app.handleUpdateFailureSettings).Methods(http.MethodPost)
	router.HandleFunc("/api/session/{id}/update", app.handleUpdateSessionSettings).Methods(http.MethodPost)
	router.HandleFunc("/api/session/{id}", app.handleSession).Methods(http.MethodGet, http.MethodDelete)
	router.HandleFunc("/api/session/{id}", app.handlePatchSession).Methods(http.MethodPatch)
	router.HandleFunc("/api/session/{id}/metrics", app.handlePostSessionMetrics).Methods(http.MethodPost)
	router.HandleFunc("/api/session/{id}/network", app.handleGetNetworkLog).Methods(http.MethodGet)
	router.HandleFunc("/api/network/stream", app.handleNetworkStream).Methods(http.MethodGet)
	router.HandleFunc("/api/external-ips", app.handleGetExternalIPs).Methods(http.MethodGet)
	router.HandleFunc("/api/clear-sessions", app.handleClearSessions).Methods(http.MethodPost)
	router.HandleFunc("/api/session-group/link", app.handleLinkSessions).Methods(http.MethodPost)
	router.HandleFunc("/api/session-group/unlink", app.handleUnlinkSession).Methods(http.MethodPost)
	router.HandleFunc("/api/session-group/{groupId}", app.handleGetGroup).Methods(http.MethodGet)
	router.HandleFunc("/myshows", app.handleMyShows).Methods(http.MethodGet)
	router.HandleFunc("/debug", app.handleDebug).Methods(http.MethodGet)
	router.HandleFunc("/api/nftables/status", app.handleNftStatus).Methods(http.MethodGet)
	router.HandleFunc("/api/nftables/capabilities", app.handleNftCapabilities).Methods(http.MethodGet)
	router.HandleFunc("/api/version", app.handleVersion).Methods(http.MethodGet)
	router.HandleFunc("/api/nftables/port/{port}", app.handleNftPort).Methods(http.MethodGet)
	router.HandleFunc("/api/nftables/bandwidth/{port}", app.handleNftBandwidth).Methods(http.MethodPost)
	router.HandleFunc("/api/nftables/loss/{port}", app.handleNftLoss).Methods(http.MethodPost)
	router.HandleFunc("/api/nftables/shape/{port}", app.handleNftShape).Methods(http.MethodPost)
	router.HandleFunc("/api/nftables/pattern/{port}", app.handleNftPattern).Methods(http.MethodPost)
	router.HandleFunc("/close-socket", app.handleCloseSocket).Methods(http.MethodGet)
	router.HandleFunc("/terminate-worker", app.handleTerminateWorker).Methods(http.MethodGet)
	router.HandleFunc("/force-close", app.handleForceClose).Methods(http.MethodGet)

	router.PathPrefix("/").HandlerFunc(app.handleProxy)

	ports := []int{30081, 30181, 30281, 30381, 30481, 30581, 30681, 30781, 30881}

	errorCh := make(chan error, len(ports))
	for _, port := range ports {
		addr := fmt.Sprintf(":%d", port)
		go func(bind string) {
			log.Printf("go-proxy listening on %s", bind)
			srv := &http.Server{
				Addr:    bind,
				Handler: router,
			}
			errorCh <- srv.ListenAndServe()
		}(addr)
	}

	err := <-errorCh
	if err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With, Player-ID, X-Playback-Session-Id, X-Forwarded-Port")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) handleVersion(w http.ResponseWriter, r *http.Request) {
	version := strings.TrimSpace(versionString)
	if version == "" {
		version = "unknown"
	}
	writeJSON(w, map[string]string{"version": version})
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	a.removeInactiveSessions()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

func (a *App) handleGetSessions(w http.ResponseWriter, r *http.Request) {
	a.removeInactiveSessions()
	sessions := a.getSessionList()
	if shouldScopeSessionsByRequesterIP(r) {
		requesterIP := extractClientIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"))
		sessions = filterSessionsByOriginationIP(sessions, requesterIP)
	}
	if len(sessions) > 10 {
		sessions = sessions[:10]
	}
	writeJSON(w, a.normalizeSessionsForResponse(sessions))
}

// handleNetworkStream emits each network log entry as it lands in any
// session's ring buffer. Body is one SSE `data:` line per entry,
// {"session_id":"...","entry":{...}}. Subscribers must reconnect on
// disconnect; nothing is replayed.
func (a *App) handleNetworkStream(w http.ResponseWriter, r *http.Request) {
	if a.networkHub == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{"error": "stream unavailable"})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": "stream unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	clientID, ch := a.networkHub.AddClient(1024)
	defer a.networkHub.RemoveClient(clientID)

	// Heartbeat keeps idle proxies through corp firewalls/load balancers.
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			payload := struct {
				SessionID string          `json:"session_id"`
				Entry     NetworkLogEntry `json:"entry"`
			}{ev.SessionID, ev.Entry}
			b, err := json.Marshal(payload)
			if err != nil {
				continue
			}
			if _, err := w.Write([]byte("data: " + string(b) + "\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (a *App) handleSessionStream(w http.ResponseWriter, r *http.Request) {
	if a.sessionsHub == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{"error": "stream unavailable"})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": "stream unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Tell EventSource to retry every 1s (default 3s) on a clean
	// reconnect path. Combined with the client-side watchdog this
	// keeps recovery snappy after a server redeploy.
	if _, err := w.Write([]byte("retry: 1000\n\n")); err != nil {
		return
	}
	flusher.Flush()

	playerIDFilter := r.URL.Query().Get("player_id")

	a.removeInactiveSessions()
	sessions := a.getSessionList()
	normalized := a.normalizeSessionsForResponse(sessions)
	var initActive []ActiveSessionInfo
	if playerIDFilter != "" {
		initActive = buildActiveSessionsSummary(normalized)
		normalized = filterSessionsByPlayerID(normalized, playerIDFilter)
	}
	rev := atomic.AddUint64(&a.sessionsBroadcastSeq, 1)
	payload := a.buildSessionsEvent(normalized, rev, 0, initActive)
	if payload != "" {
		_, _ = w.Write([]byte(payload))
		flusher.Flush()
	}

	clientID, ch := a.sessionsHub.AddClient(playerIDFilter)
	defer a.sessionsHub.RemoveClient(clientID)

	// Heartbeat so the client can distinguish "connection alive but
	// idle" from "connection dead". 5 s cadence pairs with the
	// dashboard's 12 s watchdog (one missed heartbeat + 2 s margin)
	// so a silent SSE connection recovers within ~12 s instead of
	// the original 30 s. Also keeps middlebox idle timeouts from
	// silently dropping the SSE TCP connection.
	heartbeat := time.NewTicker(5 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := w.Write([]byte("event: heartbeat\ndata: {}\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case event, ok := <-ch:
			if !ok {
				return
			}
			var payload string
			if event.PreMarshaled != "" {
				payload = event.PreMarshaled
			} else {
				filtered := event.Sessions
				var active []ActiveSessionInfo
				if playerIDFilter != "" {
					active = buildActiveSessionsSummary(filtered)
					filtered = filterSessionsByPlayerID(filtered, playerIDFilter)
					if len(filtered) == 0 {
						continue
					}
				}
				payload = a.buildSessionsEvent(filtered, event.Revision, event.Dropped, active)
			}
			if payload == "" {
				continue
			}
			_, _ = w.Write([]byte(payload))
			flusher.Flush()
		}
	}
}

func (a *App) handlePatchSession(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	var payload SessionPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid json"})
		return
	}
	set := payload.Set
	if set == nil {
		set = map[string]interface{}{}
	}
	fields := payload.Fields
	if len(fields) == 0 {
		for key := range set {
			fields = append(fields, key)
		}
	}
	filtered := map[string]interface{}{}
	for _, key := range fields {
		if value, ok := set[key]; ok {
			filtered[key] = value
		}
	}
	if len(filtered) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "no fields provided"})
		return
	}
	session, status, errMsg := a.applySessionSettingsUpdate(id, filtered, payload.BaseRevision)
	if status != http.StatusOK {
		w.WriteHeader(status)
		if status == http.StatusConflict {
			normalized := a.normalizeSessionForResponse(session)
			writeJSON(w, map[string]interface{}{
				"error":            errMsg,
				"session":          normalized,
				"control_revision": getString(normalized, "control_revision"),
			})
			return
		}
		if errMsg == "" {
			errMsg = "update failed"
		}
		writeJSON(w, map[string]string{"error": errMsg})
		return
	}
	normalized := a.normalizeSessionForResponse(session)
	writeJSON(w, map[string]interface{}{
		"session":          normalized,
		"control_revision": getString(normalized, "control_revision"),
	})
}

// handlePostSessionMetrics updates player-reported observational data (frames,
// buffer depth, playback state) without bumping control_revision or triggering
// shaping/transport logic. This avoids revision conflicts with user-driven
// control changes (rate limit, failure settings).
func (a *App) handlePostSessionMetrics(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	var payload SessionPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid json"})
		return
	}
	set := payload.Set
	if set == nil || len(set) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "no fields provided"})
		return
	}
	metricsOnly := make(SessionData, len(set))
	for key, value := range set {
		metricsOnly[key] = value
	}
	metricsOnly["session_id"] = id
	// Stamp the moment we received this metrics payload, on the server's
	// own clock. Pairs with player_metrics_playhead_wallclock_ms (the
	// encoder's PDT at the playhead) so the dashboard can compute a
	// ground-truth live offset that's independent of the client's clock:
	//   trueOffsetMs = server_received_at_ms - playhead_wallclock_ms
	metricsOnly["server_received_at_ms"] = time.Now().UnixMilli()
	a.saveSessionByID(id, metricsOnly)
	lastEvent := strings.ToLower(strings.TrimSpace(getString(metricsOnly, "player_metrics_last_event")))
	if isSignificantPlayerEvent(lastEvent) {
		a.flushSessionsBroadcast()
	}
	// "user_marked" (the 911 button) flows into the analytics tier
	// via session_snapshots.last_event = 'user_marked'; the operator
	// triages it in the session viewer / picker. No on-disk HAR
	// snapshot is taken — historical sessions can be ZIP-bundled via
	// /analytics/api/session_bundle if forensic preservation past the
	// 30-day TTL is needed.
	w.WriteHeader(http.StatusOK)
	writeJSON(w, map[string]string{"status": "ok"})
}

func isSignificantPlayerEvent(event string) bool {
	switch strings.ToLower(strings.TrimSpace(event)) {
	case "stall_start", "stall_end", "segment_stall", "restart", "frozen", "error", "loop_marker", "playing", "buffering_start", "buffering_end", "rate_shift_up", "rate_shift_down", "video_first_frame", "video_start_time", "timejump", "user_marked":
		return true
	}
	return false
}


func (a *App) handleUpdateFailureSettings(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	var payload map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, map[string]string{"error": "invalid json"})
		return
	}
	_, status, errMsg := a.applySessionSettingsUpdate(id, payload, "")
	if status != http.StatusOK {
		if errMsg == "" {
			errMsg = "update failed"
		}
		w.WriteHeader(status)
		writeJSON(w, map[string]string{"error": errMsg})
		return
	}
	writeJSON(w, map[string]string{"message": "Settings updated successfully"})
}

func (a *App) applySessionSettingsUpdate(id string, payload map[string]interface{}, baseRevision string) (SessionData, int, string) {
	if payload == nil {
		payload = map[string]interface{}{}
	}
	log.Printf("SESSION GROUP UPDATE request session_id=%s keys=%d", id, len(payload))
	controlRevision := newControlRevision()
	transportKeys := []string{
		"transport_failure_type",
		"transport_consecutive_failures",
		"transport_failure_frequency",
		"transport_failure_units",
		"transport_consecutive_units",
		"transport_frequency_units",
		"transport_failure_mode",
		"transport_fault_type",
		"transport_fault_on_seconds",
		"transport_fault_off_seconds",
		"transport_consecutive_seconds",
		"transport_frequency_seconds",
	}
	transportUpdated := false
	for _, key := range transportKeys {
		if _, ok := payload[key]; ok {
			transportUpdated = true
			break
		}
	}
	shapeRateFields := []string{"nftables_bandwidth_mbps", "nftables_delay_ms", "nftables_packet_loss"}
	shapeRateUpdated := false
	shapeFieldsPresent := make([]string, 0, len(shapeRateFields))
	for _, key := range shapeRateFields {
		if _, ok := payload[key]; ok {
			shapeRateUpdated = true
			shapeFieldsPresent = append(shapeFieldsPresent, key)
		}
	}

	sessions := a.getSessionList()
	var target SessionData
	for _, session := range sessions {
		if getString(session, "session_id") == id {
			target = session
			break
		}
	}
	if target == nil {
		return nil, http.StatusNotFound, "Session not found"
	}
	updatedSessions := []SessionData{target}
	currentRevision := getString(target, "control_revision")
	if baseRevision != "" && currentRevision != "" && baseRevision != currentRevision {
		return target, http.StatusConflict, "revision_conflict"
	}

	var targetPort string
	targetGroupID := getString(target, "group_id")
	var transportSnapshot map[string]interface{}
	manualTransportDisarm := false
	var transportLogSession SessionData
	transportShouldApply := false
	transportFaultType := "none"
	transportConsecutive := 1
	transportConsecutiveUnits := transportUnitsSeconds
	transportFrequency := 0

	previousTransportType := normalizeTransportFaultType(getString(target, "transport_failure_type"))
	if previousTransportType == "none" {
		previousTransportType = normalizeTransportFaultType(getString(target, "transport_fault_type"))
	}

	for key, value := range payload {
		target[key] = value
	}
	if _, ok := payload["player_metrics_loop_count_player"]; ok {
		log.Printf(
			"LOOP_COUNTER_PATCH session_id=%s source=%s event=%s player_loop_count=%d loop_increment=%d server_loop_count=%d",
			id,
			getString(target, "player_metrics_source"),
			getString(target, "player_metrics_last_event"),
			getInt(target, "player_metrics_loop_count_player"),
			getInt(target, "player_metrics_loop_count_increment"),
			getInt(target, "loop_count_server"),
		)
	} else if _, ok := payload["player_metrics_loop_count_increment"]; ok {
		log.Printf(
			"LOOP_COUNTER_PATCH session_id=%s source=%s event=%s player_loop_count=%d loop_increment=%d server_loop_count=%d",
			id,
			getString(target, "player_metrics_source"),
			getString(target, "player_metrics_last_event"),
			getInt(target, "player_metrics_loop_count_player"),
			getInt(target, "player_metrics_loop_count_increment"),
			getInt(target, "loop_count_server"),
		)
	}
	applyControlRevision(target, controlRevision)
	shapeCommandSource := "session_patch"
	if shapeRateUpdated && getBool(target, "abrchar_run_lock") {
		shapeCommandSource = "abrchar"
	}
	if !shapeRateUpdated {
		log.Printf("SESSION_LIMIT_SKIP source=%s session_id=%s reason=shape_fields_missing expected=%s", shapeCommandSource, id, strings.Join(shapeRateFields, ","))
	} else {
		log.Printf("SESSION_LIMIT_UPDATE source=%s session_id=%s shape_fields_present=%s", shapeCommandSource, id, strings.Join(shapeFieldsPresent, ","))
	}
	for _, prefix := range []string{"segment", "manifest", "master_manifest"} {
		typeKey := prefix + "_failure_type"
		failureType := normalizeRequestFailureType(getString(target, typeKey))
		if failureType == "" {
			failureType = "none"
		}
		target[typeKey] = failureType
		resetKey := prefix + "_reset_failure_type"
		if resetType := getString(target, resetKey); resetType != "" {
			target[resetKey] = normalizeRequestFailureType(resetType)
		}
	}
	targetPort = getString(target, "x_forwarded_port")
	if transportUpdated {
		typeRaw := getString(target, "transport_failure_type")
		if typeRaw == "" {
			typeRaw = getString(target, "transport_fault_type")
		}
		if value, ok := payload["transport_failure_type"]; ok {
			typeRaw = fmt.Sprintf("%v", value)
		} else if value, ok := payload["transport_fault_type"]; ok {
			typeRaw = fmt.Sprintf("%v", value)
		}
		target["transport_failure_type"] = normalizeTransportFaultType(typeRaw)
		if target["transport_failure_type"] == "" {
			target["transport_failure_type"] = "none"
		}

		unitsRaw := getString(target, "transport_consecutive_units")
		if unitsRaw == "" {
			unitsRaw = getString(target, "transport_failure_units")
		}
		if unitsRaw == "" {
			unitsRaw = getString(target, "transport_failure_mode")
		}
		if value, ok := payload["transport_consecutive_units"]; ok {
			unitsRaw = fmt.Sprintf("%v", value)
		} else if value, ok := payload["transport_failure_units"]; ok {
			unitsRaw = fmt.Sprintf("%v", value)
		} else if value, ok := payload["transport_failure_mode"]; ok {
			unitsRaw = fmt.Sprintf("%v", value)
		}
		consecutiveUnits := normalizeTransportConsecutiveUnits(unitsRaw)
		if strings.Contains(strings.ToLower(strings.TrimSpace(unitsRaw)), "packet") {
			consecutiveUnits = transportUnitsPackets
		}

		onValue := floatFromInterface(target["transport_consecutive_failures"])
		if value, ok := payload["transport_consecutive_failures"]; ok {
			onValue = floatFromInterface(value)
		} else if value, ok := payload["transport_consecutive_seconds"]; ok {
			onValue = floatFromInterface(value)
		} else if value, ok := payload["transport_fault_on_seconds"]; ok {
			onValue = floatFromInterface(value)
		}
		onValue = math.Max(0, onValue)
		target["transport_consecutive_failures"] = int(math.Round(onValue))

		offSeconds := floatFromInterface(target["transport_failure_frequency"])
		if value, ok := payload["transport_failure_frequency"]; ok {
			offSeconds = floatFromInterface(value)
		} else if value, ok := payload["transport_frequency_seconds"]; ok {
			offSeconds = floatFromInterface(value)
		} else if value, ok := payload["transport_fault_off_seconds"]; ok {
			offSeconds = floatFromInterface(value)
		}
		offSeconds = math.Max(0, offSeconds)
		target["transport_failure_frequency"] = int(math.Round(offSeconds))

		target["transport_failure_units"] = consecutiveUnits
		target["transport_consecutive_units"] = consecutiveUnits
		target["transport_frequency_units"] = transportUnitsSeconds
		target["transport_failure_mode"] = transportModeFromConsecutiveUnits(consecutiveUnits)
		target["transport_fault_type"] = target["transport_failure_type"]
		target["transport_fault_on_seconds"] = float64(getInt(target, "transport_consecutive_failures"))
		target["transport_fault_off_seconds"] = float64(getInt(target, "transport_failure_frequency"))
		target["transport_consecutive_seconds"] = target["transport_fault_on_seconds"]
		target["transport_frequency_seconds"] = target["transport_fault_off_seconds"]
		target["transport_failure_at"] = nil
		target["transport_failure_recover_at"] = nil
		target["transport_reset_failure_type"] = nil
		target["transport_fault_started_at"] = nil
		target["transport_fault_active"] = false
		target["transport_fault_phase_seconds"] = 0.0
		target["transport_fault_cycle_seconds"] = 0.0
		transportShouldApply = true
		transportFaultType = normalizeTransportFaultType(getString(target, "transport_failure_type"))
		transportConsecutive = getInt(target, "transport_consecutive_failures")
		transportConsecutiveUnits = normalizeTransportConsecutiveUnits(getString(target, "transport_consecutive_units"))
		transportFrequency = getInt(target, "transport_failure_frequency")
		currentTransportType := normalizeTransportFaultType(getString(target, "transport_failure_type"))
		if previousTransportType != "none" && currentTransportType == "none" {
			manualTransportDisarm = true
			transportLogSession = target
		}
		transportSnapshot = map[string]interface{}{
			"transport_failure_type":         target["transport_failure_type"],
			"transport_consecutive_failures": target["transport_consecutive_failures"],
			"transport_failure_frequency":    target["transport_failure_frequency"],
			"transport_failure_units":        target["transport_failure_units"],
			"transport_consecutive_units":    target["transport_consecutive_units"],
			"transport_frequency_units":      target["transport_frequency_units"],
			"transport_failure_mode":         target["transport_failure_mode"],
			"transport_failure_at":           nil,
			"transport_failure_recover_at":   nil,
			"transport_reset_failure_type":   nil,
			"transport_fault_type":           target["transport_fault_type"],
			"transport_fault_on_seconds":     target["transport_fault_on_seconds"],
			"transport_fault_off_seconds":    target["transport_fault_off_seconds"],
			"transport_consecutive_seconds":  target["transport_consecutive_seconds"],
			"transport_frequency_seconds":    target["transport_frequency_seconds"],
			"transport_fault_started_at":     target["transport_fault_started_at"],
			"transport_fault_active":         false,
			"transport_fault_phase_seconds":  0.0,
			"transport_fault_cycle_seconds":  0.0,
		}
	}
	if transportUpdated && targetPort != "" && transportSnapshot != nil {
		for _, session := range sessions {
			if getString(session, "x_forwarded_port") != targetPort {
				continue
			}
			for key, value := range transportSnapshot {
				session[key] = value
			}
		}
	}
	if manualTransportDisarm {
		logFaultEvent(transportLogSession, targetPort, "transport_none", "control", "transport_disarm_manual")
	}

	if targetGroupID != "" {
		log.Printf("SESSION GROUP UPDATE propagate session_id=%s group_id=%s", id, targetGroupID)
		for _, session := range sessions {
			sessionGroupID := getString(session, "group_id")
			sessionID := getString(session, "session_id")
			if sessionID == id || sessionGroupID != targetGroupID {
				continue
			}
			log.Printf("SESSION GROUP UPDATE member session_id=%s group_id=%s", sessionID, sessionGroupID)
			for key, value := range payload {
				session[key] = value
			}
			applyControlRevision(session, controlRevision)
			for _, prefix := range []string{"segment", "manifest", "master_manifest"} {
				typeKey := prefix + "_failure_type"
				failureType := normalizeRequestFailureType(getString(session, typeKey))
				if failureType == "" {
					failureType = "none"
				}
				session[typeKey] = failureType
				resetKey := prefix + "_reset_failure_type"
				if resetType := getString(session, resetKey); resetType != "" {
					session[resetKey] = normalizeRequestFailureType(resetType)
				}
			}
			if transportUpdated && transportSnapshot != nil {
				for key, value := range transportSnapshot {
					session[key] = value
				}
				groupMemberPort := getString(session, "x_forwarded_port")
				if groupMemberPort != "" && transportShouldApply {
					if portNum, err := strconv.Atoi(groupMemberPort); err == nil {
						a.armTransportFaultLoop(portNum, transportFaultType, transportConsecutive, transportConsecutiveUnits, transportFrequency)
					}
				}
			}
			updatedSessions = append(updatedSessions, session)
		}
	}
	if targetGroupID == "" {
		log.Printf("SESSION GROUP UPDATE no group for session_id=%s", id)
	}
	// Only clear pattern state when shaping fields are updated WITHOUT a pattern
	// being enabled. If the payload includes both shaping fields and pattern_enabled=true,
	// the pattern takes ownership of the rate.
	patternInPayload := getBool(payload, "nftables_pattern_enabled")
	if shapeRateUpdated && !patternInPayload {
		for _, session := range updatedSessions {
			if session == nil {
				continue
			}
			session["nftables_pattern_enabled"] = false
			session["nftables_pattern_steps"] = []NftShapeStep{}
			session["nftables_pattern_step"] = nil
			session["nftables_pattern_step_runtime"] = nil
			session["nftables_pattern_rate_runtime_mbps"] = nil
			session["nftables_pattern_step_runtime_at"] = nil
		}
	}

	for _, session := range updatedSessions {
		if session == nil {
			continue
		}
		a.saveSessionByID(getString(session, "session_id"), session)
	}
	if transportShouldApply {
		if portNum, err := strconv.Atoi(targetPort); err == nil {
			a.armTransportFaultLoop(portNum, transportFaultType, transportConsecutive, transportConsecutiveUnits, transportFrequency)
		}
	}
	if shapeRateUpdated && !patternInPayload {
		portsApplied := map[int]struct{}{}
		skippedNil := 0
		skippedNoPort := 0
		skippedInvalidPort := 0
		skippedDuplicatePort := 0
		for _, session := range updatedSessions {
			if session == nil {
				skippedNil++
				continue
			}
			portStr := getString(session, "x_forwarded_port")
			if portStr == "" {
				skippedNoPort++
				log.Printf("SESSION_LIMIT_SKIP source=%s session_id=%s reason=missing_x_forwarded_port", shapeCommandSource, getString(session, "session_id"))
				continue
			}
			portNum, err := strconv.Atoi(portStr)
			if err != nil {
				skippedInvalidPort++
				log.Printf("SESSION_LIMIT_SKIP source=%s session_id=%s reason=invalid_x_forwarded_port value=%q", shapeCommandSource, getString(session, "session_id"), portStr)
				continue
			}
			if _, exists := portsApplied[portNum]; exists {
				skippedDuplicatePort++
				continue
			}
			portsApplied[portNum] = struct{}{}
			rateMbps := getFloat(session, "nftables_bandwidth_mbps")
			delayMs := getInt(session, "nftables_delay_ms")
			lossPct := getFloat(session, "nftables_packet_loss")
			sessionID := getString(session, "session_id")
			if shapeCommandSource == "abrchar" {
				log.Printf("ABRCHAR_LIMIT_CMD session_id=%s port=%d rate_mbps=%.3f delay_ms=%d loss_pct=%.3f control_revision=%s", sessionID, portNum, rateMbps, delayMs, lossPct, controlRevision)
			} else {
				log.Printf("SESSION_LIMIT_CMD source=%s session_id=%s port=%d rate_mbps=%.3f delay_ms=%d loss_pct=%.3f control_revision=%s", shapeCommandSource, sessionID, portNum, rateMbps, delayMs, lossPct, controlRevision)
			}
			a.stopShapeLoop(portNum)
			if shapeCommandSource == "abrchar" {
				log.Printf("ABRCHAR_LIMIT_APPLY session_id=%s port=%d rate_mbps=%.3f delay_ms=%d loss_pct=%.3f", sessionID, portNum, rateMbps, delayMs, lossPct)
			} else {
				log.Printf("SESSION_LIMIT_APPLY source=%s session_id=%s port=%d rate_mbps=%.3f delay_ms=%d loss_pct=%.3f", shapeCommandSource, sessionID, portNum, rateMbps, delayMs, lossPct)
			}
			a.applySessionShaping(session, portNum)
		}
		log.Printf(
			"SESSION_LIMIT_DISPATCH source=%s session_id=%s updated_sessions=%d applied_ports=%d skipped_nil=%d skipped_missing_port=%d skipped_invalid_port=%d skipped_duplicate_port=%d",
			shapeCommandSource,
			id,
			len(updatedSessions),
			len(portsApplied),
			skippedNil,
			skippedNoPort,
			skippedInvalidPort,
			skippedDuplicatePort,
		)
	}
	// Start or stop the pattern loop if pattern fields were included in the PATCH.
	if _, hasPatternEnabled := payload["nftables_pattern_enabled"]; hasPatternEnabled {
		patternEnabled := getBool(target, "nftables_pattern_enabled")
		portStr := getString(target, "x_forwarded_port")
		if portNum, err := strconv.Atoi(portStr); err == nil && portNum > 0 {
			if patternEnabled {
				steps := parseShapeStepsFromSession(target)
				if len(steps) > 0 {
					delayMs := getInt(target, "nftables_delay_ms")
					loss := getFloat(target, "nftables_packet_loss")
					log.Printf("SESSION_PATTERN_START source=session_patch session_id=%s port=%d steps=%d", id, portNum, len(steps))
					if err := a.applyShapePattern(portNum, steps, delayMs, loss); err != nil {
						log.Printf("SESSION_PATTERN_START_FAILED session_id=%s port=%d: %v", id, portNum, err)
					}
				}
			} else {
				log.Printf("SESSION_PATTERN_STOP source=session_patch session_id=%s port=%d", id, portNum)
				a.stopShapeLoop(portNum)
			}
		}
	}

	return target, http.StatusOK, ""
}

// parseShapeStepsFromSession extracts []NftShapeStep from the session's
// nftables_pattern_steps field (stored as []interface{} from JSON).
func parseShapeStepsFromSession(session SessionData) []NftShapeStep {
	raw, ok := session["nftables_pattern_steps"]
	if !ok {
		return nil
	}
	switch steps := raw.(type) {
	case []NftShapeStep:
		return steps
	case []interface{}:
		out := make([]NftShapeStep, 0, len(steps))
		for _, item := range steps {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			step := NftShapeStep{
				RateMbps:        getFloat(m, "rate_mbps"),
				DurationSeconds: getFloat(m, "duration_seconds"),
				Enabled:         true,
			}
			if v, ok := m["enabled"]; ok {
				if b, ok := v.(bool); ok {
					step.Enabled = b
				}
			}
			if step.RateMbps > 0 && step.DurationSeconds > 0 {
				out = append(out, step)
			}
		}
		return out
	}
	return nil
}

func (a *App) handleUpdateSessionSettings(w http.ResponseWriter, r *http.Request) {
	a.handleUpdateFailureSettings(w, r)
}

func (a *App) handleSession(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if r.Method == http.MethodDelete {
		sessions := a.getSessionList()
		filtered := make([]SessionData, 0, len(sessions))
		removedPorts := map[int]struct{}{}
		for _, session := range sessions {
			if getString(session, "session_id") != id {
				filtered = append(filtered, session)
				continue
			}
			a.removeServerLoopState(id)
			a.recordSessionEnd(session, "deleted")
			if port, err := strconv.Atoi(getString(session, "x_forwarded_port")); err == nil {
				removedPorts[port] = struct{}{}
			}
		}
		a.saveSessionList(filtered)
		for port := range removedPorts {
			a.disablePatternForPort(port)
			a.armTransportFaultLoop(port, "none", 1, transportUnitsSeconds, 0)
			// Tear down any tc rate limit / netem / filter on this
			// port so a future session that reuses the slot starts
			// clean — pairs with the ClearPortShaping at session-
			// allocation time as belt-and-braces (issue #352).
			if a.traffic != nil {
				_ = a.traffic.UpdateNetem(port, 0, 0)
				a.traffic.ClearPortShaping(port)
			}
		}
		writeJSON(w, map[string]string{"message": "Session deleted successfully"})
		return
	}
	if session := a.getSessionData(id); session != nil {
		for _, prefix := range []string{"segment", "manifest", "playlist"} {
			typeKey := prefix + "_failure_type"
			failureType := normalizeRequestFailureType(getString(session, typeKey))
			if failureType == "" {
				failureType = "none"
			}
			session[typeKey] = failureType
		}
		dropPackets := int64FromInterface(session["transport_fault_drop_packets"])
		rejectPackets := int64FromInterface(session["transport_fault_reject_packets"])
		if port, err := strconv.Atoi(getString(session, "x_forwarded_port")); err == nil {
			if counters, ok := getTransportFaultRuleCounters()[port]; ok {
				dropPackets = counters.DropPackets
				rejectPackets = counters.RejectPackets
			}
		}
		port := getString(session, "x_forwarded_port_external")
		if port == "" {
			port = getString(session, "x_forwarded_port")
		}
		if port != "" {
			applySessionThroughput(session, a.getSessionThroughput(session))
		}
		session["transport_fault_drop_packets"] = dropPackets
		session["transport_fault_reject_packets"] = rejectPackets
		writeJSON(w, session)
		return
	}
	w.WriteHeader(http.StatusNotFound)
	writeJSON(w, map[string]string{"error": "Session not found"})
}

func (a *App) handleGetNetworkLog(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]

	a.networkLogsMu.RLock()
	ringBuffer, exists := a.networkLogs[id]
	a.networkLogsMu.RUnlock()

	if !exists {
		writeJSON(w, map[string]interface{}{
			"session_id": id,
			"entries":    []NetworkLogEntry{},
		})
		return
	}

	entries := ringBuffer.GetAll()
	writeJSON(w, map[string]interface{}{
		"session_id": id,
		"entries":    entries,
		"count":      len(entries),
	})
}

func (a *App) handleGetExternalIPs(w http.ResponseWriter, r *http.Request) {
	sessionList := a.getSessionList()
	if shouldScopeSessionsByRequesterIP(r) {
		requesterIP := extractClientIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"))
		sessionList = filterSessionsByOriginationIP(sessionList, requesterIP)
	}

	type ExternalIPEntry struct {
		SessionID       string `json:"session_id"`
		PlayerID        string `json:"player_id"`
		OriginationIP   string `json:"origination_ip"`
		OriginationTime string `json:"origination_time"`
		LastRequestTime string `json:"last_request_time"`
		IsExternal      bool   `json:"is_external"`
		UserAgent       string `json:"user_agent,omitempty"`
	}

	var externalIPs []ExternalIPEntry
	var allIPs []ExternalIPEntry

	for _, session := range sessionList {
		originIP := getString(session, "origination_ip")
		if originIP == "" {
			continue
		}

		entry := ExternalIPEntry{
			SessionID:       getString(session, "session_id"),
			PlayerID:        getString(session, "player_id"),
			OriginationIP:   originIP,
			OriginationTime: getString(session, "origination_time"),
			LastRequestTime: getString(session, "last_request"),
			IsExternal:      getBool(session, "is_external_ip"),
			UserAgent:       getString(session, "user_agent"),
		}

		allIPs = append(allIPs, entry)
		if entry.IsExternal {
			externalIPs = append(externalIPs, entry)
		}
	}

	// Check for filter parameter
	filter := r.URL.Query().Get("filter")
	var result []ExternalIPEntry

	if filter == "external" {
		result = externalIPs
	} else {
		result = allIPs
	}

	writeJSON(w, map[string]interface{}{
		"entries":       result,
		"total":         len(result),
		"external_only": filter == "external",
	})
}

func (a *App) handleClearSessions(w http.ResponseWriter, r *http.Request) {
	sessions := a.getSessionList()
	portSet := map[int]struct{}{}
	a.shapeMu.Lock()
	for port := range a.shapeLoops {
		portSet[port] = struct{}{}
	}
	a.shapeMu.Unlock()
	for _, session := range sessions {
		a.removeServerLoopState(getString(session, "session_id"))
		a.recordSessionEnd(session, "cleared")
		portStr := getString(session, "x_forwarded_port")
		if portStr == "" {
			continue
		}
		if port, err := strconv.Atoi(portStr); err == nil {
			portSet[port] = struct{}{}
		}
	}
	ports := make([]int, 0, len(portSet))
	for port := range portSet {
		ports = append(ports, port)
	}
	for _, port := range ports {
		a.disablePatternForPort(port)
		a.armTransportFaultLoop(port, "none", 1, transportUnitsSeconds, 0)
	}
	a.saveSessionList([]SessionData{})
	writeJSON(w, map[string]string{"message": "All sessions cleared successfully"})
}

func (a *App) handleLinkSessions(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		SessionIds []string `json:"session_ids"`
		GroupId    string   `json:"group_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid json"})
		return
	}
	if len(payload.SessionIds) < 2 {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "at least 2 sessions required"})
		return
	}
	log.Printf("SESSION GROUP LINK request sessions=%v group_id=%s", payload.SessionIds, payload.GroupId)
	controlRevision := newControlRevision()

	// Generate a group ID if not provided
	groupID := payload.GroupId
	if groupID == "" {
		groupID = fmt.Sprintf("G%d", time.Now().Unix()%10000)
	}

	linkedCount := 0
	for _, targetID := range payload.SessionIds {
		update := SessionData{
			"session_id":       targetID,
			"group_id":         groupID,
			"control_revision": controlRevision,
		}
		a.saveSessionByID(targetID, update)
		linkedCount++
	}
	log.Printf("SESSION GROUP LINK result group_id=%s linked=%d", groupID, linkedCount)

	writeJSON(w, map[string]interface{}{
		"message":      "Sessions linked successfully",
		"group_id":     groupID,
		"linked_count": linkedCount,
	})
}

func (a *App) handleUnlinkSession(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		SessionId   string `json:"session_id"`
		GroupId     string `json:"group_id"`
		UnlinkGroup bool   `json:"unlink_group"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid json"})
		return
	}

	sessions := a.getSessionList()
	found := false
	updated := 0
	groupID := payload.GroupId
	if groupID == "" && payload.SessionId != "" {
		for _, session := range sessions {
			if getString(session, "session_id") == payload.SessionId {
				groupID = getString(session, "group_id")
				break
			}
		}
	}
	controlRevision := newControlRevision()
	if payload.UnlinkGroup && groupID != "" {
		for _, session := range sessions {
			if getString(session, "group_id") == groupID {
				sid := getString(session, "session_id")
				a.saveSessionByID(sid, SessionData{
					"session_id":       sid,
					"group_id":         "",
					"control_revision": controlRevision,
				})
				updated++
				found = true
			}
		}
	} else if payload.SessionId != "" {
		a.saveSessionByID(payload.SessionId, SessionData{
			"session_id":       payload.SessionId,
			"group_id":         "",
			"control_revision": controlRevision,
		})
		updated++
		found = true
	}

	if found {
		writeJSON(w, map[string]interface{}{
			"message":        "Session group updated successfully",
			"group_id":       groupID,
			"unlinked_count": updated,
		})
	} else {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]string{"error": "Session not found"})
	}
}

func (a *App) handleGetGroup(w http.ResponseWriter, r *http.Request) {
	groupID := mux.Vars(r)["groupId"]
	if groupID == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "group_id required"})
		return
	}

	groupSessions := a.getSessionsByGroupId(groupID)
	writeJSON(w, map[string]interface{}{
		"group_id": groupID,
		"sessions": groupSessions,
		"count":    len(groupSessions),
	})
}

func (a *App) handleMyShows(w http.ResponseWriter, r *http.Request) {
	url := fmt.Sprintf("http://%s:%s/api/content", a.upstreamHost, a.upstreamPort)
	resp, err := a.client.Get(url)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": "Error fetching content from upstream server"})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": "Error fetching content from upstream server"})
		return
	}
	var items []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": "Error fetching content from upstream server"})
		return
	}
	shows := make([]map[string]string, 0)
	for _, item := range items {
		name := getString(item, "name")
		if name == "" {
			continue
		}
		if !getBool(item, "has_hls") {
			continue
		}
		descriptionParts := []string{fmt.Sprintf("Name: %s", name), fmt.Sprintf("Go-live: /go-live/%s/master.m3u8", name)}
		if segment, ok := item["segment_duration"]; ok {
			descriptionParts = append(descriptionParts, fmt.Sprintf("Segment duration: %v", segment))
		}
		if maxResolution := getString(item, "max_resolution"); maxResolution != "" {
			descriptionParts = append(descriptionParts, fmt.Sprintf("Max resolution: %s", maxResolution))
		}
		if maxHeight := getNumber(item, "max_height"); maxHeight != nil {
			descriptionParts = append(descriptionParts, fmt.Sprintf("Max height: %v", maxHeight))
		}
		shows = append(shows, map[string]string{
			"title":       fmt.Sprintf("/go-live/%s/master.m3u8", name),
			"description": strings.Join(descriptionParts, "\n"),
		})
	}
	writeJSON(w, shows)
}

func (a *App) handleDebug(w http.ResponseWriter, r *http.Request) {
	keys := make([]string, 0, len(r.Header))
	for key := range r.Header {
		keys = append(keys, key)
	}
	writeJSON(w, map[string]interface{}{
		"headers": keys,
		"method":  r.Method,
		"path":    r.URL.Path,
	})
}

func (a *App) handleNftStatus(w http.ResponseWriter, r *http.Request) {
	if runtime.GOOS != "linux" {
		writeJSON(w, map[string]string{"status": "disabled", "message": "Traffic shaping requires Linux (tc/netem)"})
		return
	}
	if a.traffic == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{"status": "disabled", "error": "Manager not initialized"})
		return
	}
	active := a.traffic.IsActive()
	if active {
		writeJSON(w, map[string]string{"status": "active", "message": "TC (traffic control) is running"})
		return
	}
	writeJSON(w, map[string]string{"status": "inactive", "message": "TC is not configured"})
}

func (a *App) handleNftCapabilities(w http.ResponseWriter, r *http.Request) {
	status := "disabled"
	reason := "traffic shaping requires Linux (tc/netem)"
	if runtime.GOOS == "linux" {
		status = "enabled"
		reason = ""
	}
	writeJSON(w, map[string]string{"status": status, "platform": runtime.GOOS, "reason": reason})
}

func (a *App) handleNftPort(w http.ResponseWriter, r *http.Request) {
	if a.traffic == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{"error": "Manager not initialized"})
		return
	}
	portStr := mux.Vars(r)["port"]
	mappedPort, _ := a.portMap.MapExternalPort(portStr)
	port, err := strconv.Atoi(mappedPort)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid port"})
		return
	}
	config, err := a.traffic.GetPortConfig(port)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if config == nil {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]string{"error": "Port not found or error reading config"})
		return
	}
	if pattern, ok := a.getShapePattern(port); ok {
		config["pattern_steps"] = pattern.Steps
		config["pattern_enabled"] = len(pattern.Steps) > 0
		config["pattern_step_runtime"] = pattern.ActiveStep
		config["pattern_rate_runtime_mbps"] = pattern.ActiveRateMbps
		config["pattern_runtime_at"] = pattern.ActiveAt
	} else {
		config["pattern_steps"] = []NftShapeStep{}
		config["pattern_enabled"] = false
	}
	writeJSON(w, config)
}

func sanitizeShapeSteps(steps []NftShapeStep) []NftShapeStep {
	out := make([]NftShapeStep, 0, len(steps))
	for _, step := range steps {
		duration := step.DurationSeconds
		if duration <= 0 {
			duration = 1
		}
		rate := step.RateMbps
		if rate < 0 {
			rate = 0
		}
		out = append(out, NftShapeStep{
			RateMbps:        rate,
			DurationSeconds: math.Round(duration*10) / 10,
			Enabled:         step.Enabled,
		})
	}
	return out
}

func (a *App) getShapePattern(port int) (NftShapePattern, bool) {
	a.shapeMu.Lock()
	defer a.shapeMu.Unlock()
	pattern, ok := a.shapeStates[port]
	if !ok {
		return NftShapePattern{}, false
	}
	copied := NftShapePattern{
		Steps:          append([]NftShapeStep(nil), pattern.Steps...),
		ActiveStep:     pattern.ActiveStep,
		ActiveRateMbps: pattern.ActiveRateMbps,
		ActiveAt:       pattern.ActiveAt,
	}
	return copied, true
}

func (a *App) setShapeRuntimeStep(port int, stepIndex int, rateMbps float64) {
	a.shapeMu.Lock()
	defer a.shapeMu.Unlock()
	pattern, ok := a.shapeStates[port]
	if !ok {
		return
	}
	pattern.ActiveStep = stepIndex
	pattern.ActiveRateMbps = rateMbps
	pattern.ActiveAt = time.Now().UTC().Format(time.RFC3339Nano)
	a.shapeStates[port] = pattern
}

func (a *App) stopShapeLoop(port int) {
	a.shapeMu.Lock()
	cancel, ok := a.shapeLoops[port]
	if ok {
		delete(a.shapeLoops, port)
	}
	delete(a.shapeStates, port)
	a.shapeMu.Unlock()
	if ok && cancel != nil {
		cancel()
	}
}

func (a *App) applyShapePattern(port int, steps []NftShapeStep, delayMs int, loss float64) error {
	if a.traffic == nil {
		return fmt.Errorf("traffic manager not initialized")
	}
	cleanSteps := sanitizeShapeSteps(steps)
	if len(cleanSteps) == 0 {
		a.stopShapeLoop(port)
		a.updateSessionsByPortWithControl(port, map[string]interface{}{
			"nftables_pattern_enabled": false,
			"nftables_pattern_steps":   []NftShapeStep{},
		}, "")
		return nil
	}
	if err := a.traffic.UpdateNetem(port, delayMs, loss); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	var oldCancel context.CancelFunc
	a.shapeMu.Lock()
	oldCancel = a.shapeLoops[port]
	a.shapeLoops[port] = cancel
	a.shapeStates[port] = NftShapePattern{
		Steps:          append([]NftShapeStep(nil), cleanSteps...),
		ActiveStep:     0,
		ActiveRateMbps: 0,
		ActiveAt:       "",
	}
	a.shapeMu.Unlock()
	if oldCancel != nil {
		oldCancel()
	}
	a.updateSessionsByPortWithControl(port, map[string]interface{}{
		"nftables_pattern_enabled": true,
		"nftables_pattern_steps":   cleanSteps,
		"nftables_delay_ms":        delayMs,
		"nftables_packet_loss":     loss,
	}, "")
	go a.runShapePatternLoop(ctx, port, cleanSteps, delayMs, loss)
	return nil
}

func (a *App) runShapePatternLoop(ctx context.Context, port int, steps []NftShapeStep, delayMs int, loss float64) {
	if len(steps) == 0 {
		return
	}
	hasEnabledStep := false
	for _, step := range steps {
		if step.Enabled {
			hasEnabledStep = true
			break
		}
	}
	stepIndex := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		step := steps[stepIndex]
		ts := time.Now().UTC().Format(time.RFC3339Nano)
		if !step.Enabled {
			log.Printf(
				"NETSHAPE pattern_step ts=%s port=%d step=%d/%d rate_mbps=%.3f duration_s=%.1f enabled=%t status=skipped_disabled",
				ts,
				port,
				stepIndex+1,
				len(steps),
				step.RateMbps,
				step.DurationSeconds,
				step.Enabled,
			)
			if !hasEnabledStep {
				timer := time.NewTimer(250 * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			stepIndex = (stepIndex + 1) % len(steps)
			continue
		}
		if err := a.applyShapeIfChanged(port, step.RateMbps, delayMs, loss); err != nil {
			log.Printf(
				"NETSHAPE pattern_step ts=%s port=%d step=%d/%d rate_mbps=%.3f duration_s=%.1f enabled=%t status=rate_failed err=%v",
				ts,
				port,
				stepIndex+1,
				len(steps),
				step.RateMbps,
				step.DurationSeconds,
				step.Enabled,
				err,
			)
		} else {
			a.setShapeRuntimeStep(port, stepIndex+1, step.RateMbps)
			log.Printf(
				"NETSHAPE pattern_step ts=%s port=%d step=%d/%d rate_mbps=%.3f duration_s=%.1f enabled=%t status=applied",
				ts,
				port,
				stepIndex+1,
				len(steps),
				step.RateMbps,
				step.DurationSeconds,
				step.Enabled,
			)
		}
		// netem is applied via applyShapeIfChanged above.
		a.updateSessionsByPort(port, map[string]interface{}{
			"nftables_pattern_enabled":           true,
			"nftables_pattern_steps":             steps,
			"nftables_pattern_step":              stepIndex + 1,
			"nftables_pattern_step_runtime":      stepIndex + 1,
			"nftables_pattern_rate_runtime_mbps": step.RateMbps,
		})
		wait := time.Duration(step.DurationSeconds * float64(time.Second))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		stepIndex = (stepIndex + 1) % len(steps)
	}
}

func (a *App) disablePatternForPort(port int) {
	a.stopShapeLoop(port)
	a.updateSessionsByPortWithControl(port, map[string]interface{}{
		"nftables_pattern_enabled": false,
		"nftables_pattern_steps":   []NftShapeStep{},
		"nftables_pattern_step":    nil,
	}, "")
}

func transportRuleComment(port int) string {
	return fmt.Sprintf("go_proxy_transport_port_%d", port)
}

func runNftScript(script string) ([]byte, error) {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	return cmd.CombinedOutput()
}

func ensureTransportFaultChain() error {
	if out, err := runNftScript(fmt.Sprintf("add table inet %s\n", transportFaultTableName)); err != nil {
		msg := strings.ToLower(string(out))
		if !strings.Contains(msg, "file exists") && !strings.Contains(msg, "exists") {
			return fmt.Errorf("create nft table failed: %s", strings.TrimSpace(string(out)))
		}
	}
	chainScript := fmt.Sprintf(
		"add chain inet %s %s { type filter hook input priority -150; policy accept; }\n",
		transportFaultTableName,
		transportFaultChainName,
	)
	if out, err := runNftScript(chainScript); err != nil {
		msg := strings.ToLower(string(out))
		if !strings.Contains(msg, "file exists") && !strings.Contains(msg, "exists") {
			return fmt.Errorf("create nft chain failed: %s", strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func clearTransportFaultRule(port int) error {
	if err := ensureTransportFaultChain(); err != nil {
		return err
	}
	cmd := exec.Command("nft", "-a", "list", "chain", "inet", transportFaultTableName, transportFaultChainName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.ToLower(string(out))
		if strings.Contains(msg, "no such file") || strings.Contains(msg, "does not exist") {
			return nil
		}
		return fmt.Errorf("list transport fault chain failed: %s", strings.TrimSpace(string(out)))
	}
	comment := transportRuleComment(port)
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, comment) {
			continue
		}
		match := nftHandleRegex.FindStringSubmatch(line)
		if len(match) != 2 {
			continue
		}
		delCmd := exec.Command("nft", "delete", "rule", "inet", transportFaultTableName, transportFaultChainName, "handle", match[1])
		if delOut, delErr := delCmd.CombinedOutput(); delErr != nil {
			msg := strings.ToLower(string(delOut))
			if strings.Contains(msg, "no such file") || strings.Contains(msg, "does not exist") {
				continue
			}
			return fmt.Errorf("delete transport fault rule failed: %s", strings.TrimSpace(string(delOut)))
		}
	}
	return nil
}

func getTransportFaultRuleCounters() map[int]TransportFaultRuleCounters {
	countersByPort := map[int]TransportFaultRuleCounters{}
	cmd := exec.Command("nft", "-a", "list", "chain", "inet", transportFaultTableName, transportFaultChainName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.ToLower(string(out))
		if strings.Contains(msg, "no such file") || strings.Contains(msg, "does not exist") {
			return countersByPort
		}
		return countersByPort
	}

	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "go_proxy_transport_port_") {
			continue
		}
		commentMatch := nftCommentPortRegex.FindStringSubmatch(line)
		if len(commentMatch) != 2 {
			continue
		}
		port, convErr := strconv.Atoi(commentMatch[1])
		if convErr != nil {
			continue
		}
		counterMatch := nftCounterRegex.FindStringSubmatch(line)
		if len(counterMatch) != 3 {
			continue
		}
		packets, packetErr := strconv.ParseInt(counterMatch[1], 10, 64)
		bytesVal, bytesErr := strconv.ParseInt(counterMatch[2], 10, 64)
		if packetErr != nil || bytesErr != nil {
			continue
		}
		entry := countersByPort[port]
		lower := strings.ToLower(line)
		if strings.Contains(lower, " reject") {
			entry.RejectPackets += packets
			entry.RejectBytes += bytesVal
		} else if strings.Contains(lower, " drop") {
			entry.DropPackets += packets
			entry.DropBytes += bytesVal
		}
		countersByPort[port] = entry
	}

	return countersByPort
}

func applyTransportFaultRule(port int, faultType string) error {
	if err := ensureTransportFaultChain(); err != nil {
		return err
	}
	if err := clearTransportFaultRule(port); err != nil {
		return err
	}
	faultType = normalizeTransportFaultType(faultType)
	if faultType == "none" {
		return nil
	}
	ruleAction := "drop"
	if faultType == "reject" {
		ruleAction = "reject with tcp reset"
	}
	script := fmt.Sprintf(
		"add rule inet %s %s tcp dport %d counter %s comment %q\n",
		transportFaultTableName,
		transportFaultChainName,
		port,
		ruleAction,
		transportRuleComment(port),
	)
	if out, err := runNftScript(script); err != nil {
		return fmt.Errorf("add transport fault rule failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func transportFaultConfigFromSession(session SessionData) (string, int, string, int) {
	faultType := normalizeTransportFaultType(getString(session, "transport_failure_type"))
	if faultType == "none" {
		faultType = normalizeTransportFaultType(getString(session, "transport_fault_type"))
	}
	consecutiveUnits := normalizeTransportConsecutiveUnits(getString(session, "transport_consecutive_units"))
	if consecutiveUnits == transportUnitsSeconds {
		consecutiveUnits = normalizeTransportConsecutiveUnits(getString(session, "transport_failure_units"))
	}
	if consecutiveUnits == transportUnitsSeconds {
		consecutiveUnits = transportConsecutiveUnitsFromMode(getString(session, "transport_failure_mode"))
	}
	consecutive := getInt(session, "transport_consecutive_failures")
	if consecutive < 0 {
		consecutive = int(math.Round(floatFromInterface(session["transport_consecutive_seconds"])))
	}
	if consecutive < 0 {
		consecutive = int(math.Round(floatFromInterface(session["transport_fault_on_seconds"])))
	}
	if consecutive < 0 {
		consecutive = 0
	}
	frequency := getInt(session, "transport_failure_frequency")
	if frequency < 0 {
		frequency = int(math.Round(floatFromInterface(session["transport_frequency_seconds"])))
	}
	if frequency < 0 {
		frequency = int(math.Round(floatFromInterface(session["transport_fault_off_seconds"])))
	}
	if frequency < 0 {
		frequency = 0
	}
	return faultType, consecutive, consecutiveUnits, frequency
}

func (a *App) getFirstSessionByPort(port int) SessionData {
	portStr := strconv.Itoa(port)
	for _, session := range a.getSessionList() {
		if getString(session, "x_forwarded_port") == portStr {
			return session
		}
	}
	return nil
}

func (a *App) setTransportFaultSessionState(port int, faultType string, active bool, startedAt string, phaseSeconds float64, cycleSeconds float64) {
	sessions := a.getSessionList()
	changed := false
	controlRevision := ""
	phaseRounded := math.Round(phaseSeconds*1000) / 1000
	cycleRounded := math.Round(cycleSeconds*1000) / 1000
	for _, session := range sessions {
		portStr := getString(session, "x_forwarded_port")
		if portStr == "" {
			continue
		}
		if portNum, err := strconv.Atoi(portStr); err == nil && portNum == port {
			prevType := getString(session, "transport_fault_type")
			if prevType == "" {
				prevType = getString(session, "transport_failure_type")
			}
			prevActive := getBool(session, "transport_fault_active")
			prevStarted := getString(session, "transport_fault_started_at")
			session["transport_failure_type"] = faultType
			session["transport_fault_type"] = faultType
			session["transport_fault_active"] = active
			session["transport_fault_started_at"] = startedAt
			session["transport_fault_phase_seconds"] = phaseRounded
			session["transport_fault_cycle_seconds"] = cycleRounded
			controlChanged := prevType != faultType || prevActive != active
			if !controlChanged && startedAt != "" && prevStarted != startedAt {
				controlChanged = true
			}
			if controlChanged {
				if controlRevision == "" {
					controlRevision = newControlRevision()
				}
				applyControlRevision(session, controlRevision)
			}
			changed = true
		}
	}
	if changed {
		a.saveSessionList(sessions)
	}
}

func (a *App) resetTransportFaultCounters(port int) {
	a.updateSessionsByPort(port, map[string]interface{}{
		"transport_fault_drop_packets":   int64(0),
		"transport_fault_reject_packets": int64(0),
	})
}

func (a *App) stopTransportFaultLoop(port int) {
	a.faultMu.Lock()
	cancel, ok := a.faultLoops[port]
	if ok {
		delete(a.faultLoops, port)
	}
	a.faultMu.Unlock()
	if ok && cancel != nil {
		cancel()
	}
}

func (a *App) armTransportFaultLoop(port int, faultType string, consecutiveThreshold int, consecutiveUnits string, frequencySeconds int) {
	if consecutiveThreshold < 0 {
		consecutiveThreshold = 0
	}
	if frequencySeconds < 0 {
		frequencySeconds = 0
	}
	consecutiveUnits = normalizeTransportConsecutiveUnits(consecutiveUnits)
	faultType = normalizeTransportFaultType(faultType)
	a.stopTransportFaultLoop(port)
	if err := clearTransportFaultRule(port); err != nil {
		log.Printf("FAULT transport_cleanup_failed port=%d err=%v", port, err)
	}
	if faultType == "none" {
		a.setTransportFaultSessionState(port, "none", false, "", 0, 0)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.faultMu.Lock()
	a.faultLoops[port] = cancel
	a.faultMu.Unlock()
	go a.runTransportFaultLoop(ctx, port, faultType, consecutiveThreshold, consecutiveUnits, frequencySeconds)
}

func (a *App) runTransportFaultLoop(ctx context.Context, port int, faultType string, consecutiveThreshold int, consecutiveUnits string, frequencySeconds int) {
	defer func() {
		a.faultMu.Lock()
		if cancel, ok := a.faultLoops[port]; ok && cancel != nil {
			delete(a.faultLoops, port)
		}
		a.faultMu.Unlock()
		_ = clearTransportFaultRule(port)
	}()

	cycleSeconds := float64(frequencySeconds)
	if consecutiveUnits == transportUnitsSeconds {
		cycleSeconds = float64(consecutiveThreshold + frequencySeconds)
	}
	if consecutiveThreshold <= 0 {
		cycleSeconds = 0
	}
	if cycleSeconds < 0 {
		cycleSeconds = 0
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		start := time.Now().UTC()
		a.resetTransportFaultCounters(port)
		if err := applyTransportFaultRule(port, faultType); err != nil {
			log.Printf("FAULT transport_arm_failed ts=%s port=%d fault_type=transport_%s err=%v", start.Format(time.RFC3339Nano), port, faultType, err)
		}
		a.setTransportFaultSessionState(port, faultType, true, start.Format(time.RFC3339Nano), 0, cycleSeconds)
		if session := a.getFirstSessionByPort(port); session != nil {
			bumpFaultCounter(session, "transport_"+faultType)
			logFaultEvent(session, strconv.Itoa(port), "transport_"+faultType, "control", "transport_arm")
			counterKey := faultCounterKey("transport_" + faultType)
			if counterKey != "" {
				a.updateSessionsByPort(port, map[string]interface{}{
					counterKey:          session[counterKey],
					"fault_count_total": session["fault_count_total"],
				})
			}
		}
		if consecutiveThreshold <= 0 {
			<-ctx.Done()
			return
		}

		if consecutiveUnits == transportUnitsPackets {
			ticker := time.NewTicker(100 * time.Millisecond)
			reachedThreshold := false
			for !reachedThreshold {
				select {
				case <-ctx.Done():
					ticker.Stop()
					return
				case now := <-ticker.C:
					matchedPackets := int64(0)
					if counters, ok := getTransportFaultRuleCounters()[port]; ok {
						if faultType == "reject" {
							matchedPackets = counters.RejectPackets
						} else {
							matchedPackets = counters.DropPackets
						}
					}
					phaseSeconds := now.Sub(start).Seconds()
					a.setTransportFaultSessionState(port, faultType, true, start.Format(time.RFC3339Nano), phaseSeconds, cycleSeconds)
					if matchedPackets >= int64(consecutiveThreshold) {
						reachedThreshold = true
					}
				}
			}
			ticker.Stop()
		} else {
			timer := time.NewTimer(time.Duration(consecutiveThreshold) * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}

		_ = clearTransportFaultRule(port)
		if frequencySeconds <= 0 {
			a.setTransportFaultSessionState(port, "none", false, "", 0, cycleSeconds)
			if session := a.getFirstSessionByPort(port); session != nil {
				logFaultEvent(session, strconv.Itoa(port), "transport_none", "control", "transport_disarm_auto")
			}
			return
		}

		a.setTransportFaultSessionState(port, faultType, false, "", 0, cycleSeconds)
		if session := a.getFirstSessionByPort(port); session != nil {
			logFaultEvent(session, strconv.Itoa(port), "transport_none", "control", "transport_disarm_cycle")
		}

		pause := time.NewTimer(time.Duration(frequencySeconds) * time.Second)
		select {
		case <-ctx.Done():
			pause.Stop()
			return
		case <-pause.C:
		}
	}
}

func (a *App) restoreTransportFaultSchedules() {
	seenPorts := map[int]struct{}{}
	for _, session := range a.getSessionList() {
		portStr := getString(session, "x_forwarded_port")
		if portStr == "" {
			continue
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			continue
		}
		if _, ok := seenPorts[port]; ok {
			continue
		}
		seenPorts[port] = struct{}{}
		faultType, consecutive, consecutiveUnits, frequency := transportFaultConfigFromSession(session)
		a.armTransportFaultLoop(port, faultType, consecutive, consecutiveUnits, frequency)
	}
}

func (a *App) handleNftPattern(w http.ResponseWriter, r *http.Request) {
	if a.traffic == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{"error": "Manager not initialized"})
		return
	}
	portStr := mux.Vars(r)["port"]
	mappedPort, _ := a.portMap.MapExternalPort(portStr)
	log.Printf("NETSHAPE request kind=pattern path=%s port_param=%s mapped_port=%s x_forwarded_port=%s", r.URL.Path, portStr, mappedPort, r.Header.Get("X-Forwarded-Port"))
	port, err := strconv.Atoi(mappedPort)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid port"})
		return
	}
	var payload struct {
		Steps                  []NftShapeStep `json:"steps"`
		DelayMs                int            `json:"delay_ms"`
		LossPct                float64        `json:"loss_pct"`
		SegmentDurationSeconds float64        `json:"segment_duration_seconds"`
		DefaultSegments        float64        `json:"default_segments"`
		DefaultStepSeconds     float64        `json:"default_step_seconds"`
		TemplateMode           string         `json:"template_mode"`
		TemplateMarginPct      float64        `json:"template_margin_pct"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid json"})
		return
	}
	switch payload.TemplateMode {
	case "sliders", "square_wave", "ramp_up", "ramp_down", "pyramid":
	default:
		payload.TemplateMode = "sliders"
	}
	switch payload.TemplateMarginPct {
	case 0, 10, 25, 50:
	default:
		payload.TemplateMarginPct = 0
	}
	if payload.DefaultStepSeconds <= 0 {
		payload.DefaultStepSeconds = payload.DefaultSegments * payload.SegmentDurationSeconds
	}
	if len(payload.Steps) == 0 {
		a.disablePatternForPort(port)
		a.updateSessionsByPortWithControl(port, map[string]interface{}{
			"nftables_pattern_segment_duration_seconds": payload.SegmentDurationSeconds,
			"nftables_pattern_default_segments":         payload.DefaultSegments,
			"nftables_pattern_default_step_seconds":     payload.DefaultStepSeconds,
			"nftables_pattern_template_mode":            payload.TemplateMode,
			"nftables_pattern_margin_pct":               payload.TemplateMarginPct,
		}, "")
		writeJSON(w, map[string]interface{}{
			"success":         true,
			"port":            port,
			"pattern_enabled": false,
			"steps":           []NftShapeStep{},
		})
		return
	}
	cleanSteps := sanitizeShapeSteps(payload.Steps)
	if err := a.applyShapePattern(port, cleanSteps, payload.DelayMs, payload.LossPct); err != nil {
		log.Printf("NETSHAPE pattern apply failed port=%d: %v", port, err)
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": "Failed to apply pattern", "details": err.Error()})
		return
	}
	a.updateSessionsByPortWithControl(port, map[string]interface{}{
		"nftables_pattern_segment_duration_seconds": payload.SegmentDurationSeconds,
		"nftables_pattern_default_segments":         payload.DefaultSegments,
		"nftables_pattern_default_step_seconds":     payload.DefaultStepSeconds,
		"nftables_pattern_template_mode":            payload.TemplateMode,
		"nftables_pattern_margin_pct":               payload.TemplateMarginPct,
	}, "")

	// Propagate to group members
	snap := a.getSessionList()
	groupID := a.getGroupIdByPort(port, snap)
	if groupID != "" {
		groupPorts := a.getPortsForGroup(groupID, snap)
		for _, groupPort := range groupPorts {
			if groupPort == port {
				continue // Skip the original port
			}
			if err := a.applyShapePattern(groupPort, cleanSteps, payload.DelayMs, payload.LossPct); err != nil {
				log.Printf("NETSHAPE group pattern propagation failed port=%d: %v", groupPort, err)
				continue
			}
			a.updateSessionsByPortWithControl(groupPort, map[string]interface{}{
				"nftables_pattern_segment_duration_seconds": payload.SegmentDurationSeconds,
				"nftables_pattern_default_segments":         payload.DefaultSegments,
				"nftables_pattern_default_step_seconds":     payload.DefaultStepSeconds,
				"nftables_pattern_template_mode":            payload.TemplateMode,
				"nftables_pattern_margin_pct":               payload.TemplateMarginPct,
			}, "")
			log.Printf("NETSHAPE group pattern propagation applied port=%d group=%s", groupPort, groupID)
		}
	}

	writeJSON(w, map[string]interface{}{
		"success":         true,
		"port":            port,
		"pattern_enabled": true,
		"steps":           cleanSteps,
		"delay_ms":        payload.DelayMs,
		"loss_pct":        payload.LossPct,
	})
}

func (a *App) handleNftBandwidth(w http.ResponseWriter, r *http.Request) {
	if a.traffic == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{"error": "Manager not initialized"})
		return
	}
	portStr := mux.Vars(r)["port"]
	mappedPort, _ := a.portMap.MapExternalPort(portStr)
	log.Printf("NETSHAPE request kind=bandwidth path=%s port_param=%s mapped_port=%s x_forwarded_port=%s", r.URL.Path, portStr, mappedPort, r.Header.Get("X-Forwarded-Port"))
	port, err := strconv.Atoi(mappedPort)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid port"})
		return
	}
	var payload map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid json"})
		return
	}
	rate := "10"
	if val, ok := payload["rate"]; ok {
		switch v := val.(type) {
		case string:
			rate = v
		case float64:
			rate = fmt.Sprintf("%g", v)
		case int:
			rate = fmt.Sprintf("%d", v)
		}
	}
	rate = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(rate, "mbps", ""), "mbit", ""))
	rate = strings.TrimSpace(rate)
	rateMbps, err := strconv.ParseFloat(rate, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid rate"})
		return
	}
	a.disablePatternForPort(port)
	if err := a.traffic.UpdateRateLimit(port, rateMbps); err != nil {
		log.Printf("NETSHAPE rate limit failed port=%d rate=%g: %v", port, rateMbps, err)
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": "Failed to update rate limit", "details": err.Error()})
		return
	}
	a.updateSessionsByPortWithControl(port, map[string]interface{}{
		"nftables_bandwidth_mbps": rateMbps,
	}, "")
	writeJSON(w, map[string]interface{}{"success": true, "port": port, "rate": fmt.Sprintf("%g Mbps", rateMbps)})
}

func (a *App) handleNftLoss(w http.ResponseWriter, r *http.Request) {
	if a.traffic == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{"error": "Manager not initialized"})
		return
	}
	portStr := mux.Vars(r)["port"]
	mappedPort, _ := a.portMap.MapExternalPort(portStr)
	log.Printf("NETSHAPE request kind=loss path=%s port_param=%s mapped_port=%s x_forwarded_port=%s", r.URL.Path, portStr, mappedPort, r.Header.Get("X-Forwarded-Port"))
	port, err := strconv.Atoi(mappedPort)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid port"})
		return
	}
	var payload map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid json"})
		return
	}
	loss := 0.0
	if val, ok := payload["loss_pct"]; ok {
		switch v := val.(type) {
		case float64:
			loss = v
		case int:
			loss = float64(v)
		case string:
			parsed, _ := strconv.ParseFloat(v, 64)
			loss = parsed
		}
	}
	a.disablePatternForPort(port)
	if err := a.traffic.UpdateNetem(port, 0, loss); err != nil {
		log.Printf("NETSHAPE packet loss failed port=%d loss=%.2f: %v", port, loss, err)
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": "Failed to update packet loss", "details": err.Error()})
		return
	}
	a.updateSessionsByPortWithControl(port, map[string]interface{}{
		"nftables_packet_loss": loss,
	}, "")
	writeJSON(w, map[string]interface{}{"success": true, "port": port, "loss_pct": loss})
}

func (a *App) handleNftShape(w http.ResponseWriter, r *http.Request) {
	if a.traffic == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{"error": "Manager not initialized"})
		return
	}
	portStr := mux.Vars(r)["port"]
	mappedPort, _ := a.portMap.MapExternalPort(portStr)
	log.Printf("NETSHAPE request kind=shape path=%s port_param=%s mapped_port=%s x_forwarded_port=%s", r.URL.Path, portStr, mappedPort, r.Header.Get("X-Forwarded-Port"))
	port, err := strconv.Atoi(mappedPort)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid port"})
		return
	}
	var payload map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid json"})
		return
	}
	rateMbps := 0.0
	if val, ok := payload["rate_mbps"]; ok {
		switch v := val.(type) {
		case float64:
			rateMbps = v
		case int:
			rateMbps = float64(v)
		case string:
			parsed, _ := strconv.ParseFloat(v, 64)
			rateMbps = parsed
		}
	}
	delayMs := 0
	if val, ok := payload["delay_ms"]; ok {
		switch v := val.(type) {
		case float64:
			delayMs = int(v)
		case int:
			delayMs = v
		case string:
			parsed, _ := strconv.Atoi(v)
			delayMs = parsed
		}
	}
	loss := 0.0
	if val, ok := payload["loss_pct"]; ok {
		switch v := val.(type) {
		case float64:
			loss = v
		case int:
			loss = float64(v)
		case string:
			parsed, _ := strconv.ParseFloat(v, 64)
			loss = parsed
		}
	}
	a.disablePatternForPort(port)
	if err := a.traffic.UpdateRateLimit(port, rateMbps); err != nil {
		log.Printf("NETSHAPE rate limit failed port=%d rate=%g: %v", port, rateMbps, err)
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": "Failed to update rate limit", "details": err.Error()})
		return
	}
	if err := a.traffic.UpdateNetem(port, delayMs, loss); err != nil {
		log.Printf("NETSHAPE netem failed port=%d delay=%d loss=%.2f: %v", port, delayMs, loss, err)
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": "Failed to update delay/loss", "details": err.Error()})
		return
	}
	a.updateSessionsByPortWithControl(port, map[string]interface{}{
		"nftables_bandwidth_mbps": rateMbps,
		"nftables_delay_ms":       delayMs,
		"nftables_packet_loss":    loss,
	}, "")

	// Propagate to group members
	snap2 := a.getSessionList()
	groupID := a.getGroupIdByPort(port, snap2)
	if groupID != "" {
		groupPorts := a.getPortsForGroup(groupID, snap2)
		for _, groupPort := range groupPorts {
			if groupPort == port {
				continue // Skip the original port
			}
			a.disablePatternForPort(groupPort)
			if err := a.traffic.UpdateRateLimit(groupPort, rateMbps); err != nil {
				log.Printf("NETSHAPE group propagation rate limit failed port=%d rate=%g: %v", groupPort, rateMbps, err)
				continue
			}
			if err := a.traffic.UpdateNetem(groupPort, delayMs, loss); err != nil {
				log.Printf("NETSHAPE group propagation netem failed port=%d delay=%d loss=%.2f: %v", groupPort, delayMs, loss, err)
				continue
			}
			a.updateSessionsByPortWithControl(groupPort, map[string]interface{}{
				"nftables_bandwidth_mbps": rateMbps,
				"nftables_delay_ms":       delayMs,
				"nftables_packet_loss":    loss,
			}, "")
			log.Printf("NETSHAPE group propagation applied port=%d rate=%g delay=%d loss=%.2f group=%s", groupPort, rateMbps, delayMs, loss, groupID)
		}
	}

	log.Printf("NETSHAPE applied port=%d rate=%g delay=%d loss=%.2f", port, rateMbps, delayMs, loss)
	writeJSON(w, map[string]interface{}{
		"success":   true,
		"port":      port,
		"rate_mbps": rateMbps,
		"delay_ms":  delayMs,
		"loss_pct":  loss,
	})
}

func (a *App) handleCloseSocket(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("Hijack not supported"))
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("Failed to close socket"))
		return
	}
	_ = conn.Close()
}

func (a *App) handleTerminateWorker(w http.ResponseWriter, r *http.Request) {
	go func() {
		time.Sleep(100 * time.Millisecond)
		os.Exit(0)
	}()
	_, _ = w.Write([]byte("Terminating worker"))
}

func (a *App) handleForceClose(w http.ResponseWriter, r *http.Request) {
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = exec.Command("kill", "-9", fmt.Sprintf("%d", os.Getpid())).Run()
		os.Exit(137)
	}()
	_, _ = w.Write([]byte("Force closing"))
}

func requestKindLabel(isSegment, isUpdateManifest, isMasterManifest bool) string {
	if isSegment {
		return "segment"
	}
	if isMasterManifest {
		return "master_manifest"
	}
	if isUpdateManifest {
		return "manifest"
	}
	return "other"
}

func logFaultEvent(session SessionData, port, faultType, requestKind, actionTaken string) {
	if strings.HasPrefix(faultType, "transport_") {
		consecutive := float64(getInt(session, "transport_consecutive_failures"))
		if consecutive <= 0 {
			consecutive = floatFromInterface(session["transport_consecutive_seconds"])
		}
		consecutiveUnits := normalizeTransportConsecutiveUnits(getString(session, "transport_consecutive_units"))
		if consecutiveUnits == transportUnitsSeconds {
			consecutiveUnits = normalizeTransportConsecutiveUnits(getString(session, "transport_failure_units"))
		}
		if consecutiveUnits == transportUnitsSeconds {
			consecutiveUnits = transportConsecutiveUnitsFromMode(getString(session, "transport_failure_mode"))
		}
		frequency := float64(getInt(session, "transport_failure_frequency"))
		if frequency < 0 {
			frequency = floatFromInterface(session["transport_frequency_seconds"])
		}
		log.Printf(
			"FAULT ts=%s session_id=%s port=%s fault_type=%s request_kind=%s action_taken=%s transport_consecutive=%.3f transport_consecutive_units=%s transport_frequency_s=%.3f transport_active=%t transport_phase_s=%.3f transport_cycle_s=%.3f",
			time.Now().UTC().Format(time.RFC3339Nano),
			getString(session, "session_id"),
			port,
			faultType,
			requestKind,
			actionTaken,
			consecutive,
			consecutiveUnits,
			frequency,
			getBool(session, "transport_fault_active"),
			floatFromInterface(session["transport_fault_phase_seconds"]),
			floatFromInterface(session["transport_fault_cycle_seconds"]),
		)
		return
	}
	log.Printf(
		"FAULT ts=%s session_id=%s port=%s fault_type=%s request_kind=%s action_taken=%s",
		time.Now().UTC().Format(time.RFC3339Nano),
		getString(session, "session_id"),
		port,
		faultType,
		requestKind,
		actionTaken,
	)
}

func faultCounterKey(faultType string) string {
	faultType = strings.TrimSpace(strings.ToLower(faultType))
	if faultType == "" || faultType == "none" {
		return ""
	}
	return "fault_count_" + strings.ReplaceAll(faultType, "-", "_")
}

func bumpFaultCounter(session SessionData, faultType string) {
	key := faultCounterKey(faultType)
	if key == "" {
		return
	}
	// Same lock as the fault-decision path: concurrent goroutines
	// incrementing the same counter on the same session map would
	// otherwise lose updates.
	sessionStateMu.Lock()
	defer sessionStateMu.Unlock()
	session[key] = getInt(session, key) + 1
	session["fault_count_total"] = getInt(session, "fault_count_total") + 1
}

func closeSocketAsReject(conn net.Conn) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetLinger(0)
	}
	_ = conn.Close()
}

func closeSocketAsDrop(conn net.Conn) {
	closeSocketAsDropAfter(conn, socketHangDuration)
}

func closeSocketAsDropAfter(conn net.Conn, delay time.Duration) {
	if delay < 0 {
		delay = 0
	}
	go func(c net.Conn) {
		time.Sleep(delay)
		_ = c.Close()
	}(conn)
}

func writeChunkedHeaders(conn net.Conn, contentType string) error {
	if strings.TrimSpace(contentType) == "" {
		contentType = "application/octet-stream"
	}
	header := fmt.Sprintf(
		"HTTP/1.1 200 OK\r\nContent-Type: %s\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n",
		contentType,
	)
	_, err := conn.Write([]byte(header))
	return err
}

func writeChunkedBodyBytes(conn net.Conn, body []byte) error {
	if len(body) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(conn, "%x\r\n", len(body)); err != nil {
		return err
	}
	if _, err := conn.Write(body); err != nil {
		return err
	}
	_, err := conn.Write([]byte("\r\n"))
	return err
}

func isSocketFaultType(faultType string) bool {
	switch faultType {
	case "request_connect_hang",
		"request_connect_reset",
		"request_connect_delayed",
		"request_first_byte_hang",
		"request_first_byte_reset",
		"request_first_byte_delayed",
		"request_body_hang",
		"request_body_reset",
		"request_body_delayed":
		return true
	default:
		return false
	}
}

func applySocketFault(w http.ResponseWriter, faultType, contentType string) (string, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return "", fmt.Errorf("hijack unsupported")
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return "", err
	}
	midBody := bytes.Repeat([]byte("X"), socketMidBodyBytes)
	switch faultType {
	case "request_connect_reset":
		closeSocketAsReject(conn)
		return "request_connect_reset", nil
	case "request_connect_hang":
		closeSocketAsDrop(conn)
		return "request_connect_hang", nil
	case "request_connect_delayed":
		closeSocketAsDropAfter(conn, socketDelayDuration)
		return "request_connect_delayed", nil
	case "request_first_byte_reset":
		if err := writeChunkedHeaders(conn, contentType); err != nil {
			_ = conn.Close()
			return "", err
		}
		closeSocketAsReject(conn)
		return "request_first_byte_reset", nil
	case "request_first_byte_hang":
		if err := writeChunkedHeaders(conn, contentType); err != nil {
			_ = conn.Close()
			return "", err
		}
		closeSocketAsDrop(conn)
		return "request_first_byte_hang", nil
	case "request_first_byte_delayed":
		if err := writeChunkedHeaders(conn, contentType); err != nil {
			_ = conn.Close()
			return "", err
		}
		closeSocketAsDropAfter(conn, socketDelayDuration)
		return "request_first_byte_delayed", nil
	case "request_body_reset":
		if err := writeChunkedHeaders(conn, contentType); err != nil {
			_ = conn.Close()
			return "", err
		}
		if err := writeChunkedBodyBytes(conn, midBody); err != nil {
			_ = conn.Close()
			return "", err
		}
		closeSocketAsReject(conn)
		return "request_body_reset", nil
	case "request_body_hang":
		if err := writeChunkedHeaders(conn, contentType); err != nil {
			_ = conn.Close()
			return "", err
		}
		if err := writeChunkedBodyBytes(conn, midBody); err != nil {
			_ = conn.Close()
			return "", err
		}
		closeSocketAsDrop(conn)
		return "request_body_hang", nil
	case "request_body_delayed":
		if err := writeChunkedHeaders(conn, contentType); err != nil {
			_ = conn.Close()
			return "", err
		}
		if err := writeChunkedBodyBytes(conn, midBody); err != nil {
			_ = conn.Close()
			return "", err
		}
		closeSocketAsDropAfter(conn, socketDelayDuration)
		return "request_body_delayed", nil
	default:
		_ = conn.Close()
		return "", fmt.Errorf("unsupported socket fault type: %s", faultType)
	}
}

func normalizeTransportFaultType(raw string) string {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "drop":
		return "drop"
	case "reject":
		return "reject"
	default:
		return "none"
	}
}

func normalizeRequestFailureType(raw string) string {
	failureType := strings.TrimSpace(strings.ToLower(raw))
	switch failureType {
	case "hung", "socket_timeout":
		return "request_connect_hang"
	case "socket_drop":
		return "request_connect_hang"
	case "socket_reject":
		return "request_connect_reset"
	case "socket_drop_before_headers", "request_connect_hang":
		return "request_connect_hang"
	case "socket_reject_before_headers", "request_connect_reset":
		return "request_connect_reset"
	case "request_connect_delayed":
		return "request_connect_delayed"
	case "socket_drop_after_headers", "request_first_byte_hang":
		return "request_first_byte_hang"
	case "socket_reject_after_headers", "request_first_byte_reset":
		return "request_first_byte_reset"
	case "request_first_byte_delayed":
		return "request_first_byte_delayed"
	case "socket_drop_mid_body", "request_body_hang":
		return "request_body_hang"
	case "socket_reject_mid_body", "request_body_reset":
		return "request_body_reset"
	case "request_body_delayed":
		return "request_body_delayed"
	default:
		return failureType
	}
}

func normalizeTransportConsecutiveUnits(raw string) string {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "packet", "packets", "pkt", "pkts":
		return transportUnitsPackets
	default:
		return transportUnitsSeconds
	}
}

func transportConsecutiveUnitsFromMode(raw string) string {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "failures_per_packets", "failures_per_packet":
		return transportUnitsPackets
	default:
		return transportUnitsSeconds
	}
}

func transportModeFromConsecutiveUnits(units string) string {
	if normalizeTransportConsecutiveUnits(units) == transportUnitsPackets {
		return "failures_per_packets"
	}
	return "failures_per_seconds"
}

// transferTimeoutsFor returns the active and idle transfer-timeout durations
// configured for this session, gated by the per-request-kind apply flags.
// Returns 0 for either value when disabled or when the kind isn't selected.
func transferTimeoutsFor(session SessionData, isSegment, isManifest, isMasterManifest bool) (active, idle time.Duration) {
	var applies bool
	switch {
	case isMasterManifest:
		applies = getBool(session, "transfer_timeout_applies_master")
	case isManifest:
		applies = getBool(session, "transfer_timeout_applies_manifests")
	case isSegment:
		applies = getBool(session, "transfer_timeout_applies_segments")
	}
	if !applies {
		return 0, 0
	}
	if v := getInt(session, "transfer_active_timeout_seconds"); v > 0 {
		active = time.Duration(v) * time.Second
	}
	if v := getInt(session, "transfer_idle_timeout_seconds"); v > 0 {
		idle = time.Duration(v) * time.Second
	}
	return
}

// idleWriter wraps the downstream io.Writer (proxy → client). If no
// successful Write completes for the configured idle window, it cancels
// the request context (which closes the connection mid-transfer) and
// records that the idle timer fired. This catches a stalled client that
// has stopped draining bytes — TCP back-pressure surfaces here.
type idleWriter struct {
	w        io.Writer
	cancel   context.CancelFunc
	timeout  time.Duration
	timer    *time.Timer
	timedOut atomic.Bool
}

func newIdleWriter(w io.Writer, timeout time.Duration, cancel context.CancelFunc) *idleWriter {
	iw := &idleWriter{w: w, cancel: cancel, timeout: timeout}
	iw.timer = time.AfterFunc(timeout, func() {
		iw.timedOut.Store(true)
		cancel()
	})
	return iw
}

func (iw *idleWriter) Write(p []byte) (int, error) {
	n, err := iw.w.Write(p)
	if n > 0 {
		iw.timer.Reset(iw.timeout)
	}
	return n, err
}

func (iw *idleWriter) Stop() {
	iw.timer.Stop()
}

func (a *App) handleProxy(w http.ResponseWriter, r *http.Request) {
	// Anchor the player's perceived timeline at the moment we received
	// their request. ClientWaitMs (wait perceived by the player) is
	// computed against this on every path, including faults.
	requestReceivedAt := time.Now()
	// playerURL is the URL the player asked for — used as the primary
	// `URL` field on every NetworkLogEntry so HAR entries reflect what
	// the player did, not the proxy → origin URL.
	playerURL := r.URL.String()
	// Snapshot the player's request headers + query string once for HAR
	// capture (issue #279). Sensitive headers are filtered inside
	// capturedHeaders. The parsed query is preserved in original URL
	// order via capturedQueryString.
	requestHeaders := capturedHeaders(r.Header)
	queryString := capturedQueryString(r.URL)
	// Extract the player's `play_id` query param (issue #280). Used to
	// scope HAR snapshots to a single playback episode. Stamped onto
	// every NetworkLogEntry created in this handler via the logEntry
	// closure below.
	playID := strings.TrimSpace(r.URL.Query().Get("play_id"))
	logEntry := func(sessionID string, entry NetworkLogEntry) {
		if entry.PlayID == "" {
			entry.PlayID = playID
		}
		a.addNetworkLogEntry(sessionID, entry)
	}
	filename := strings.TrimPrefix(r.URL.Path, "/")
	escapedPath := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	if filename == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	externalPort := r.Header.Get("X-Forwarded-Port")
	if externalPort == "" {
		externalPort = hostPortOrDefault(r.Host, "30181")
	}
	internalPort := externalPort
	if mapped, ok := a.portMap.MapExternalPort(externalPort); ok {
		internalPort = mapped
	} else {
		// No explicit port mapping — derive internal port from known listening ports (30081-30881)
		sessionNum := thirdFromLastDigit(externalPort)
		if n, err := strconv.Atoi(sessionNum); err == nil && n >= 0 && n <= 8 {
			internalPort = replaceThirdFromLastDigit("30081", n)
		}
	}
	log.Printf("Original URL: %s", r.URL.String())
	log.Printf("Original Host: %s", r.Host)
	log.Printf("X-Forwarded-Port: %s", r.Header.Get("X-Forwarded-Port"))

	a.removeInactiveSessions()
	sessionList := a.getSessionList()
	sessionNumber := thirdFromLastDigit(externalPort)
	playerID := r.URL.Query().Get("player_id")
	playerHeader := r.Header.Get("player_id")
	playerHeaderAlt := r.Header.Get("Player-ID")
	playbackSessionHeader := r.Header.Get("X-Playback-Session-Id")
	requesterIP := extractClientIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"))

	if playerID != "" && sessionNumber == "0" {
		if existing := findSessionByPlayerID(sessionList, playerID, playerHeader, playerHeaderAlt, playbackSessionHeader); existing != nil {
			assigned := getString(existing, "session_number")
			if assigned == "" {
				assigned = getString(existing, "session_id")
			}
			if assigned != "" {
				assignedNum, _ := strconv.Atoi(assigned)
				newPort := replaceThirdFromLastDigit(externalPort, assignedNum)
				host := hostWithoutPort(r.Host)
				newURL := fmt.Sprintf("http://%s:%s/%s", host, newPort, escapedPath)
				if r.URL.RawQuery != "" {
					newURL = newURL + "?" + r.URL.RawQuery
				}
				log.Printf("Redirecting to existing session URL: %s %s -> %s", newURL, externalPort, newPort)
				http.Redirect(w, r, newURL, http.StatusFound)
				return
			}
		}
		if isExternalIP(requesterIP) {
			activeForRequester := countActiveSessionsForIP(sessionList, requesterIP)
			if activeForRequester >= externalWANSessionLimit {
				w.WriteHeader(http.StatusTooManyRequests)
				writeJSON(w, map[string]interface{}{
					"error":                  "external session limit reached",
					"limit":                  externalWANSessionLimit,
					"requester_ip":           requesterIP,
					"active_sessions_for_ip": activeForRequester,
				})
				return
			}
		}
		if len(sessionList) >= a.maxSessions {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		createdAt := nowISO()
		allocated := allocateSessionNumber(sessionList, a.maxSessions)
		assignedExternalPort := replaceThirdFromLastDigit(externalPort, allocated)
		assignedInternalPort := assignedExternalPort
		// Sweep any leftover tc rate-limit / filter on the assigned
		// internal port — closes the leak in #352 where a previous
		// session's rule survived a proxy restart and silently capped
		// the new session's bandwidth. Idempotent and quiet on a
		// clean port; logs only when it actually finds something
		// to clear.
		if a.traffic != nil {
			if internalPortInt, err := strconv.Atoi(assignedInternalPort); err == nil {
				a.traffic.ClearPortShaping(internalPortInt)
			}
		}
		if mapped, ok := a.portMap.MapExternalPort(assignedExternalPort); ok {
			assignedInternalPort = mapped
		} else {
			// No explicit port mapping — derive internal port from known listening ports (30081-30881)
			assignedInternalPort = replaceThirdFromLastDigit("30081", allocated)
			log.Printf("PORT_MAP_DERIVE external=%s internal=%s allocated=%d", assignedExternalPort, assignedInternalPort, allocated)
		}
		groupID := extractGroupId(playerID)
		sessionData := SessionData{
			"session_number":                           fmt.Sprintf("%d", allocated),
			"sid":                                      fmt.Sprintf("%d", allocated),
			"session_id":                               fmt.Sprintf("%d", allocated),
			"player_id":                                playerID,
			"group_id":                                 groupID,
			"control_revision":                         newControlRevision(),
			"headers_player_id":                        playerHeader,
			"headers_player-ID":                        playerHeaderAlt,
			"headers_x_playback_session_id":            playbackSessionHeader,
			"manifest_requests_count":                  0,
			"master_manifest_requests_count":           0,
			"segments_count":                           0,
			"all_requests_count":                       0,
			"last_request":                             createdAt,
			"first_request_time":                       createdAt,
			"session_start_time":                       createdAt,
			// Segment / manifest / master_manifest fault config —
			// initialise mode + units explicitly so both server
			// (NewFailureHandler) and dashboard (Mode dropdown) read
			// the same value from a single source of truth instead
			// of falling back to duplicated hard-coded defaults.
			// "failures_per_seconds" mode → consecutive=requests,
			// frequency=seconds, matching the dashboard's visible
			// default Mode for a fresh session.
			"segment_failure_type":                     "none",
			"segment_failure_frequency":                0,
			"segment_consecutive_failures":             0,
			"segment_failure_units":                    "requests",
			"segment_consecutive_units":                "requests",
			"segment_frequency_units":                  "seconds",
			"segment_failure_mode":                     "failures_per_seconds",
			"manifest_failure_type":                    "none",
			"manifest_failure_frequency":               0,
			"manifest_failure_units":                   "requests",
			"manifest_consecutive_units":               "requests",
			"manifest_frequency_units":                 "seconds",
			"manifest_failure_mode":                    "failures_per_seconds",
			"manifest_consecutive_failures":            0,
			"master_manifest_failure_type":             "none",
			"master_manifest_failure_frequency":        0,
			"master_manifest_failure_units":            "requests",
			"master_manifest_consecutive_units":        "requests",
			"master_manifest_frequency_units":          "seconds",
			"master_manifest_failure_mode":             "failures_per_seconds",
			"master_manifest_consecutive_failures":     0,
			// "All" fault override — when all_failure_type != "none",
			// HandleRequest uses this rule for every HTTP request and
			// ignores the per-kind tabs above. Same control shape as
			// segment, plus all_failure_urls for variant scoping.
			"all_failure_type":                         "none",
			"all_failure_frequency":                    0,
			"all_consecutive_failures":                 0,
			"all_failure_units":                        "requests",
			"all_consecutive_units":                    "requests",
			"all_frequency_units":                      "seconds",
			"all_failure_mode":                         "failures_per_seconds",
			"current_failures":                         0,
			"consecutive_failures_count":               0,
			"player_ip":                                requesterIP,
			"user_agent":                               "",
			"origination_ip":                           requesterIP,
			"origination_time":                         createdAt,
			"is_external_ip":                           isExternalIP(requesterIP),
			"manifest_failure_at":                      nil,
			"manifest_failure_recover_at":              nil,
			"manifest_failure_urls":                    []string{},
			"segment_failure_urls":                     []string{},
			"segment_failure_at":                       nil,
			"segment_failure_recover_at":               nil,
			"master_manifest_failure_at":               nil,
			"master_manifest_failure_recover_at":       nil,
			"all_failure_at":                           nil,
			"all_failure_recover_at":                   nil,
			"all_failure_urls":                         []string{},
			"transport_failure_type":                   "none",
			"transport_failure_frequency":              0,
			"transport_consecutive_failures":           1,
			"transport_failure_units":                  "seconds",
			"transport_consecutive_units":              "seconds",
			"transport_frequency_units":                "seconds",
			"transport_failure_mode":                   "failures_per_seconds",
			"transport_failure_at":                     nil,
			"transport_failure_recover_at":             nil,
			"transport_fault_type":                     "none",
			"transport_fault_on_seconds":               1,
			"transport_fault_off_seconds":              0,
			"transport_consecutive_seconds":            1,
			"transport_frequency_seconds":              0,
			"transport_fault_active":                   false,
			"transport_fault_started_at":               nil,
			"transport_fault_drop_packets":             0,
			"transport_fault_reject_packets":           0,
			"fault_count_total":                        0,
			"fault_count_socket_reject":                0,
			"fault_count_socket_drop":                  0,
			"fault_count_socket_drop_before_headers":   0,
			"fault_count_socket_reject_before_headers": 0,
			"fault_count_socket_drop_after_headers":    0,
			"fault_count_socket_reject_after_headers":  0,
			"fault_count_socket_drop_mid_body":         0,
			"fault_count_socket_reject_mid_body":       0,
			"fault_count_request_connect_hang":         0,
			"fault_count_request_connect_reset":        0,
			"fault_count_request_connect_delayed":      0,
			"fault_count_request_first_byte_hang":      0,
			"fault_count_request_first_byte_reset":     0,
			"fault_count_request_first_byte_delayed":   0,
			"fault_count_request_body_hang":            0,
			"fault_count_request_body_reset":           0,
			"fault_count_request_body_delayed":         0,
			"transfer_active_timeout_seconds":          0,
			"transfer_idle_timeout_seconds":            0,
			"transfer_timeout_applies_segments":        true,
			"transfer_timeout_applies_manifests":       false,
			"transfer_timeout_applies_master":          false,
			"fault_count_transfer_active_timeout":      0,
			"fault_count_transfer_idle_timeout":        0,
			"x_forwarded_port":                         assignedInternalPort,
			"x_forwarded_port_external":                assignedExternalPort,
			"loop_count_server":                        0,
		}
		a.resetServerLoopState(fmt.Sprintf("%d", allocated))
		sessionList = append(sessionList, sessionData)
		a.saveSessionList(sessionList)
		manifestURL := "/" + escapedPath
		if r.URL.RawQuery != "" {
			manifestURL = manifestURL + "?" + r.URL.RawQuery
		}
		a.recordSessionStart(sessionData, manifestURL)
		host := hostWithoutPort(r.Host)
		newURL := fmt.Sprintf("http://%s:%s/%s", host, assignedExternalPort, escapedPath)
		if r.URL.RawQuery != "" {
			newURL = newURL + "?" + r.URL.RawQuery
		}
		log.Printf("Redirecting to new URL with port: %s %s -> %s", newURL, externalPort, assignedExternalPort)
		http.Redirect(w, r, newURL, http.StatusFound)
		return
	}

	if sessionNumber == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	index := -1
	for i, session := range sessionList {
		if getString(session, "session_id") == sessionNumber {
			index = i
			break
		}
	}
	if index == -1 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	sessionData := sessionList[index]
	sessionData["last_request"] = nowISO()
	sessionData["last_request_url"] = filename
	sessionData["user_agent"] = r.UserAgent()
	// Stamp the player's current play_id on the session so the SSE stream
	// (and downstream analytics) can partition by playback episode. The
	// player regenerates play_id at every fresh loadStream boundary
	// (issue #280); when this value differs from the prior snapshot,
	// that's a "new playback started" signal in replay views.
	if playID != "" {
		sessionData["play_id"] = playID
	}

	// Extract client IP considering X-Forwarded-For
	clientIP := extractClientIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"))
	sessionData["player_ip"] = clientIP
	sessionData["x_forwarded_for"] = r.Header.Get("X-Forwarded-For")

	// Track origination IP on first request
	if _, hasOriginIP := sessionData["origination_ip"]; !hasOriginIP {
		sessionData["origination_ip"] = clientIP
		sessionData["origination_time"] = nowISO()
		sessionData["is_external_ip"] = isExternalIP(clientIP)

		// Log external IP access
		if isExternalIP(clientIP) {
			log.Printf("[GO-PROXY][EXTERNAL-IP] session_id=%s player_id=%s ip=%s user_agent=%q",
				sessionNumber,
				getString(sessionData, "player_id"),
				clientIP,
				r.UserAgent(),
			)
		}
	}

	sessionData["x_forwarded_port"] = internalPort
	sessionData["x_forwarded_port_external"] = externalPort
	log.Printf(
		"[GO-PROXY][REQUEST] method=%s host=%s port=%s path=%s query=%s session_id=%s player_id_q=%s player_id_h=%s playback_session_h=%s client_ip=%s user_agent=%q",
		r.Method,
		hostWithoutPort(r.Host),
		hostPortOrDefault(r.Host, ""),
		r.URL.Path,
		r.URL.RawQuery,
		sessionNumber,
		r.URL.Query().Get("player_id"),
		r.Header.Get("Player-ID"),
		r.Header.Get("X-Playback-Session-Id"),
		clientIP,
		r.UserAgent(),
	)
	requestBytes := int64(0)
	if r.ContentLength > 0 {
		requestBytes = r.ContentLength
	}

	if _, ok := sessionData["session_start_time"]; !ok {
		sessionData["session_start_time"] = nowISO()
	}
	if startStr, ok := sessionData["session_start_time"].(string); ok {
		if startTime, err := time.Parse("2006-01-02T15:04:05.000", startStr); err == nil {
			sessionData["session_duration"] = math.Round(time.Since(startTime).Seconds()*1000) / 1000
		}
	}

	upstreamURL := fmt.Sprintf("http://%s:%s/%s", a.upstreamHost, a.upstreamPort, escapedPath)
	contentType, isMasterManifest, isManifest, isSegment, playlistInfo := a.getContentType(upstreamURL)
	requestKind := requestKindLabel(isSegment, isManifest, isMasterManifest)
	segmentTransferStartedAt := time.Time{}
	segmentTransferStartBytes := int64(0)
	var flightPortNum int
	if isSegment {
		segmentTransferStartedAt = time.Now()
		segmentTransferStartBytes, _ = a.getSessionWireTCBytesNow(sessionData)
		if fp, err := strconv.Atoi(internalPort); err == nil {
			flightPortNum = fp
			log.Printf("SEGMENT_FLIGHT_INIT port=%d internalPort=%s externalPort=%s", fp, internalPort, externalPort)
		}
	}

	if isMasterManifest {
		sessionData["master_manifest_url"] = filename
	}
	if isManifest {
		sessionData["manifest_url"] = filename
	}
	if playlistInfo != nil {
		sessionData["manifest_variants"] = playlistInfo
	}
	inferServerVideoRendition(sessionData, filename, isManifest, isSegment)
	if isSegment {
		a.observeServerSegmentLoop(sessionData, filename)
	}

	handler := NewRequestHandler(isSegment, isManifest, isMasterManifest, sessionData)
	// Serialise the failure-decision read-modify-write so video+audio
	// requests arriving in the same millisecond don't both pass the
	// "1 per N seconds" filter and double-fire.
	//
	// The full atomic sequence is:
	//   1. Refresh dedup state from the latest snap (defeats stale
	//      clones).
	//   2. Run HandleRequest (decides + writes to local clone).
	//   3. Save back to the snap BEFORE unlocking, so the next
	//      goroutine to take the lock sees this goroutine's writes
	//      when it refreshes.
	// The save MUST be inside the lock — if it's after the unlock,
	// a second goroutine can acquire the lock and refresh from a
	// snap that still doesn't have the first goroutine's writes,
	// and the rule fires twice.
	sessionStateMu.Lock()
	refreshFailureStateFromLatest(a, sessionData, sessionNumber)
	failureType := handler.HandleRequest(filename)
	a.saveSessionByID(sessionNumber, sessionData)
	sessionStateMu.Unlock()

	if failureType != "none" {
		log.Printf("FAILURE! Identifier: %s, %s, %s", sessionNumber, upstreamURL, failureType)
		actionTaken := ""
		if failureType == "corrupted" && isSegment {
			proxyReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
			if err != nil {
				actionTaken = "http_502_new_request_failed"
				w.WriteHeader(http.StatusBadGateway)
				bumpFaultCounter(sessionData, failureType)
				logFaultEvent(sessionData, externalPort, failureType, requestKind, actionTaken)
				updateSessionTraffic(sessionData, requestBytes, 0)
				// Log network entry for fault
				sessionID := getString(sessionData, "session_id")
				netEntry := createFaultLogEntry(playerURL, upstreamURL, requestKind, failureType, actionTaken, http.StatusBadGateway, requestBytes, requestReceivedAt)
				stampNetMeta(&netEntry, requestHeaders, queryString, nil)
				logEntry(sessionID,netEntry)
				sessionList[index] = sessionData
				a.saveSessionList(sessionList)
				return
			}
			resp, netEntry, err := a.doRequestWithTracing(r.Context(), proxyReq)
			// doRequestWithTracing populates URL/Path from the upstream
			// request — override with the player-facing values so HAR
			// entries reflect what the player did, not the proxy → origin URL.
			netEntry.URL = playerURL
			netEntry.Path = r.URL.Path
			if err != nil {
				netEntry.ClientWaitMs = elapsedMs(requestReceivedAt)
				if errors.Is(err, context.DeadlineExceeded) {
					actionTaken = "http_504_upstream_timeout"
					w.WriteHeader(http.StatusGatewayTimeout)
					netEntry.Status = http.StatusGatewayTimeout
				} else {
					actionTaken = "http_502_upstream_failed"
					w.WriteHeader(http.StatusBadGateway)
					netEntry.Status = http.StatusBadGateway
				}
				bumpFaultCounter(sessionData, failureType)
				logFaultEvent(sessionData, externalPort, failureType, requestKind, actionTaken)
				updateSessionTraffic(sessionData, requestBytes, 0)
				// Log network entry with timing (even for failures)
				sessionID := getString(sessionData, "session_id")
				netEntry.RequestKind = requestKind
				netEntry.BytesIn = requestBytes
				netEntry.Faulted = true
				netEntry.FaultType = failureType
				netEntry.FaultAction = actionTaken
				netEntry.FaultCategory = categorizeFaultType(failureType)
				stampNetMeta(netEntry, requestHeaders, queryString, nil)
				logEntry(sessionID,*netEntry)
				sessionList[index] = sessionData
				a.saveSessionList(sessionList)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				actionTaken = fmt.Sprintf("http_%d_upstream", resp.StatusCode)
				netEntry.ClientWaitMs = elapsedMs(requestReceivedAt)
				w.WriteHeader(resp.StatusCode)
				bumpFaultCounter(sessionData, failureType)
				logFaultEvent(sessionData, externalPort, failureType, requestKind, actionTaken)
				updateSessionTraffic(sessionData, requestBytes, 0)
				// Log network entry with upstream error
				sessionID := getString(sessionData, "session_id")
				netEntry.RequestKind = requestKind
				netEntry.BytesIn = requestBytes
				netEntry.Faulted = true
				netEntry.FaultType = failureType
				netEntry.FaultAction = actionTaken
				netEntry.FaultCategory = categorizeFaultType(failureType)
				stampNetMeta(netEntry, requestHeaders, queryString, resp)
				logEntry(sessionID,*netEntry)
				sessionList[index] = sessionData
				a.saveSessionList(sessionList)
				return
			}
			if contentType != "" {
				w.Header().Set("Content-Type", contentType)
			}
			w.Header().Set("X-Session-ID", getString(sessionData, "session_number"))
			netEntry.ClientWaitMs = elapsedMs(requestReceivedAt)
			w.WriteHeader(http.StatusOK)
			bytesOut, transferMs, copyErr := streamToClientMeasured(w, resp.Body, true)
			if copyErr != nil && !errors.Is(copyErr, io.EOF) {
				log.Printf("segment_corrupted write error session_id=%s err=%v", getString(sessionData, "session_id"), copyErr)
			}
			netEntry.TransferMs = transferMs
			mergeTotalTiming(netEntry)
			actionTaken = "segment_corrupted_zero_fill"
			bumpFaultCounter(sessionData, failureType)
			logFaultEvent(sessionData, externalPort, failureType, requestKind, actionTaken)
			updateSessionTraffic(sessionData, requestBytes, bytesOut)
			if isSegment {
				a.applyFullSegmentNetworkBitrate(sessionData, segmentTransferStartBytes, segmentTransferStartedAt)
			}
			// Log network entry for corruption (has timing + bytes transferred, but zeroed)
			sessionID := getString(sessionData, "session_id")
			netEntry.RequestKind = requestKind
			netEntry.BytesIn = requestBytes
			netEntry.BytesOut = bytesOut
			netEntry.Faulted = true
			netEntry.FaultType = failureType
			netEntry.FaultAction = actionTaken
			netEntry.FaultCategory = categorizeFaultType(failureType)
			stampNetMeta(netEntry, requestHeaders, queryString, resp)
			logEntry(sessionID,*netEntry)
			sessionList[index] = sessionData
			a.saveSessionList(sessionList)
			return
		}
		if isSocketFaultType(failureType) {
			socketAction, err := applySocketFault(w, failureType, contentType)
			if err != nil {
				actionTaken = "fallback_http_503"
				w.WriteHeader(http.StatusServiceUnavailable)
			} else {
				actionTaken = socketAction
			}
			bumpFaultCounter(sessionData, failureType)
			logFaultEvent(sessionData, externalPort, failureType, requestKind, actionTaken)
			updateSessionTraffic(sessionData, requestBytes, 0)
			// The status logged here must reflect what the *client*
			// saw on the wire, not an internal sentinel:
			//  - request_connect_*  → nothing was written; no status.
			//                         Use 0 so the dashboard renders "—".
			//  - request_first_byte_*, request_body_* → applySocketFault
			//                         emitted "HTTP/1.1 200 OK" via
			//                         writeChunkedHeaders before the cut,
			//                         so the wire status is 200.
			//  - applySocketFault hijack-failure fallback → we wrote 503
			//                         via w.WriteHeader, so 503.
			sessionID := getString(sessionData, "session_id")
			var status int
			switch {
			case actionTaken == "fallback_http_503":
				status = http.StatusServiceUnavailable
			case strings.HasPrefix(failureType, "request_connect_"):
				status = 0
			default:
				// request_first_byte_*, request_body_*: chunked
				// headers (200 OK) were written before the cut.
				status = http.StatusOK
			}
			netEntry := createFaultLogEntry(playerURL, upstreamURL, requestKind, failureType, actionTaken, status, requestBytes, requestReceivedAt)
			stampNetMeta(&netEntry, requestHeaders, queryString, nil)
			logEntry(sessionID,netEntry)
			sessionList[index] = sessionData
			a.saveSessionList(sessionList)
			return
		}
		updateSessionTraffic(sessionData, requestBytes, 0)
		sessionList[index] = sessionData
		a.saveSessionList(sessionList)
		switch failureType {
		case "404":
			actionTaken = "http_404"
			w.WriteHeader(http.StatusNotFound)
		case "403":
			actionTaken = "http_403"
			w.WriteHeader(http.StatusForbidden)
		case "500":
			actionTaken = "http_500"
			w.WriteHeader(http.StatusInternalServerError)
		case "timeout":
			actionTaken = "http_504_timeout"
			w.WriteHeader(http.StatusGatewayTimeout)
		case "connection_refused":
			actionTaken = "http_503_connection_refused"
			w.WriteHeader(http.StatusServiceUnavailable)
		case "dns_failure":
			actionTaken = "http_502_dns_failure"
			w.WriteHeader(http.StatusBadGateway)
		case "rate_limiting":
			actionTaken = "http_429_rate_limited"
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			actionTaken = "http_500_unknown_failure"
			w.WriteHeader(http.StatusInternalServerError)
		}
		bumpFaultCounter(sessionData, failureType)
		logFaultEvent(sessionData, externalPort, failureType, requestKind, actionTaken)
		// Log network entry for HTTP faults
		sessionID := getString(sessionData, "session_id")
		status := http.StatusInternalServerError
		switch actionTaken {
		case "http_404":
			status = http.StatusNotFound
		case "http_403":
			status = http.StatusForbidden
		case "http_500":
			status = http.StatusInternalServerError
		case "http_504_timeout":
			status = http.StatusGatewayTimeout
		case "http_503_connection_refused":
			status = http.StatusServiceUnavailable
		case "http_502_dns_failure":
			status = http.StatusBadGateway
		case "http_429_rate_limited":
			status = http.StatusTooManyRequests
		}
		netEntry := createFaultLogEntry(playerURL, upstreamURL, requestKind, failureType, actionTaken, status, requestBytes, requestReceivedAt)
		stampNetMeta(&netEntry, requestHeaders, queryString, nil)
		logEntry(sessionID,netEntry)
		return
	}

	activeTimeout, idleTimeout := transferTimeoutsFor(sessionData, isSegment, isManifest, isMasterManifest)
	proxyCtx := r.Context()
	var proxyCancel context.CancelFunc
	if activeTimeout > 0 {
		proxyCtx, proxyCancel = context.WithTimeout(proxyCtx, activeTimeout)
	} else if idleTimeout > 0 {
		proxyCtx, proxyCancel = context.WithCancel(proxyCtx)
	} else {
		proxyCancel = func() {}
	}
	defer proxyCancel()

	proxyReq, err := http.NewRequestWithContext(proxyCtx, http.MethodGet, upstreamURL, nil)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		// Log network entry for error
		sessionID := getString(sessionData, "session_id")
		netEntry := createFaultLogEntry(playerURL, upstreamURL, requestKind, "none", "http_502_request_failed", http.StatusBadGateway, requestBytes, requestReceivedAt)
		stampNetMeta(&netEntry, requestHeaders, queryString, nil)
		logEntry(sessionID,netEntry)
		return
	}
	clientRange := r.Header.Get("Range")
	if clientRange != "" {
		proxyReq.Header.Set("Range", clientRange)
	}
	if ifRange := r.Header.Get("If-Range"); ifRange != "" {
		proxyReq.Header.Set("If-Range", ifRange)
	}
	resp, netEntry, err := a.doRequestWithTracing(proxyCtx, proxyReq)
	// doRequestWithTracing always returns a non-nil entry — but if a
	// future regression breaks that contract, fall back to a minimal
	// stub here so the rest of handleProxy can deref freely without
	// scattered nil-guards.
	if netEntry == nil {
		netEntry = &NetworkLogEntry{
			Timestamp: time.Now(),
			Method:    proxyReq.Method,
			URL:       playerURL,
			Path:      r.URL.Path,
		}
	}
	// doRequestWithTracing populates URL/Path from the upstream request —
	// override with the player-facing values so HAR entries reflect what
	// the player did, not the proxy → origin URL.
	netEntry.URL = playerURL
	netEntry.Path = r.URL.Path
	netEntry.RequestRange = clientRange
	if resp != nil {
		netEntry.ResponseContentRange = resp.Header.Get("Content-Range")
	}
	if err != nil {
		netEntry.ClientWaitMs = elapsedMs(requestReceivedAt)
		// Set status before writing header
		if errors.Is(err, context.DeadlineExceeded) {
			netEntry.Status = http.StatusGatewayTimeout
			w.WriteHeader(http.StatusGatewayTimeout)
			if activeTimeout > 0 {
				bumpFaultCounter(sessionData, "transfer_active_timeout")
				logFaultEvent(sessionData, externalPort, "transfer_active_timeout", requestKind, "transfer_active_timeout_pre_headers")
			}
		} else {
			netEntry.Status = http.StatusBadGateway
			w.WriteHeader(http.StatusBadGateway)
		}
		// Log network entry for error
		sessionID := getString(sessionData, "session_id")
		netEntry.RequestKind = requestKind
		netEntry.BytesIn = requestBytes
		stampNetMeta(netEntry, requestHeaders, queryString, nil)
		logEntry(sessionID,*netEntry)
		return
	}
	defer resp.Body.Close()
	// idleW wraps the downstream writer below so the timer measures gaps
	// in proxy-to-client writes (i.e. it fires when the player stops
	// draining bytes), not gaps in origin-to-proxy reads.
	var idleW *idleWriter
	if resp.StatusCode >= 400 {
		log.Printf(
			"PROXY upstream_status status=%d url=%s filename=%s request_kind=%s session_id=%s player_id=%s external_port=%s",
			resp.StatusCode,
			upstreamURL,
			filename,
			requestKind,
			getString(sessionData, "session_id"),
			getString(sessionData, "player_id"),
			externalPort,
		)
		netEntry.ClientWaitMs = elapsedMs(requestReceivedAt)
		w.WriteHeader(resp.StatusCode)
		// Log network entry for upstream error
		sessionID := getString(sessionData, "session_id")
		netEntry.RequestKind = requestKind
		netEntry.BytesIn = requestBytes
		stampNetMeta(netEntry, requestHeaders, queryString, resp)
		logEntry(sessionID,*netEntry)
		return
	}
	copyUpstreamHeaders(w, resp)
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("X-Session-ID", getString(sessionData, "session_number"))

	var bytesOut int64

	// Apply content manipulation for master playlists
	if isMasterManifest && shouldApplyContentManipulation(sessionData) {
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("ERROR: Failed to read master playlist body: %v", err)
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(proxyCtx.Err(), context.DeadlineExceeded) {
				bumpFaultCounter(sessionData, "transfer_active_timeout")
				logFaultEvent(sessionData, externalPort, "transfer_active_timeout", requestKind, "transfer_active_timeout_mid_body")
			}
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		modifiedBody, err := a.applyContentManipulation(bodyBytes, sessionData, contentType)
		if err != nil {
			log.Printf("ERROR: Failed to manipulate master playlist: %v", err)
			// Fall back to original content
			modifiedBody = bodyBytes
		}

		w.Header().Set("Content-Length", strconv.Itoa(len(modifiedBody)))
		netEntry.ClientWaitMs = elapsedMs(requestReceivedAt)
		w.WriteHeader(resp.StatusCode)
		var downstream io.Writer = w
		if idleTimeout > 0 {
			idleW = newIdleWriter(w, idleTimeout, proxyCancel)
			downstream = idleW
		}
		writer := bufio.NewWriter(downstream)
		transferStart := time.Now()
		_, writeErr := writer.Write(modifiedBody)
		flushErr := writer.Flush()
		netEntry.TransferMs = elapsedMs(transferStart)
		if idleW != nil {
			idleW.Stop()
		}
		bytesOut = int64(len(modifiedBody))
		log.Printf("[GO-PROXY][CONTENT] Applied content manipulation to master playlist session_id=%s", getString(sessionData, "session_id"))
		if writeErr != nil || flushErr != nil {
			if idleW != nil && idleW.timedOut.Load() {
				bumpFaultCounter(sessionData, "transfer_idle_timeout")
				logFaultEvent(sessionData, externalPort, "transfer_idle_timeout", requestKind, "transfer_idle_timeout_mid_body")
				netEntry.Faulted = true
				netEntry.FaultType = "transfer_idle_timeout"
				netEntry.FaultAction = "transfer_idle_timeout_mid_body"
				netEntry.FaultCategory = categorizeFaultType("transfer_idle_timeout")
			} else if errors.Is(writeErr, context.DeadlineExceeded) || errors.Is(flushErr, context.DeadlineExceeded) || errors.Is(proxyCtx.Err(), context.DeadlineExceeded) {
				bumpFaultCounter(sessionData, "transfer_active_timeout")
				logFaultEvent(sessionData, externalPort, "transfer_active_timeout", requestKind, "transfer_active_timeout_mid_body")
				netEntry.Faulted = true
				netEntry.FaultType = "transfer_active_timeout"
				netEntry.FaultAction = "transfer_active_timeout_mid_body"
				netEntry.FaultCategory = categorizeFaultType("transfer_active_timeout")
			} else {
				netEntry.Faulted = true
				netEntry.FaultType = "client_disconnect"
				netEntry.FaultAction = "transfer_abandoned"
				netEntry.FaultCategory = "client_disconnect"
			}
		}
	} else {
		netEntry.ClientWaitMs = elapsedMs(requestReceivedAt)
		w.WriteHeader(resp.StatusCode)
		var downstream io.Writer = w
		if idleTimeout > 0 {
			idleW = newIdleWriter(w, idleTimeout, proxyCancel)
			downstream = idleW
		}
		writer := bufio.NewWriter(downstream)
		if isSegment {
			log.Printf(
				"[GO-PROXY][REQUEST][SEGMENT] response status=%d content_type=%s content_length=%s accept_ranges=%s content_range=%s url=%s session_id=%s external_port=%s",
				resp.StatusCode,
				resp.Header.Get("Content-Type"),
				resp.Header.Get("Content-Length"),
				resp.Header.Get("Accept-Ranges"),
				resp.Header.Get("Content-Range"),
				upstreamURL,
				getString(sessionData, "session_id"),
				externalPort,
			)
		}
		var copyErr error
		transferStart := time.Now()
		bytesOut, copyErr = io.Copy(writer, resp.Body)
		flushErr := writer.Flush()
		netEntry.TransferMs = elapsedMs(transferStart)
		if idleW != nil {
			idleW.Stop()
		}
		if copyErr != nil || flushErr != nil {
			writeErr := copyErr
			if writeErr == nil {
				writeErr = flushErr
			}
			log.Printf("[GO-PROXY][ABANDONED] client disconnected mid-transfer url=%s bytes_sent=%d content_length=%s request_kind=%s session_id=%s external_port=%s err=%v",
				upstreamURL, bytesOut, resp.Header.Get("Content-Length"), requestKind, getString(sessionData, "session_id"), externalPort, writeErr)
			if idleW != nil && idleW.timedOut.Load() {
				bumpFaultCounter(sessionData, "transfer_idle_timeout")
				logFaultEvent(sessionData, externalPort, "transfer_idle_timeout", requestKind, "transfer_idle_timeout_mid_body")
				netEntry.Faulted = true
				netEntry.FaultType = "transfer_idle_timeout"
				netEntry.FaultAction = "transfer_idle_timeout_mid_body"
				netEntry.FaultCategory = categorizeFaultType("transfer_idle_timeout")
			} else if errors.Is(copyErr, context.DeadlineExceeded) || errors.Is(proxyCtx.Err(), context.DeadlineExceeded) {
				bumpFaultCounter(sessionData, "transfer_active_timeout")
				logFaultEvent(sessionData, externalPort, "transfer_active_timeout", requestKind, "transfer_active_timeout_mid_body")
				netEntry.Faulted = true
				netEntry.FaultType = "transfer_active_timeout"
				netEntry.FaultAction = "transfer_active_timeout_mid_body"
				netEntry.FaultCategory = categorizeFaultType("transfer_active_timeout")
			} else {
				// Real client closed the socket mid-body (broken pipe,
				// ECONNRESET, etc.). Not a deliberate fault — tag it
				// so the dashboard can show "abandoned by client" in
				// red without the user having to read the proxy log.
				netEntry.Faulted = true
				netEntry.FaultType = "client_disconnect"
				netEntry.FaultAction = "transfer_abandoned"
				netEntry.FaultCategory = "client_disconnect"
			}
		}
		if isManifest || isMasterManifest {
			log.Printf("[GO-PROXY][MANIFEST] url=%s bytes_sent=%d upstream_content_length=%s status=%d session_id=%s external_port=%s",
				upstreamURL, bytesOut, resp.Header.Get("Content-Length"), resp.StatusCode, getString(sessionData, "session_id"), externalPort)
		}
		// On the segment success path, hand flight-end off to the socket drain goroutine
		// so it waits for the TC backlog to drain before marking the flight as done.
		// Only launch on Linux where a.traffic is available (TC backlog polling requires TC).
		if flightPortNum > 0 && a.traffic != nil {
			port := flightPortNum
			go a.awaitSocketDrain(port)
		}
	}

	updateSessionTraffic(sessionData, requestBytes, bytesOut)
	if isSegment {
		a.applyFullSegmentNetworkBitrate(sessionData, segmentTransferStartBytes, segmentTransferStartedAt)
	}
	// Log successful network entry
	sessionID := getString(sessionData, "session_id")
	netEntry.RequestKind = requestKind
	netEntry.BytesIn = requestBytes
	netEntry.BytesOut = bytesOut
	stampNetMeta(netEntry, requestHeaders, queryString, resp)
	logEntry(sessionID,*netEntry)
	a.saveSessionByID(sessionNumber, sessionData)
}

// shouldApplyContentManipulation checks if any content manipulation settings are enabled
func shouldApplyContentManipulation(session SessionData) bool {
	if getBool(session, "content_strip_codecs") {
		return true
	}
	if getBool(session, "content_strip_average_bandwidth") {
		return true
	}
	if getBool(session, "content_overstate_bandwidth") {
		return true
	}
	if getInt(session, "content_live_offset") > 0 {
		return true
	}
	allowedVariants := getStringSlice(session, "content_allowed_variants")
	if len(allowedVariants) > 0 {
		return true
	}
	return false
}

// applyContentManipulation modifies master playlist/manifest content based on session settings
func (a *App) applyContentManipulation(body []byte, session SessionData, contentType string) ([]byte, error) {
	stripCodecs := getBool(session, "content_strip_codecs")
	stripAvgBandwidth := getBool(session, "content_strip_average_bandwidth")
	overstateBandwidth := getBool(session, "content_overstate_bandwidth")
	liveOffset := getInt(session, "content_live_offset")
	allowedVariants := getStringSlice(session, "content_allowed_variants")

	// Handle HLS master playlists
	if strings.Contains(strings.ToLower(contentType), "mpegurl") || strings.Contains(strings.ToLower(contentType), "m3u8") {
		result, err := manipulateHLSMaster(body, stripCodecs, stripAvgBandwidth, overstateBandwidth, liveOffset, allowedVariants)
		if err != nil {
			return nil, err
		}
		// Variant playlists carry HOLD-BACK / PART-HOLD-BACK and their own
		// EXT-X-START tag (not all players honor master-level inheritance —
		// notably hls.js, which would otherwise park at the oldest segment).
		// Master EXT-X-START is rewritten inside manipulateHLSMaster; this
		// pass handles the variant side. No-op on master playlists.
		if liveOffset > 0 {
			result = rewriteVariantLiveOffsetTags(result, liveOffset)
		}
		return result, nil
	}

	// Handle DASH manifests
	if strings.Contains(strings.ToLower(contentType), "dash") || strings.Contains(strings.ToLower(contentType), "mpd") {
		return manipulateDASHManifest(body, stripCodecs, stripAvgBandwidth, overstateBandwidth, liveOffset, allowedVariants)
	}

	return body, nil
}

// manipulateHLSMaster modifies an HLS master playlist
func manipulateHLSMaster(body []byte, stripCodecs bool, stripAvgBandwidth bool, overstateBandwidth bool, liveOffset int, allowedVariants []string) ([]byte, error) {
	playlist, listType, err := m3u8.DecodeFrom(bufio.NewReader(bytes.NewReader(body)), true)
	if err != nil {
		return nil, fmt.Errorf("failed to decode HLS playlist: %w", err)
	}

	if listType != m3u8.MASTER {
		// Not a master playlist, return unchanged
		return body, nil
	}

	master := playlist.(*m3u8.MasterPlaylist)
	modified := false

	// Filter variants if allowedVariants is specified
	if len(allowedVariants) > 0 {
		allowedMap := make(map[string]bool)
		for _, v := range allowedVariants {
			allowedMap[v] = true
		}

		filteredVariants := make([]*m3u8.Variant, 0)
		for _, variant := range master.Variants {
			if variant != nil && allowedMap[variant.URI] {
				filteredVariants = append(filteredVariants, variant)
			}
		}

		if len(filteredVariants) != len(master.Variants) {
			master.Variants = filteredVariants
			modified = true
		}
	}

	// Strip codecs if requested
	if stripCodecs {
		hasCodecs := false
		for _, variant := range master.Variants {
			if variant != nil && variant.Codecs != "" {
				hasCodecs = true
				variant.Codecs = ""
			}
		}
		if hasCodecs {
			modified = true
		}
	}

	// Strip AVERAGE-BANDWIDTH if requested
	if stripAvgBandwidth {
		for _, variant := range master.Variants {
			if variant != nil && variant.AverageBandwidth > 0 {
				variant.AverageBandwidth = 0
				modified = true
			}
		}
	}

	// Overstate BANDWIDTH and AVERAGE-BANDWIDTH by 10% if requested
	if overstateBandwidth {
		for _, variant := range master.Variants {
			if variant == nil {
				continue
			}
			if variant.Bandwidth > 0 {
				variant.Bandwidth = uint32(float64(variant.Bandwidth) * 1.10)
				modified = true
			}
			if variant.AverageBandwidth > 0 {
				variant.AverageBandwidth = uint32(float64(variant.AverageBandwidth) * 1.10)
				modified = true
			}
		}
	}

	// Inject #EXT-X-START with negative offset for live edge positioning
	if liveOffset > 0 {
		modified = true
	}

	if !modified {
		return body, nil
	}

	// Encode the modified playlist
	var buf bytes.Buffer
	_, err = master.Encode().WriteTo(&buf)
	if err != nil {
		return nil, fmt.Errorf("failed to encode HLS playlist: %w", err)
	}

	// Replace (or inject) EXT-X-START when a session live_offset is set.
	// As of the go-live master playlist injection, master.m3u8 already
	// contains a default EXT-X-START tag. When the session specifies a
	// liveOffset, we must *replace* that existing value — not append a
	// second one — so the playlist ends up with exactly one tag carrying
	// the user's requested offset. A negative/zero liveOffset means "no
	// override, pass through go-live's default". Injection goes AFTER
	// #EXT-X-VERSION so AVPlayer sees the version before any higher-version
	// tags — inserting between #EXTM3U and #EXT-X-VERSION triggers -12646
	// "playlist parse error".
	if liveOffset > 0 {
		encoded := buf.String()
		startTag := fmt.Sprintf("#EXT-X-START:TIME-OFFSET=-%d,PRECISE=YES\n", liveOffset)
		if idx := strings.Index(encoded, "#EXT-X-START:"); idx >= 0 {
			end := strings.Index(encoded[idx:], "\n")
			if end < 0 {
				encoded = encoded[:idx] + strings.TrimRight(startTag, "\n")
			} else {
				encoded = encoded[:idx] + startTag + encoded[idx+end+1:]
			}
		} else if idx := strings.Index(encoded, "#EXT-X-VERSION:"); idx >= 0 {
			end := strings.Index(encoded[idx:], "\n")
			if end >= 0 {
				insertAt := idx + end + 1
				encoded = encoded[:insertAt] + startTag + encoded[insertAt:]
			}
		} else {
			encoded = strings.Replace(encoded, "#EXTM3U\n", "#EXTM3U\n"+startTag, 1)
		}
		buf.Reset()
		buf.WriteString(encoded)
	}

	return buf.Bytes(), nil
}

// rewriteVariantLiveOffsetTags updates HOLD-BACK inside EXT-X-SERVER-CONTROL
// lines and TIME-OFFSET inside EXT-X-START lines of a media (variant)
// playlist. PART-HOLD-BACK is intentionally left alone: it's a sub-second
// timing parameter (>= 3× partial duration, ~0.6s on our LL playlist), not a
// window-scale offset, so applying the user's content_live_offset value
// (typically 6–24s) to it would push LL clients to a deep offset and defeat
// the LL use case. Other attributes (CAN-SKIP-UNTIL, PRECISE, etc.) are
// preserved. No clamping: the user is testing spec-violating values too
// (HLS spec requires HOLD-BACK >= 3× target duration; AVPlayer rejects below
// that with -12646), so we surface the chosen value verbatim.
// Master-playlist EXT-X-START is rewritten elsewhere — see manipulateHLSMaster.
var (
	serverControlLineRE = regexp.MustCompile(`(?m)^#EXT-X-SERVER-CONTROL:.*$`)
	extXStartLineRE     = regexp.MustCompile(`(?m)^#EXT-X-START:.*$`)
)

func rewriteVariantLiveOffsetTags(body []byte, liveOffsetSecs int) []byte {
	// Master playlists carry #EXT-X-STREAM-INF and have no HOLD-BACK; their
	// EXT-X-START is already rewritten by manipulateHLSMaster. Skip cleanly
	// here so we don't double-write (same value, but hidden coupling).
	if bytes.Contains(body, []byte("#EXT-X-STREAM-INF")) {
		return body
	}
	holdBackValue := fmt.Sprintf("%d.000", liveOffsetSecs)
	timeOffsetValue := fmt.Sprintf("-%d.000", liveOffsetSecs)
	body = serverControlLineRE.ReplaceAllFunc(body, func(line []byte) []byte {
		const prefix = "#EXT-X-SERVER-CONTROL:"
		attrs := strings.Split(strings.TrimPrefix(string(line), prefix), ",")
		for i, a := range attrs {
			if strings.HasPrefix(strings.TrimSpace(a), "HOLD-BACK=") {
				attrs[i] = "HOLD-BACK=" + holdBackValue
			}
		}
		return []byte(prefix + strings.Join(attrs, ","))
	})
	body = extXStartLineRE.ReplaceAllFunc(body, func(line []byte) []byte {
		const prefix = "#EXT-X-START:"
		attrs := strings.Split(strings.TrimPrefix(string(line), prefix), ",")
		for i, a := range attrs {
			if strings.HasPrefix(strings.TrimSpace(a), "TIME-OFFSET=") {
				attrs[i] = "TIME-OFFSET=" + timeOffsetValue
			}
		}
		return []byte(prefix + strings.Join(attrs, ","))
	})
	return body
}

// manipulateDASHManifest modifies a DASH manifest
// Note: stripCodecs and allowedVariants parameters are reserved for future DASH implementation
func manipulateDASHManifest(body []byte, stripCodecs bool, stripAvgBandwidth bool, overstateBandwidth bool, liveOffset int, allowedVariants []string) ([]byte, error) {
	// DASH manifest manipulation would require XML parsing and manipulation
	// using libraries like encoding/xml or third-party XML processors.
	// This is deferred to keep the initial implementation focused on HLS.
	_ = stripCodecs        // Silence unused parameter warning
	_ = stripAvgBandwidth  // Silence unused parameter warning
	_ = overstateBandwidth // Silence unused parameter warning
	_ = liveOffset         // Silence unused parameter warning
	_ = allowedVariants    // Silence unused parameter warning
	log.Printf("[GO-PROXY][CONTENT] DASH manifest manipulation not yet implemented")
	return body, nil
}

func (a *App) applySessionShaping(session SessionData, port int) {
	if a.traffic == nil || runtime.GOOS != "linux" {
		log.Printf("NETSHAPE apply skipped port=%d reason=traffic_unavailable_or_non_linux runtime=%s traffic_nil=%t", port, runtime.GOOS, a.traffic == nil)
		return
	}
	if getBool(session, "nftables_pattern_enabled") || sessionHasPatternSteps(session) {
		// Pattern loop owns the rate while enabled; avoid per-request overrides.
		log.Printf("NETSHAPE apply skipped port=%d reason=pattern_enabled pattern_enabled=%t pattern_steps=%t", port, getBool(session, "nftables_pattern_enabled"), sessionHasPatternSteps(session))
		return
	}
	rate := getFloat(session, "nftables_bandwidth_mbps")
	delay := getInt(session, "nftables_delay_ms")
	loss := getFloat(session, "nftables_packet_loss")
	if err := a.applyShapeIfChanged(port, rate, delay, loss); err != nil {
		log.Printf("NETSHAPE apply failed port=%d rate=%g delay=%d loss=%.2f: %v", port, rate, delay, loss, err)
		return
	}
}

func almostEqualShape(a ShapeApplyState, b ShapeApplyState) bool {
	const eps = 0.0001
	return a.delay == b.delay &&
		math.Abs(a.rate-b.rate) <= eps &&
		math.Abs(a.loss-b.loss) <= eps
}

func copyUpstreamHeaders(w http.ResponseWriter, resp *http.Response) {
	if resp == nil {
		return
	}
	// Copy relevant headers for media playback and range handling.
	pass := []string{
		"Accept-Ranges",
		"Cache-Control",
		"Content-Length",
		"Content-Range",
		"Content-Type",
		"ETag",
		"Expires",
		"Last-Modified",
	}
	for _, key := range pass {
		if value := resp.Header.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
}

func (a *App) applyShapeIfChanged(port int, rate float64, delay int, loss float64) error {
	const eps = 0.0001
	desired := ShapeApplyState{rate: rate, delay: delay, loss: loss}
	last, ok := a.getShapeApplyState(port)
	if ok && almostEqualShape(last, desired) {
		log.Printf("NETSHAPE apply skipped port=%d reason=unchanged rate_mbps=%.3f delay_ms=%d loss_pct=%.3f", port, rate, delay, loss)
		return nil
	}
	if rate == 0 && delay == 0 && loss == 0 {
		log.Printf("NETSHAPE apply clear port=%d", port)
		if err := a.traffic.UpdateRateLimit(port, 0); err != nil {
			return err
		}
		a.setShapeApplyState(port, desired)
		return nil
	}
	rateChanged := !ok || math.Abs(last.rate-rate) > eps
	if rateChanged {
		log.Printf("NETSHAPE apply rate_change port=%d from_mbps=%.3f to_mbps=%.3f", port, last.rate, rate)
		if err := a.traffic.UpdateRateLimit(port, rate); err != nil {
			return err
		}
	}
	delayChanged := !ok || last.delay != delay
	lossChanged := !ok || math.Abs(last.loss-loss) > eps
	if delayChanged || lossChanged {
		log.Printf("NETSHAPE apply netem_change port=%d from_delay_ms=%d to_delay_ms=%d from_loss_pct=%.3f to_loss_pct=%.3f", port, last.delay, delay, last.loss, loss)
		if err := a.traffic.UpdateNetem(port, delay, loss); err != nil {
			return err
		}
	}
	a.setShapeApplyState(port, desired)
	return nil
}

func (a *App) getContentType(target string) (string, bool, bool, bool, []PlaylistInfo) {
	parsed, err := url.Parse(target)
	if err != nil {
		return "", false, false, false, nil
	}
	if parsed.Hostname() != "" {
		parsed.Host = fmt.Sprintf("%s:%s", parsed.Hostname(), a.upstreamPort)
	}
	headReq, err := http.NewRequest(http.MethodHead, parsed.String(), nil)
	if err != nil {
		return "", false, false, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	headReq = headReq.WithContext(ctx)
	resp, err := a.client.Do(headReq)
	if err != nil {
		return "", false, false, false, nil
	}
	contentType := resp.Header.Get("Content-Type")
	resp.Body.Close()

	if resp.StatusCode == http.StatusMethodNotAllowed {
		contentType = ""
	}
	if strings.HasSuffix(strings.ToLower(parsed.Path), ".m3u8") && contentType == "" {
		contentType = "application/vnd.apple.mpegurl"
	}
	if strings.HasSuffix(strings.ToLower(parsed.Path), ".mpd") && contentType == "" {
		contentType = "application/dash+xml"
	}

	if strings.Contains(strings.ToLower(contentType), "mpegurl") {
		getReq, _ := http.NewRequest(http.MethodGet, parsed.String(), nil)
		ctxGet, cancelGet := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelGet()
		getReq = getReq.WithContext(ctxGet)
		getResp, err := a.client.Do(getReq)
		if err != nil {
			return contentType, false, true, false, nil
		}
		defer getResp.Body.Close()
		if getResp.StatusCode >= 400 {
			return contentType, false, true, false, nil
		}
		contentType = getResp.Header.Get("Content-Type")
		body, _ := io.ReadAll(getResp.Body)
		if len(body) > 0 {
			playlist, listType, err := m3u8.DecodeFrom(bufio.NewReader(bytes.NewReader(body)), true)
			if err == nil {
				switch listType {
				case m3u8.MASTER:
					master := playlist.(*m3u8.MasterPlaylist)
					infos := make([]PlaylistInfo, 0)
					for _, variant := range master.Variants {
						resolution := "unknown"
						if variant.Resolution != "" {
							resolution = variant.Resolution
						}
						infos = append(infos, PlaylistInfo{
							URL:        variant.URI,
							Bandwidth:  int(variant.Bandwidth),
							Resolution: resolution,
						})
					}
					return contentType, true, false, false, infos
				case m3u8.MEDIA:
					return contentType, false, true, false, nil
				}
			}
		}
		return contentType, false, true, false, nil
	}
	if strings.Contains(strings.ToLower(contentType), "dash") || strings.Contains(strings.ToLower(contentType), "mpd") {
		return contentType, false, true, false, nil
	}
	return contentType, false, false, true, nil
}

func (a *App) trackPortThroughput() {
	type throughputSample struct {
		at            time.Time
		deltaBytes    int64
		dtSeconds     float64
		active        bool
		inFlight      bool
		backlogActive bool // true when TC queue backlog > 0 (active segment transfer)
	}
	type a1sSample struct {
		at   time.Time
		mbps float64
	}
	type throughputState struct {
		bytes                int64
		timestamp            time.Time
		samples              []throughputSample
		a1sHistory           []a1sSample // rolling buffer of a1s values for a6s averaging
	}
	const (
		sampleInterval      = 100 * time.Millisecond
		shortWindow         = 1 * time.Second
		mediumWindow        = 6 * time.Second
		transferRateWindow   = 400 * time.Millisecond
		activeByteThreshold = int64(8192)
	)
	cache := map[int]throughputState{}
	counterReady := map[int]bool{}
	activePorts := map[int]struct{}{}
	lastPortsRefresh := time.Time{}
	updatePort := func(port int, bytesValue int64, backlogBytes int64, now time.Time) {
		if bytesValue <= 0 {
			return
		}
		state, ok := cache[port]
		if !ok || state.timestamp.IsZero() {
			cache[port] = throughputState{
				bytes:                bytesValue,
				timestamp:            now,
				samples:              state.samples,
				a1sHistory:           state.a1sHistory,
			}
			return
		}
		deltaBytes := bytesValue - state.bytes
		deltaSeconds := now.Sub(state.timestamp).Seconds()
		state.bytes = bytesValue
		state.timestamp = now
		if deltaBytes < 0 || deltaSeconds <= 0 {
			cache[port] = state
			return
		}
		flightInfo, inFlight := a.getSegmentFlightInfo(port)
		flightStart := flightInfo.startTime
		backlogActive := backlogBytes > 0
		if inFlight {
			log.Printf("SEGMENT_INFLIGHT port=%d age_ms=%d tc_backlog_bytes=%d backlog_active=%t", port, now.Sub(flightStart).Milliseconds(), backlogBytes, backlogActive)
		}
		// Sample TC queue backlog for mbps_video_app (TC drain rate).
		sample := throughputSample{
			at:            now,
			deltaBytes:    deltaBytes,
			dtSeconds:     deltaSeconds,
			active:        deltaBytes > activeByteThreshold,
			inFlight:      inFlight,
			backlogActive: backlogActive,
		}
		state.samples = append(state.samples, sample)
		// Trim samples older than 6s (needed for adjacentBacklogActiveRate).
		mediumCutoff := now.Add(-mediumWindow)
		shortCutoff := now.Add(-shortWindow)
		{
			trimmed := state.samples[:0]
			for _, s := range state.samples {
				if !s.at.Before(mediumCutoff) {
					trimmed = append(trimmed, s)
				}
			}
			state.samples = trimmed
		}
		// adjacentBacklogActiveRate computes throughput over the most recent contiguous
		// run of samples where backlogActive==true (TC queue had queued bytes).
		adjacentBacklogActiveRate := func(samples []throughputSample, cutoff time.Time) (float64, bool) {
			var runBytes int64
			runSeconds := 0.0
			inRun := false
			for idx := len(samples) - 1; idx >= 0; idx-- {
				existing := samples[idx]
				if existing.at.Before(cutoff) {
					break
				}
				if !inRun {
					if !existing.backlogActive {
						continue
					}
					inRun = true
				}
				if !existing.backlogActive {
					break
				}
				if existing.deltaBytes > 0 {
					runBytes += existing.deltaBytes
				}
				if existing.dtSeconds > 0 {
					runSeconds += existing.dtSeconds
				}
			}
			if !inRun || runSeconds <= 0 {
				return 0, false
			}
			return (float64(runBytes) * 8) / (runSeconds * 1024 * 1024), true
		}
		adjacent1sRate, hasAdjacent1s := adjacentBacklogActiveRate(state.samples, shortCutoff)

		var mbpsShaperRate interface{}
		if backlogActive && hasAdjacent1s {
			mbpsShaperRate = math.Round((adjacent1sRate * 100)) / 100
		} else {
			mbpsShaperRate = nil
		}
		// Record non-nil a1s values and compute a6s as their rolling average over 6s.
		if v, ok := mbpsShaperRate.(float64); ok {
			state.a1sHistory = append(state.a1sHistory, a1sSample{at: now, mbps: v})
		}
		{
			trimmed := state.a1sHistory[:0]
			for _, s := range state.a1sHistory {
				if !s.at.Before(mediumCutoff) {
					trimmed = append(trimmed, s)
				}
			}
			state.a1sHistory = trimmed
		}
		var mbpsShaperAvg interface{}
		if len(state.a1sHistory) > 0 {
			sum := 0.0
			for _, s := range state.a1sHistory {
				sum += s.mbps
			}
			mbpsShaperAvg = math.Round((sum/float64(len(state.a1sHistory)))*100) / 100
		}
		// mbps_transfer_rate: byte-change-gated rate computed in awaitSocketDrain.
		// Rate is only emitted when TC bytes change AND ≥100ms since previous
		// report, eliminating HTB burst aliasing.
		var mbpsTransferRate interface{}
		{
			a.wireRateMu.Lock()
			wr, ok := a.wireRate[port]
			a.wireRateMu.Unlock()
			if ok && now.Sub(wr.at) < transferRateWindow {
				mbpsTransferRate = wr.mbps
			}
		}
		// mbps_transfer_complete: completed-segment bitrate from SOCKET_DRAIN_DONE.
		// Emitted for one SSE tick after each drain completes.
		var mbpsTransferComplete interface{}
		{
			a.transferCompleteMu.Lock()
			drainMbps, ok := a.transferCompleteMbps[port]
			drainAt, _ := a.transferCompleteAt[port]
			a.transferCompleteMu.Unlock()
			// Only emit if the drain completed within the last SSE tick (~100ms).
			if ok && now.Sub(drainAt) < 2*sampleInterval {
				mbpsTransferComplete = drainMbps
			}
		}




		cache[port] = state
		payload := map[string]interface{}{
			"bytes":                       deltaBytes,
			"wire_tc_bytes_now":           bytesValue,
			"timestamp":                   now.Unix(),
			"timestamp_ms":                now.UnixMilli(),
			"mbps_shaper_rate":               mbpsShaperRate,
			"mbps_shaper_avg":               mbpsShaperAvg,
			"mbps_transfer_rate":           mbpsTransferRate,
			"mbps_transfer_complete":          mbpsTransferComplete,
		}
		a.throughputMu.Lock()
		a.throughputData[port] = payload
		a.throughputMu.Unlock()
		log.Printf(
			"WIRE_TC_METRIC port=%d bytes_now=%d delta_bytes=%d dt_s=%.3f active=%t",
			port,
			bytesValue,
			deltaBytes,
			deltaSeconds,
			sample.active,
		)
	}
	for {
		tickNow := time.Now()
		if lastPortsRefresh.IsZero() || tickNow.Sub(lastPortsRefresh) >= time.Second {
			sessions := a.getSessionList()
			refreshed := map[int]struct{}{}
			addPort := func(portStr string) {
				if portStr == "" {
					return
				}
				if port, err := strconv.Atoi(portStr); err == nil {
					refreshed[port] = struct{}{}
				}
			}
			for _, session := range sessions {
				addPort(getString(session, "x_forwarded_port"))
			}
			// Clear state for ports that are no longer active.
			for port := range cache {
				if _, ok := refreshed[port]; !ok {
					delete(cache, port)
					delete(counterReady, port)
				}
			}
			activePorts = refreshed
			lastPortsRefresh = tickNow
		}
		if len(activePorts) == 0 {
			time.Sleep(sampleInterval)
			continue
		}
		if a.traffic != nil && runtime.GOOS == "linux" {
			for port := range activePorts {
				if !counterReady[port] {
					if err := a.traffic.EnsureClass(port, 10000); err != nil {
						log.Printf("WIRE_TC_METRIC port=%d counter_ready=false ensure_class_err=%v", port, err)
						delete(cache, port) // clear stale state (a1sHistory etc.)
						continue
					}
					counterReady[port] = true
					log.Printf("WIRE_TC_METRIC port=%d counter_ready=true mode=unlimited_counter", port)
				}
				bytesValue, backlogBytes, err := a.traffic.GetPortStats(port)
				if err != nil {
					log.Printf("WIRE_TC_METRIC port=%d counter_read_err=%v", port, err)
					continue
				}
				sampleNow := time.Now()
				updatePort(port, bytesValue, backlogBytes, sampleNow)
			}
			time.Sleep(sampleInterval)
			continue
		}
		cmd := exec.Command("nft", "list", "chain", "inet", "throttle", "output")
		output, err := cmd.CombinedOutput()
		if err == nil {
			bytesValue := parseNftBytes(string(output))
			if bytesValue >= 0 {
				for port := range activePorts {
					sampleNow := time.Now()
					updatePort(port, bytesValue, -1, sampleNow) // no backlog info on non-Linux path
				}
			}
		}
		time.Sleep(sampleInterval)
	}
}

func parseNftBytes(output string) int64 {
	match := nftCounterRegex.FindStringSubmatch(output)
	if len(match) == 3 {
		val, _ := strconv.ParseInt(match[2], 10, 64)
		return val
	}
	return 0
}

func (a *App) getSessionData(identifier string) SessionData {
	if identifier == "" {
		return nil
	}
	sessions := a.getSessionList()
	for _, session := range sessions {
		if getString(session, "session_id") == identifier {
			return session
		}
	}
	return nil
}

func newControlRevision() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func parseControlRevision(rev string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, rev)
}

func isControlRevisionNewer(existing string, incoming string) bool {
	if existing == "" {
		return false
	}
	if incoming == "" {
		return true
	}
	existingAt, existingErr := parseControlRevision(existing)
	incomingAt, incomingErr := parseControlRevision(incoming)
	if existingErr != nil || incomingErr != nil {
		return existing > incoming
	}
	return existingAt.After(incomingAt)
}

func copySessionControlState(target SessionData, source SessionData) {
	if target == nil || source == nil {
		return
	}
	controlKeys := []string{
		"control_revision",
		"nftables_bandwidth_mbps",
		"nftables_delay_ms",
		"nftables_packet_loss",
		"nftables_pattern_enabled",
		"nftables_pattern_steps",
		"nftables_pattern_step",
		"nftables_pattern_step_runtime",
		"nftables_pattern_rate_runtime_mbps",
		"nftables_pattern_step_runtime_at",
		"nftables_pattern_segment_duration_seconds",
		"nftables_pattern_default_segments",
		"nftables_pattern_default_step_seconds",
		"nftables_pattern_template_mode",
		"nftables_pattern_margin_pct",
		"abrchar_run_lock",
		"abrchar_run_owner",
		"abrchar_run_started_at",
	}
	for _, key := range controlKeys {
		if value, ok := source[key]; ok {
			target[key] = value
		}
	}
}

func applyControlRevision(session SessionData, revision string) {
	rev := revision
	if rev == "" {
		rev = newControlRevision()
	}
	session["control_revision"] = rev
}

func cloneSession(session SessionData) SessionData {
	if session == nil {
		return nil
	}
	clone := make(SessionData, len(session))
	for key, value := range session {
		clone[key] = cloneInterface(value)
	}
	return clone
}

// getOrCreateNetworkLog retrieves or creates a network log ring buffer for a session
func (a *App) getOrCreateNetworkLog(sessionID string) *NetworkLogRingBuffer {
	a.networkLogsMu.RLock()
	if rb, exists := a.networkLogs[sessionID]; exists {
		a.networkLogsMu.RUnlock()
		return rb
	}
	a.networkLogsMu.RUnlock()

	a.networkLogsMu.Lock()
	defer a.networkLogsMu.Unlock()

	// Double-check after acquiring write lock
	if rb, exists := a.networkLogs[sessionID]; exists {
		return rb
	}

	// Keep enough requests to support a rolling 5-minute client view under load.
	rb := NewNetworkLogRingBuffer(5000)
	a.networkLogs[sessionID] = rb
	return rb
}

// addNetworkLogEntry adds a network log entry to the session's ring buffer
// and fans it out to any subscribed SSE clients (the analytics forwarder).
//
// If the entry arrived with an empty PlayID (typical for variant manifests
// and segments — iOS HLS doesn't preserve the master manifest's
// `?play_id=…` query string on derived URLs), fall back to the session's
// last-known sticky play_id from the live session map. session_snapshots
// already does this implicitly via the "if playID != ''" guard at the
// session level; without this fallback the network_requests table ends
// up with most rows attributed to play_id='' and the session-viewer's
// play_id filter only catches the master manifest hits.
func (a *App) addNetworkLogEntry(sessionID string, entry NetworkLogEntry) {
	if sessionID == "" {
		return
	}
	if entry.PlayID == "" {
		entry.PlayID = a.sessionStickyPlayID(sessionID)
	}
	rb := a.getOrCreateNetworkLog(sessionID)
	rb.Add(entry)
	if a.networkHub != nil {
		a.networkHub.Broadcast(sessionID, entry)
	}
}

// sessionStickyPlayID reads the session's last-known play_id from the
// atomic snapshot without cloning the whole list. Returns "" if the
// session isn't tracked or has no play_id stamped yet.
func (a *App) sessionStickyPlayID(sessionID string) string {
	snap := a.sessionsSnap.Load()
	if snap == nil {
		return ""
	}
	for _, s := range *snap {
		id, _ := s["session_id"].(string)
		if id != sessionID {
			continue
		}
		pid, _ := s["play_id"].(string)
		return pid
	}
	return ""
}

func durationToMilliseconds(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}

func mergeTotalTiming(entry *NetworkLogEntry) {
	if entry == nil {
		return
	}
	combined := entry.TTFBMs + entry.TransferMs
	if combined > entry.TotalMs {
		entry.TotalMs = combined
	}
}

// elapsedMs returns time.Since(t0) in fractional milliseconds.
func elapsedMs(t0 time.Time) float64 {
	return float64(time.Since(t0).Microseconds()) / 1000.0
}

// streamToClientMeasured copies bytes from src to client response writer and measures
// downstream write+flush time, which is where traffic shaping backpressure appears.
func streamToClientMeasured(w http.ResponseWriter, src io.Reader, zeroFill bool) (int64, float64, error) {
	var bytesOut int64
	var writeElapsed time.Duration
	buf := make([]byte, 32*1024)
	flusher, canFlush := w.(http.Flusher)

	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if zeroFill {
				for i := range chunk {
					chunk[i] = 0
				}
			}

			writeStart := time.Now()
			written, writeErr := w.Write(chunk)
			writeElapsed += time.Since(writeStart)
			if written > 0 {
				bytesOut += int64(written)
			}
			if writeErr != nil {
				return bytesOut, durationToMilliseconds(writeElapsed), writeErr
			}
			if written != n {
				return bytesOut, durationToMilliseconds(writeElapsed), io.ErrShortWrite
			}

			if canFlush {
				flushStart := time.Now()
				flusher.Flush()
				writeElapsed += time.Since(flushStart)
			}
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return bytesOut, durationToMilliseconds(writeElapsed), readErr
		}
	}

	return bytesOut, durationToMilliseconds(writeElapsed), nil
}

// doRequestWithTracing executes an HTTP request with timing trace and returns the response and timings
func (a *App) doRequestWithTracing(ctx context.Context, req *http.Request) (*http.Response, *NetworkLogEntry, error) {
	// Note: caller is expected to overwrite entry.URL / entry.Path with
	// the *player-facing* URL after this returns; what we set here is the
	// upstream URL. We populate UpstreamURL up front and copy it into URL
	// as a safe default for callers that don't override.
	upstreamURL := req.URL.String()
	entry := &NetworkLogEntry{
		Timestamp:   time.Now(),
		Method:      req.Method,
		URL:         upstreamURL,
		UpstreamURL: upstreamURL,
		Path:        req.URL.Path,
	}

	var start, dnsStart, connectStart, tlsStart time.Time
	start = time.Now()

	trace := &httptrace.ClientTrace{
		DNSStart: func(_ httptrace.DNSStartInfo) {
			dnsStart = time.Now()
		},
		DNSDone: func(_ httptrace.DNSDoneInfo) {
			if !dnsStart.IsZero() {
				entry.DNSMs = float64(time.Since(dnsStart).Microseconds()) / 1000.0
			}
		},
		ConnectStart: func(_, _ string) {
			connectStart = time.Now()
		},
		ConnectDone: func(_, _ string, _ error) {
			if !connectStart.IsZero() {
				entry.ConnectMs = float64(time.Since(connectStart).Microseconds()) / 1000.0
			}
		},
		TLSHandshakeStart: func() {
			tlsStart = time.Now()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, _ error) {
			if !tlsStart.IsZero() {
				entry.TLSMs = float64(time.Since(tlsStart).Microseconds()) / 1000.0
			}
		},
		GotFirstResponseByte: func() {
			// TTFB is from start of request to first byte
			entry.TTFBMs = float64(time.Since(start).Microseconds()) / 1000.0
		},
	}

	req = req.WithContext(httptrace.WithClientTrace(ctx, trace))

	resp, err := a.client.Do(req)
	if err != nil {
		entry.TotalMs = float64(time.Since(start).Microseconds()) / 1000.0
		return nil, entry, err
	}

	// If we got first byte, calculate transfer time after body is read
	// Note: We'll update TransferMs after body is copied
	entry.TotalMs = float64(time.Since(start).Microseconds()) / 1000.0
	entry.Status = resp.StatusCode
	entry.ContentType = resp.Header.Get("Content-Type")

	return resp, entry, nil
}

// createFaultLogEntry creates a network log entry for a faulted request.
// `playerURL` is what the player asked for; `upstreamURL` (optional, may be
// empty if the proxy never reached the upstream) is the resolved upstream URL
// used for forensics under HAR's _extensions.upstream.url.
func createFaultLogEntry(playerURL, upstreamURL, requestKind, faultType, faultAction string, status int, bytesIn int64, requestReceivedAt time.Time) NetworkLogEntry {
	wait := elapsedMs(requestReceivedAt)
	return NetworkLogEntry{
		Timestamp:     time.Now(),
		Method:        "GET",
		URL:           playerURL,
		UpstreamURL:   upstreamURL,
		Path:          extractPathFromURL(playerURL),
		RequestKind:   requestKind,
		Status:        status,
		BytesIn:       bytesIn,
		BytesOut:      0,
		Faulted:       true,
		FaultType:     faultType,
		FaultAction:   faultAction,
		FaultCategory: categorizeFaultType(faultType),
		// Fault paths short-circuit before any body — wait time is the
		// total latency from receiving the request to writing the
		// response status line.
		ClientWaitMs: wait,
		TotalMs:      wait,
	}
}

// extractPathFromURL extracts the path from a URL string
func extractPathFromURL(urlStr string) string {
	if u, err := url.Parse(urlStr); err == nil {
		return u.Path
	}
	return urlStr
}

// categorizeFaultType returns the category for a given fault type
func categorizeFaultType(faultType string) string {
	faultType = strings.ToLower(strings.TrimSpace(faultType))

	if faultType == "" || faultType == "none" {
		return ""
	}

	// Socket faults
	if strings.HasPrefix(faultType, "request_") {
		return "socket"
	}

	// Corruption
	if faultType == "corrupted" {
		return "corruption"
	}

	// Transport faults
	if strings.HasPrefix(faultType, "transport_") {
		return "transport"
	}

	// Server-enforced transfer timeouts (active or idle). These look
	// superficially like a 200 that got cut, same as a socket fault —
	// but the cut came from go-proxy's transfer-timeout policy, not
	// from injected request_body_*. Dashboard distinguishes via this
	// category so the waterfall can render a clock glyph rather than
	// the scissors used for deliberate fault injection.
	if strings.HasPrefix(faultType, "transfer_") {
		return "transfer_timeout"
	}

	// Client gave up mid-transfer (broken pipe / ECONNRESET from the
	// player's side). Tagged at the call site, but mirrored here for
	// consistency if anything else round-trips through this fn.
	if faultType == "client_disconnect" {
		return "client_disconnect"
	}

	// HTTP faults (404, 500, etc.)
	return "http"
}

func (a *App) normalizeSessionForResponse(session SessionData) SessionData {
	if session == nil {
		return nil
	}
	clone := cloneSession(session)
	normalized := a.normalizeSessionsForResponse([]SessionData{clone})
	if len(normalized) == 0 {
		return clone
	}
	return normalized[0]
}

func (a *App) updateSessionsByPortWithControl(port int, updates map[string]interface{}, controlRevision string) {
	sessions := a.getSessionList()
	changed := false
	rev := controlRevision
	if rev == "" {
		rev = newControlRevision()
	}
	for _, session := range sessions {
		if a.sessionMatchesPort(session, port) {
			log.Printf("NETSHAPE session_match port=%d session_id=%s before: x_forwarded_port=%s x_forwarded_port_external=%s nftables_bandwidth_mbps=%v",
				port, getString(session, "session_id"), getString(session, "x_forwarded_port"),
				getString(session, "x_forwarded_port_external"), session["nftables_bandwidth_mbps"])
			for key, value := range updates {
				session[key] = value
			}
			applyControlRevision(session, rev)
			log.Printf("NETSHAPE session_updated port=%d session_id=%s after: nftables_bandwidth_mbps=%v",
				port, getString(session, "session_id"), session["nftables_bandwidth_mbps"])
			changed = true
		}
	}
	if changed {
		a.saveSessionList(sessions)
	}
}

func (a *App) sessionPortToInternal(portStr string) (int, bool) {
	if portStr == "" {
		return 0, false
	}
	if mapped, ok := a.portMap.MapExternalPort(portStr); ok {
		if port, err := strconv.Atoi(mapped); err == nil {
			return port, true
		}
	}
	if port, err := strconv.Atoi(portStr); err == nil {
		return port, true
	}
	return 0, false
}

func (a *App) sessionMatchesPort(session SessionData, port int) bool {
	if portStr := getString(session, "x_forwarded_port"); portStr != "" {
		if portNum, ok := a.sessionPortToInternal(portStr); ok && portNum == port {
			return true
		}
	}
	if portStr := getString(session, "x_forwarded_port_external"); portStr != "" {
		if portNum, ok := a.sessionPortToInternal(portStr); ok && portNum == port {
			return true
		}
	}
	if a.portMap.count > 0 && a.portMap.externalBase > 0 && a.portMap.internalBase > 0 {
		sessionID := getString(session, "session_id")
		if sessionID != "" {
			if sessionNum, err := strconv.Atoi(sessionID); err == nil && sessionNum > 0 {
				desiredExternal := replaceThirdFromLastDigit(strconv.Itoa(a.portMap.externalBase), sessionNum)
				if mapped, ok := a.portMap.MapExternalPort(desiredExternal); ok {
					if mappedPort, err := strconv.Atoi(mapped); err == nil && mappedPort == port {
						session["x_forwarded_port_external"] = desiredExternal
						session["x_forwarded_port"] = mapped
						return true
					}
				}
			}
		}
	}
	return false
}

func (a *App) updateSessionsByPort(port int, updates map[string]interface{}) {
	sessions := a.getSessionList()
	changed := false
	for _, session := range sessions {
		if a.sessionMatchesPort(session, port) {
			for key, value := range updates {
				session[key] = value
			}
			changed = true
		}
	}
	if changed {
		a.saveSessionList(sessions)
	}
}

func (a *App) getSessionList() []SessionData {
	snap := a.sessionsSnap.Load()
	if snap == nil {
		return []SessionData{}
	}
	return cloneSessionList(*snap)
}

func (a *App) publishSnapshot(sessions []SessionData) {
	uiVersion := atomic.AddUint64(&a.uiStateVersionSeq, 1)
	uiRevision := newControlRevision()
	for _, session := range sessions {
		session["ui_state_version"] = uiVersion
		session["ui_state_revision"] = uiRevision
	}
	a.sessionsSnap.Store(&sessions)
	a.queueSessionsBroadcast(sessions)
}

func (a *App) saveSessionList(sessions []SessionData) {
	a.sessionsMu.Lock()
	a.publishSnapshot(cloneSessionList(sessions))
	a.sessionsMu.Unlock()
}

func (a *App) saveSessionByID(sessionID string, session SessionData) {
	a.sessionsMu.Lock()
	snap := a.getSessionList()
	updated := make([]SessionData, len(snap))
	copy(updated, snap)
	for i, s := range updated {
		if getString(s, "session_id") == sessionID {
			merged := cloneSession(s)
			for k, v := range session {
				merged[k] = v
			}
			existingRevision := getString(s, "control_revision")
			incomingRevision := getString(session, "control_revision")
			if isControlRevisionNewer(existingRevision, incomingRevision) {
				copySessionControlState(merged, s)
			}
			updated[i] = merged
			break
		}
	}
	a.publishSnapshot(updated)
	a.sessionsMu.Unlock()
}

func applySessionThroughput(session SessionData, throughput map[string]interface{}) {
	if session == nil || throughput == nil {
		return
	}
	session["measured_mbps"] = throughput["mbps"]
	session["measured_bytes"] = throughput["bytes"]
	session["wire_tc_bytes_now"] = throughput["wire_tc_bytes_now"]
	session["measurement_window"] = throughput["window_seconds"]
	session["mbps_shaper_rate"] = throughput["mbps_shaper_rate"]
	session["mbps_shaper_avg"] = throughput["mbps_shaper_avg"]
	session["mbps_transfer_rate"] = throughput["mbps_transfer_rate"]
	session["mbps_transfer_complete"] = throughput["mbps_transfer_complete"]
	session["wire_active_bytes"] = throughput["wire_active_bytes"]
	session["wire_total_bytes"] = throughput["wire_total_bytes"]
	session["wire_active_window_seconds"] = throughput["wire_active_window_seconds"]
	session["wire_window_seconds"] = throughput["wire_window_seconds"]
}

func (a *App) getSessionWireTCBytesNow(session SessionData) (int64, int64) {
	throughput := a.getSessionThroughput(session)
	if throughput == nil {
		return 0, 0
	}
	return int64FromInterface(throughput["wire_tc_bytes_now"]), int64FromInterface(throughput["timestamp_ms"])
}

func (a *App) markSegmentFlightStart(port int) uint64 {
	id := atomic.AddUint64(&a.segmentFlightSeq, 1)
	a.segmentFlightMu.Lock()
	a.segmentFlight[port] = segmentFlightInfo{startTime: time.Now(), id: id}
	a.segmentFlightMu.Unlock()
	log.Printf("SEGMENT_FLIGHT_START port=%d id=%d", port, id)
	return id
}

func (a *App) markSegmentFlightEnd(port int, id uint64) {
	a.segmentFlightMu.Lock()
	if info, ok := a.segmentFlight[port]; ok && info.id == id {
		delete(a.segmentFlight, port)
		log.Printf("SEGMENT_FLIGHT_END port=%d id=%d", port, id)
	} else if ok {
		log.Printf("SEGMENT_FLIGHT_END_SKIP port=%d id=%d current_id=%d (stale goroutine)", port, id, info.id)
	}
	a.segmentFlightMu.Unlock()
}

// getPortStatsForDrain returns (bytes, backlog, active, err) for a port.
//
// When eBPF stats are available it reads the BPF map (no subprocess).
// When eBPF is unavailable it falls back to netlink TC stats.
// active is derived from backlog > 0 (TC) or eBPF activeTTL.
const tcCacheTTL = 5 * time.Millisecond

func (a *App) getPortStatsForDrain(port int) (bytes int64, backlog int64, active bool, err error) {
	// Use per-port cache to avoid duplicate netlink calls from concurrent goroutines.
	a.tcCacheMu.Lock()
	cache := a.tcCache[port]
	if cache == nil {
		cache = &tcStatsCache{}
		a.tcCache[port] = cache
	}
	a.tcCacheMu.Unlock()

	cache.mu.Lock()
	defer cache.mu.Unlock()
	if time.Since(cache.at) < tcCacheTTL {
		return cache.bytes, cache.backlog, cache.backlog > 0, nil
	}
	b, bl, tcErr := a.traffic.GetPortStats(port)
	if tcErr != nil {
		return 0, 0, false, tcErr
	}
	cache.bytes = b
	cache.backlog = bl
	cache.at = time.Now()
	return b, bl, bl > 0, nil
}

// awaitSocketDrain tracks a segment transfer lifecycle via TC queue / eBPF byte counters.
//
// Phase 0 (5ms poll): confirms port is idle (backlog=0 / bytes stable) before watching.
// Phase 1 (5ms poll): waits for 0→active transition; fires SEGMENT_FLIGHT_START.
// Phase 2 (10ms poll): accumulates tcSamples; fires SEGMENT_FLIGHT_END when idle.
//
// tcSamples.backlog is set to 1 when bytes changed vs the previous poll, 0 when
// stable. This makes the mbps_transfer_rate backwards walk break precisely at the
// point bytes stopped incrementing — more accurate than TC backlog which can
// reflect queued-but-not-yet-sent data.
func (a *App) awaitSocketDrain(port int) {
	if a.traffic == nil {
		return
	}
	// Only one drain goroutine per port at a time.
	a.drainActiveMu.Lock()
	if a.drainActive[port] {
		a.drainActiveMu.Unlock()
		return
	}
	a.drainActive[port] = true
	a.drainActiveMu.Unlock()
	defer func() {
		a.drainActiveMu.Lock()
		delete(a.drainActive, port)
		a.drainActiveMu.Unlock()
	}()
	// Quick check: if there's no TC class for this port (no shaping configured),
	// we have no byte counters to work with — bail silently.
	// Quick check: if there's no TC class for this port (no shaping configured),
	// we have no byte counters to work with — bail silently.
	_, backlog, err := a.traffic.GetPortStats(port)
	if err != nil || backlog == -1 {
		return // no TC class → no stats available
	}
	// Phase 0: confirm port is idle before watching for a new transfer start.
	// Idle = bytes stable AND backlog == 0.
	var phase0Prev int64 = -1
	phase0Deadline := time.Now().Add(100 * time.Millisecond)
	for {
		bytes, backlog, _, err := a.getPortStatsForDrain(port)
		if err != nil {
			return
		}
		bytesStable := phase0Prev >= 0 && bytes == phase0Prev
		if bytesStable && backlog <= 0 {
			break
		}
		phase0Prev = bytes
		if time.Now().After(phase0Deadline) {
			// Port continuously active — skip Phase 0/1, go straight to Phase 2.
			// Use current bytes/time as the run start.
			log.Printf("SOCKET_DRAIN_BUSY port=%d backlog=%d (skipping to phase2)", port, backlog)
			phase0Prev = bytes
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Phase 1: wait for idle→active transition (up to 500ms).
	// Active = bytes incrementing OR backlog > 0.
	// If Phase 0 timed out (port busy), backlog is already > 0 so Phase 1
	// activates immediately.
	var runStartBytes int64
	var runStartTime time.Time
	var phase1PrevBytes int64 = phase0Prev
	phase1Deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(phase1Deadline) {
		t := time.Now()
		bytes, backlog, _, err := a.getPortStatsForDrain(port)
		if err != nil {
			return
		}
		if bytes > phase1PrevBytes || backlog > 0 {
			runStartBytes = bytes
			runStartTime = t
			break
		}
		phase1PrevBytes = bytes
		time.Sleep(5 * time.Millisecond)
	}
	if runStartTime.IsZero() {
		log.Printf("SOCKET_DRAIN_SKIP port=%d (port never became active)", port)
		return
	}
	// Active transition detected: fire SEGMENT_FLIGHT_START.
	id := a.markSegmentFlightStart(port)
	defer a.markSegmentFlightEnd(port, id)
	// Phase 2: poll every 10ms, accumulate tcSamples, until transfer completes.
	// Active = bytes incrementing OR backlog > 0.
	// Drain = backlog == 0 AND bytes stable for 2 consecutive polls.
	//
	// Wire rate (mbps_transfer_rate) is computed at byte-change events that are
	// ≥100ms after the previous report. This aligns measurement boundaries to
	// actual TC burst edges, eliminating HTB burst aliasing.
	var prevBytes int64 = -1
	var prevBacklog int64 = -1
	idleStreak := 0
	const idleThreshold = 2
	const wireRateMinGap = 250 * time.Millisecond
	var wireAnchorBytes int64 = runStartBytes
	var wireAnchorTime time.Time = runStartTime
	stallReported := false
	phase2Deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(phase2Deadline) {
		sampleTime := time.Now()
		bytes, backlogBytes, _, err := a.getPortStatsForDrain(port)
		if err != nil {
			return
		}
		bytesChanged := prevBytes >= 0 && bytes > prevBytes
		active := bytesChanged || backlogBytes > 0
		var backlogVal int64
		if active {
			backlogVal = 1
			idleStreak = 0
		} else if prevBytes >= 0 {
			idleStreak++
		}
		// Wire rate reporting with backlog-aware boundaries:
		//
		// 1. Normal: report on first byte-change ≥250ms after anchor.
		// 2. Drain: when backlog hits 0 AND bytes changed, report immediately
		//    (natural segment boundary — clean endpoint regardless of timer).
		// 3. Refill: when backlog transitions 0→>0, reset anchor (new segment
		//    starts flowing — begin fresh 250ms window).
		// 4. Stall: if bytes unchanged for ≥500ms, report 0 once.
		sinceLast := sampleTime.Sub(wireAnchorTime)
		backlogDrained := bytesChanged && backlogBytes <= 0 && prevBacklog > 0
		backlogRefilled := backlogBytes > 0 && prevBacklog == 0 && prevBacklog >= 0

		if backlogRefilled {
			// New data entering empty queue — reset anchor for fresh measurement.
			wireAnchorBytes = bytes
			wireAnchorTime = sampleTime
			stallReported = false
		} else if backlogDrained || (bytesChanged && sinceLast >= wireRateMinGap) {
			// Report rate: either backlog just drained (immediate) or ≥250ms elapsed.
			// Suppress tiny drain reports (< 1KB) — not meaningful rate data.
			// Stall reports (0 bytes) are handled separately and still fire.
			deltaBytes := bytes - wireAnchorBytes
			elapsed := sinceLast.Seconds()
			if !(backlogDrained && deltaBytes < 1024) && deltaBytes > 0 && elapsed > 0 {
				rate := math.Round(float64(deltaBytes)*8/(elapsed*1024*1024)*100) / 100
				tag := "interval"
				if backlogDrained {
					tag = "drain"
				}
				a.wireRateMu.Lock()
				a.wireRate[port] = wireRateSample{at: sampleTime, mbps: rate, bytes: deltaBytes}
				a.wireRateMu.Unlock()
				log.Printf("SEGMENT_WIRE_RATE port=%d mbps=%.2f delta_bytes=%d elapsed_ms=%d backlog=%d tag=%s",
					port, rate, deltaBytes, sinceLast.Milliseconds(), backlogBytes, tag)
			}
			wireAnchorBytes = bytes
			wireAnchorTime = sampleTime
			stallReported = false
		} else if !bytesChanged && !stallReported && sinceLast >= 2*wireRateMinGap {
			// Bytes stalled for ≥500ms — report 0 once.
			a.wireRateMu.Lock()
			a.wireRate[port] = wireRateSample{at: sampleTime, mbps: 0, bytes: 0}
			a.wireRateMu.Unlock()
			log.Printf("SEGMENT_WIRE_RATE port=%d mbps=0.00 stall_ms=%d backlog=%d tag=stall",
				port, sinceLast.Milliseconds(), backlogBytes)
			stallReported = true
		}
		prevBacklog = backlogBytes
		prevBytes = bytes
		sample := tcSample{at: sampleTime, bytes: bytes, backlog: backlogVal}
		a.tcSamplesMu.Lock()
		samples := a.tcSamples[port]
		samples = append(samples, sample)
		if len(samples) > 20 {
			samples = samples[len(samples)-20:]
		}
		a.tcSamples[port] = samples
		a.tcSamplesMu.Unlock()
		log.Printf("SEGMENT_WIRE_10MS port=%d bytes=%d backlog_bytes=%d active=%t idle_streak=%d", port, bytes, backlogBytes, active, idleStreak)
		if idleStreak >= idleThreshold {
			runEndTime := sampleTime
			elapsed := runEndTime.Sub(runStartTime).Seconds()
			runBytes := bytes - runStartBytes
			runMbps := 0.0
			if elapsed > 0 {
				runMbps = math.Round((float64(runBytes)*8)/(elapsed*1024*1024)*100) / 100
			}
			log.Printf("SOCKET_DRAIN_DONE port=%d run_bytes=%d elapsed_s=%.3f mbps=%.2f", port, runBytes, elapsed, runMbps)
			if runMbps > 0 {
				a.transferCompleteMu.Lock()
				a.transferCompleteMbps[port] = runMbps
				a.transferCompleteAt[port] = sampleTime
				a.transferCompleteMu.Unlock()
			}
			a.segmentRunMu.Lock()
			a.segmentRun[port] = segmentRunRecord{
				startTime:  runStartTime,
				startBytes: runStartBytes,
				endTime:    runEndTime,
				endBytes:   bytes,
			}
			a.segmentRunMu.Unlock()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	log.Printf("SOCKET_DRAIN_TIMEOUT port=%d (timed out waiting for port to go idle)", port)
}

func (a *App) getSegmentFlightInfo(port int) (segmentFlightInfo, bool) {
	a.segmentFlightMu.Lock()
	info, ok := a.segmentFlight[port]
	a.segmentFlightMu.Unlock()
	return info, ok
}

func (a *App) getSegmentFlightStart(port int) (time.Time, bool) {
	info, ok := a.getSegmentFlightInfo(port)
	return info.startTime, ok
}

func (a *App) applyFullSegmentNetworkBitrate(session SessionData, startBytes int64, startedAt time.Time) {
	if session == nil || startBytes <= 0 || startedAt.IsZero() {
		return
	}
	endBytes, _ := a.getSessionWireTCBytesNow(session)
	if endBytes <= startBytes {
		// Allow a brief wait for throughput sampler to publish post-transfer bytes.
		for i := 0; i < 6; i++ {
			time.Sleep(50 * time.Millisecond)
			endBytes, _ = a.getSessionWireTCBytesNow(session)
			if endBytes > startBytes {
				break
			}
		}
	}
	if endBytes <= startBytes {
		return
	}
	durationS := time.Since(startedAt).Seconds()
	if durationS <= 0 {
		return
	}
	bytesDelta := endBytes - startBytes
	mbps := (float64(bytesDelta) * 8.0) / (durationS * 1024.0 * 1024.0)
	session["full_segment_network_bitrate_mbps"] = math.Round(mbps*1000) / 1000
	session["full_segment_network_bytes"] = bytesDelta
	session["full_segment_network_duration_s"] = math.Round(durationS*1000) / 1000
}

func (a *App) getSessionThroughput(session SessionData) map[string]interface{} {
	if session == nil {
		return nil
	}
	portStr := getString(session, "x_forwarded_port")
	if portStr == "" {
		return nil
	}
	portNum, err := strconv.Atoi(portStr)
	if err != nil {
		return nil
	}
	a.throughputMu.RLock()
	data, ok := a.throughputData[portNum]
	a.throughputMu.RUnlock()
	if !ok {
		return nil
	}
	return data
}

func (a *App) hydrateSessionThroughput(session SessionData) {
	if session == nil {
		return
	}
	applySessionThroughput(session, a.getSessionThroughput(session))
}

func (a *App) normalizeSessionsForResponse(sessions []SessionData) []SessionData {
	transportCountersByPort := getTransportFaultRuleCounters()
	for _, session := range sessions {
		a.normalizeSessionPorts(session)
		a.hydrateSessionThroughput(session)
		// Surface the kernel-observed tc rate alongside (not over) the
		// configured rate. Closes the silent-divergence path in #352
		// without redefining what `nftables_bandwidth_mbps` means in
		// the analytics archive — that field stays the configured
		// value (what the user set). The new
		// `nftables_bandwidth_kernel_mbps` field is the live tc
		// class rate from the kernel; -1 means "no class installed".
		// When the two disagree by more than 0.5 Mbps the proxy
		// logs `NETSHAPE LEAK ...` so operators see the divergence
		// without it being silently fixed-up at the API layer.
		if a.traffic != nil {
			if port, err := strconv.Atoi(getString(session, "x_forwarded_port")); err == nil && port > 0 {
				kernelMbps := a.traffic.ReadActualRateMbps(port)
				session["nftables_bandwidth_kernel_mbps"] = kernelMbps
				if kernelMbps >= 0 {
					inMemMbps := getFloat(session, "nftables_bandwidth_mbps")
					if math.Abs(inMemMbps-kernelMbps) > 0.5 {
						log.Printf("NETSHAPE LEAK port=%d configured_mbps=%.3f kernel_mbps=%.3f session_id=%s",
							port, inMemMbps, kernelMbps, getString(session, "session_id"))
					}
				}
			}
		}
		setDefault := func(key string, value interface{}) {
			if existing, ok := session[key]; !ok || existing == nil {
				session[key] = value
			}
		}
		for _, prefix := range []string{"segment", "manifest", "master_manifest"} {
			typeKey := prefix + "_failure_type"
			failureType := normalizeRequestFailureType(getString(session, typeKey))
			if failureType == "" {
				failureType = "none"
			}
			session[typeKey] = failureType
			resetKey := prefix + "_reset_failure_type"
			if resetType := getString(session, resetKey); resetType != "" {
				session[resetKey] = normalizeRequestFailureType(resetType)
			}
		}
		if getString(session, "transport_failure_type") == "" {
			session["transport_failure_type"] = normalizeTransportFaultType(getString(session, "transport_fault_type"))
		}
		setDefault("ui_state_version", float64(0))
		setDefault("ui_state_revision", "")
		setDefault("player_restart_requested", false)
		setDefault("player_restart_request_id", "")
		setDefault("player_restart_request_reason", "")
		setDefault("player_restart_request_requested_at", "")
		setDefault("player_restart_request_state", "idle")
		setDefault("player_restart_request_handled_at", "")
		setDefault("player_restart_request_handled_by", "")
		setDefault("player_restart_request_error", "")
		if getString(session, "transport_failure_type") == "" {
			session["transport_failure_type"] = "none"
		}
		units := normalizeTransportConsecutiveUnits(getString(session, "transport_consecutive_units"))
		if units == transportUnitsSeconds {
			units = normalizeTransportConsecutiveUnits(getString(session, "transport_failure_units"))
		}
		if units == transportUnitsSeconds {
			units = transportConsecutiveUnitsFromMode(getString(session, "transport_failure_mode"))
		}
		session["transport_failure_units"] = units
		session["transport_consecutive_units"] = units
		session["transport_frequency_units"] = transportUnitsSeconds
		session["transport_failure_mode"] = transportModeFromConsecutiveUnits(units)
		if _, ok := session["transport_consecutive_failures"]; !ok {
			legacyOn := floatFromInterface(session["transport_consecutive_seconds"])
			if legacyOn <= 0 {
				legacyOn = floatFromInterface(session["transport_fault_on_seconds"])
			}
			if legacyOn <= 0 {
				legacyOn = 1
			}
			session["transport_consecutive_failures"] = int(math.Round(legacyOn))
		}
		if _, ok := session["transport_failure_frequency"]; !ok {
			legacyOff := floatFromInterface(session["transport_frequency_seconds"])
			if legacyOff < 0 {
				legacyOff = floatFromInterface(session["transport_fault_off_seconds"])
			}
			if legacyOff < 0 {
				legacyOff = 0
			}
			session["transport_failure_frequency"] = int(math.Round(legacyOff))
		}
		session["transport_fault_type"] = normalizeTransportFaultType(getString(session, "transport_failure_type"))
		session["transport_fault_on_seconds"] = float64(getInt(session, "transport_consecutive_failures"))
		session["transport_fault_off_seconds"] = float64(getInt(session, "transport_failure_frequency"))
		session["transport_consecutive_seconds"] = session["transport_fault_on_seconds"]
		session["transport_frequency_seconds"] = session["transport_fault_off_seconds"]
		if _, ok := session["transport_fault_active"]; !ok {
			session["transport_fault_active"] = false
		}
		if _, ok := session["fault_count_total"]; !ok {
			session["fault_count_total"] = 0
		}
		if _, ok := session["fault_count_socket_reject"]; !ok {
			session["fault_count_socket_reject"] = 0
		}
		if _, ok := session["fault_count_socket_drop"]; !ok {
			session["fault_count_socket_drop"] = 0
		}
		if _, ok := session["fault_count_socket_drop_before_headers"]; !ok {
			session["fault_count_socket_drop_before_headers"] = 0
		}
		if _, ok := session["fault_count_socket_reject_before_headers"]; !ok {
			session["fault_count_socket_reject_before_headers"] = 0
		}
		if _, ok := session["fault_count_socket_drop_after_headers"]; !ok {
			session["fault_count_socket_drop_after_headers"] = 0
		}
		if _, ok := session["fault_count_socket_reject_after_headers"]; !ok {
			session["fault_count_socket_reject_after_headers"] = 0
		}
		if _, ok := session["fault_count_socket_drop_mid_body"]; !ok {
			session["fault_count_socket_drop_mid_body"] = 0
		}
		if _, ok := session["fault_count_socket_reject_mid_body"]; !ok {
			session["fault_count_socket_reject_mid_body"] = 0
		}
		if _, ok := session["fault_count_request_connect_hang"]; !ok {
			session["fault_count_request_connect_hang"] = getInt(session, "fault_count_socket_drop_before_headers")
		}
		if _, ok := session["fault_count_request_connect_reset"]; !ok {
			session["fault_count_request_connect_reset"] = getInt(session, "fault_count_socket_reject_before_headers")
		}
		if _, ok := session["fault_count_request_connect_delayed"]; !ok {
			session["fault_count_request_connect_delayed"] = 0
		}
		if _, ok := session["fault_count_request_first_byte_hang"]; !ok {
			session["fault_count_request_first_byte_hang"] = getInt(session, "fault_count_socket_drop_after_headers")
		}
		if _, ok := session["fault_count_request_first_byte_reset"]; !ok {
			session["fault_count_request_first_byte_reset"] = getInt(session, "fault_count_socket_reject_after_headers")
		}
		if _, ok := session["fault_count_request_first_byte_delayed"]; !ok {
			session["fault_count_request_first_byte_delayed"] = 0
		}
		if _, ok := session["fault_count_request_body_hang"]; !ok {
			session["fault_count_request_body_hang"] = getInt(session, "fault_count_socket_drop_mid_body")
		}
		if _, ok := session["fault_count_request_body_reset"]; !ok {
			session["fault_count_request_body_reset"] = getInt(session, "fault_count_socket_reject_mid_body")
		}
		if _, ok := session["fault_count_request_body_delayed"]; !ok {
			session["fault_count_request_body_delayed"] = 0
		}
		if transportCountersByPort != nil {
			portStr := getString(session, "x_forwarded_port")
			if portStr != "" {
				if portNum, err := strconv.Atoi(portStr); err == nil {
					if counters, ok := transportCountersByPort[portNum]; ok {
						session["transport_fault_drop_packets"] = counters.DropPackets
						session["transport_fault_reject_packets"] = counters.RejectPackets
					}
				}
			}
		}
		portStr := getString(session, "x_forwarded_port")
		if portStr == "" {
			portStr = getString(session, "x_forwarded_port_external")
		}
		if portNum, ok := a.sessionPortToInternal(portStr); ok {
			if pattern, ok := a.getShapePattern(portNum); ok {
				session["nftables_pattern_enabled"] = len(pattern.Steps) > 0
				session["nftables_pattern_steps"] = pattern.Steps
				if pattern.ActiveAt != "" {
					session["nftables_pattern_step"] = pattern.ActiveStep
					session["nftables_pattern_step_runtime"] = pattern.ActiveStep
					session["nftables_pattern_rate_runtime_mbps"] = pattern.ActiveRateMbps
					session["nftables_pattern_step_runtime_at"] = pattern.ActiveAt
				}
			} else {
				session["nftables_pattern_enabled"] = false
				session["nftables_pattern_steps"] = []NftShapeStep{}
			}
		}
		setDefault("nftables_bandwidth_mbps", 0)
		setDefault("nftables_delay_ms", 0)
		setDefault("nftables_packet_loss", 0)
		setDefault("nftables_pattern_enabled", false)
		setDefault("nftables_pattern_steps", []NftShapeStep{})
		setDefault("nftables_pattern_step", 0)
		setDefault("nftables_pattern_step_runtime", 0)
		setDefault("nftables_pattern_rate_runtime_mbps", session["nftables_bandwidth_mbps"])
		setDefault("nftables_pattern_segment_duration_seconds", 0)
		setDefault("nftables_pattern_default_segments", 2)
		setDefault("nftables_pattern_default_step_seconds", 0)
		setDefault("nftables_pattern_template_mode", "sliders")
		setDefault("nftables_pattern_margin_pct", 0)
		setDefault("player_metrics_profile_shift_count", 0)
		setDefault("loop_count_server", 0)
		setDefault("player_metrics_loop_count_player", 0)
		setDefault("player_metrics_loop_count_increment", 0)
		bestMbps := bestVariantMbps(session)
		videoMbps := getFloat(session, "player_metrics_video_bitrate_mbps")
		if bestMbps > 0 && videoMbps > 0 {
			quality := (videoMbps / bestMbps) * 100
			session["player_metrics_video_quality_pct"] = math.Round(quality*100) / 100
		} else {
			delete(session, "player_metrics_video_quality_pct")
		}
	}
	return sessions
}

func (a *App) normalizeSessionPorts(session SessionData) {
	if a.portMap.count <= 0 || a.portMap.externalBase <= 0 || a.portMap.internalBase <= 0 {
		return
	}
	sessionID := getString(session, "session_id")
	if sessionID == "" {
		return
	}
	sessionNum, err := strconv.Atoi(sessionID)
	if err != nil || sessionNum <= 0 {
		return
	}
	currentExternal := getString(session, "x_forwarded_port_external")
	if currentExternal == "" {
		currentExternal = strconv.Itoa(a.portMap.externalBase)
	}
	currentExternalNum, err := strconv.Atoi(currentExternal)
	if err != nil {
		return
	}
	externalGroup := a.portMap.externalBase / 1000
	if externalGroup <= 0 || currentExternalNum/1000 != externalGroup {
		return
	}
	if thirdFromLastDigit(currentExternal) == strconv.Itoa(sessionNum) {
		return
	}
	desiredExternal := replaceThirdFromLastDigit(strconv.Itoa(a.portMap.externalBase), sessionNum)
	if desiredExternal == currentExternal {
		return
	}
	session["x_forwarded_port_external"] = desiredExternal
	if mapped, ok := a.portMap.MapExternalPort(desiredExternal); ok {
		session["x_forwarded_port"] = mapped
	}
}

func (a *App) buildSessionsEvent(normalized []SessionData, revision uint64, dropped uint64, activeSessions []ActiveSessionInfo) string {
	payload := SessionsStreamPayload{
		Revision:       revision,
		Dropped:        dropped,
		Sessions:       normalized,
		ActiveSessions: activeSessions,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("event: sessions\nid: %d\ndata: %s\n\n", revision, data)
}

func (a *App) queueSessionsBroadcast(sessions []SessionData) {
	if a.sessionsHub == nil {
		return
	}
	// Snapshot before stashing. The broadcast pipeline runs asynchronously
	// (250ms timer → normalizeSessionsForResponse mutates the maps → SSE
	// clients marshal them). Without this snapshot the pipeline shares maps
	// with the calling handler, which continues to mutate sessionData
	// between successive saveSessionList calls — that's the race that
	// triggers `concurrent map iteration and map write` in json.Marshal.
	snapshot := cloneSessionList(sessions)
	a.sessionsBroadcastMu.Lock()
	a.sessionsBroadcastLatest = snapshot
	if a.sessionsBroadcastPending {
		a.sessionsBroadcastMu.Unlock()
		return
	}
	a.sessionsBroadcastPending = true
	a.sessionsBroadcastMu.Unlock()
	time.AfterFunc(250*time.Millisecond, func() {
		a.flushSessionsBroadcast()
	})
}

// cloneSessionList deep-copies the maps inside the slice so the result can
// be iterated/mutated independently of the input. Primitives are shared by
// value; nested map[string]interface{} and []interface{} are recursively
// cloned. Other slice/value types (e.g. []PlaylistInfo) are treated as
// immutable from this point on.
func cloneSessionList(sessions []SessionData) []SessionData {
	if sessions == nil {
		return nil
	}
	out := make([]SessionData, len(sessions))
	for i, session := range sessions {
		out[i] = cloneSession(session)
	}
	return out
}

func cloneInterface(v interface{}) interface{} {
	switch t := v.(type) {
	case nil:
		return nil
	case SessionData:
		return cloneSession(t)
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, vv := range t {
			out[k] = cloneInterface(vv)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, vv := range t {
			out[i] = cloneInterface(vv)
		}
		return out
	case []SessionData:
		return cloneSessionList(t)
	default:
		return v
	}
}

func (a *App) flushSessionsBroadcast() {
	a.sessionsBroadcastMu.Lock()
	sessions := a.sessionsBroadcastLatest
	a.sessionsBroadcastLatest = nil
	a.sessionsBroadcastPending = false
	a.sessionsBroadcastMu.Unlock()
	if sessions == nil {
		return
	}
	normalized := a.normalizeSessionsForResponse(sessions)
	rev := atomic.AddUint64(&a.sessionsBroadcastSeq, 1)
	preMarshaled := a.buildSessionsEvent(normalized, rev, 0, nil)
	a.sessionsHub.Broadcast(normalized, rev, preMarshaled)
}

func (a *App) removeInactiveSessions() {
	sessions := a.getSessionList()
	if len(sessions) == 0 {
		return
	}
	active := make([]SessionData, 0, len(sessions))
	now := time.Now()
	removedPorts := map[int]struct{}{}
	for _, session := range sessions {
		lastRequest := getString(session, "last_request")
		if lastRequest == "" {
			continue
		}
		lastTime, err := time.Parse("2006-01-02T15:04:05.000", lastRequest)
		if err != nil {
			continue
		}
		if now.Sub(lastTime) < 60*time.Second {
			active = append(active, session)
		} else {
			a.recordSessionEnd(session, "inactive_timeout")
			if port, err := strconv.Atoi(getString(session, "x_forwarded_port")); err == nil {
				removedPorts[port] = struct{}{}
			}
			// session removed from active list — no separate cleanup needed
		}
	}
	a.saveSessionList(active)
	for port := range removedPorts {
		a.disablePatternForPort(port)
		a.armTransportFaultLoop(port, "none", 1, transportUnitsSeconds, 0)
	}
	// Auto-ungroup single-member groups
	groupMembers := map[string][]string{}
	for _, session := range active {
		gid := getString(session, "group_id")
		if gid != "" {
			groupMembers[gid] = append(groupMembers[gid], getString(session, "session_id"))
		}
	}
	for _, members := range groupMembers {
		if len(members) == 1 {
			a.saveSessionByID(members[0], SessionData{
				"session_id": members[0],
				"group_id":   "",
			})
		}
	}
}

func writeJSON(w http.ResponseWriter, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	enc := json.NewEncoder(w)
	_ = enc.Encode(payload)
}

func getString(m map[string]interface{}, key string) string {
	if val, ok := m[key]; ok {
		switch v := val.(type) {
		case string:
			return v
		case []byte:
			return string(v)
		case float64:
			if v == math.Trunc(v) {
				return fmt.Sprintf("%d", int(v))
			}
			return fmt.Sprintf("%g", v)
		case int:
			return fmt.Sprintf("%d", v)
		}
	}
	return ""
}

func getBool(m map[string]interface{}, key string) bool {
	if val, ok := m[key]; ok {
		switch v := val.(type) {
		case bool:
			return v
		case string:
			return v == "true"
		case float64:
			return v != 0
		}
	}
	return false
}

func sessionHasPatternSteps(m map[string]interface{}) bool {
	val, ok := m["nftables_pattern_steps"]
	if !ok || val == nil {
		return false
	}
	switch v := val.(type) {
	case []NftShapeStep:
		return len(v) > 0
	case []interface{}:
		return len(v) > 0
	}
	return false
}

func getNumber(m map[string]interface{}, key string) interface{} {
	if val, ok := m[key]; ok {
		switch val.(type) {
		case float64, int, int64:
			return val
		}
	}
	return nil
}

func getFloat(m map[string]interface{}, key string) float64 {
	if val, ok := m[key]; ok {
		switch v := val.(type) {
		case float64:
			return v
		case int:
			return float64(v)
		case int64:
			return float64(v)
		case string:
			f, _ := strconv.ParseFloat(v, 64)
			return f
		}
	}
	return 0
}

func getInt(m map[string]interface{}, key string) int {
	if val, ok := m[key]; ok {
		switch v := val.(type) {
		case int:
			return v
		case int64:
			return int(v)
		case float64:
			return int(v)
		case string:
			i, _ := strconv.Atoi(v)
			return i
		}
	}
	return 0
}

func getStringSlice(m map[string]interface{}, key string) []string {
	val, ok := m[key]
	if !ok || val == nil {
		return nil
	}
	if slice, ok := val.([]string); ok {
		return slice
	}
	if raw, ok := val.([]interface{}); ok {
		list := make([]string, 0, len(raw))
		for _, item := range raw {
			if s, ok := item.(string); ok {
				list = append(list, s)
			}
		}
		return list
	}
	return nil
}

func getManifestVariants(session SessionData) []PlaylistInfo {
	val, ok := session["manifest_variants"]
	if !ok || val == nil {
		return nil
	}
	bytes, err := json.Marshal(val)
	if err != nil {
		return nil
	}
	var infos []PlaylistInfo
	if err := json.Unmarshal(bytes, &infos); err != nil {
		return nil
	}
	return infos
}

func inferServerVideoRendition(session SessionData, filename string, isManifest, isSegment bool) {
	if !(isManifest || isSegment) {
		return
	}
	decoded := filename
	if unescaped, err := url.PathUnescape(filename); err == nil && unescaped != "" {
		decoded = unescaped
	}
	rawParent := pathParent(decoded)
	variantLabel := rawParent
	if isManifest {
		base := pathBase(decoded)
		if variantLabel == "" && strings.HasPrefix(strings.ToLower(base), "playlist_") {
			variantLabel = strings.TrimSuffix(base, ".m3u8")
		}
	}
	if variantLabel == "" {
		return
	}

	variants := getManifestVariants(session)
	if len(variants) == 0 {
		return
	}

	bestIdx := -1
	bestScore := -1
	for idx, variant := range variants {
		score := 0
		variantURL := variant.URL
		variantPath := variantURL
		if parsed, err := url.Parse(variantURL); err == nil {
			if parsed.Path != "" {
				variantPath = parsed.Path
			}
		}
		variantPath = strings.TrimPrefix(variantPath, "/")
		variantParent := pathParent(variantPath)
		variantBase := pathBase(variantPath)

		if variantPath != "" && strings.Contains(decoded, variantPath) {
			score += 8
		}
		if variantParent != "" && strings.Contains(decoded, "/"+variantParent+"/") {
			score += 5
		}
		if variantBase != "" && strings.Contains(decoded, variantBase) {
			score += 3
		}
		if variantParent != "" && variantParent == variantLabel {
			score += 6
		}
		if strings.HasPrefix(strings.ToLower(variantBase), "playlist_") {
			token := strings.TrimSuffix(strings.TrimPrefix(variantBase, "playlist_"), ".m3u8")
			if token != "" && strings.Contains(decoded, "/"+token+"/") {
				score += 4
			}
		}

		if score > bestScore {
			bestScore = score
			bestIdx = idx
		}
	}

	if bestIdx >= 0 && bestScore > 0 {
		selected := variants[bestIdx]
		session["server_video_rendition"] = variantLabel
		session["server_video_rendition_at"] = nowISO()
		if selected.Bandwidth > 0 {
			session["server_video_rendition_mbps"] = math.Round((float64(selected.Bandwidth)/1_000_000)*1000) / 1000
			session["server_video_rendition_bandwidth"] = selected.Bandwidth
		}
		if selected.Resolution != "" {
			session["server_video_rendition_resolution"] = selected.Resolution
		}
		if selected.URL != "" {
			session["server_video_rendition_url"] = selected.URL
		}
	}
}

func bestVariantMbps(session SessionData) float64 {
	variants := getManifestVariants(session)
	if len(variants) == 0 {
		return 0
	}
	maxBandwidth := 0
	for _, variant := range variants {
		if variant.Bandwidth > maxBandwidth {
			maxBandwidth = variant.Bandwidth
		}
	}
	if maxBandwidth <= 0 {
		return 0
	}
	return float64(maxBandwidth) / 1_000_000
}

func nowISO() string {
	return time.Now().Format("2006-01-02T15:04:05.000")
}

func thirdFromLastDigit(port string) string {
	if len(port) < 3 {
		return ""
	}
	return string(port[len(port)-3])
}

func replaceThirdFromLastDigit(port string, replacement int) string {
	if len(port) < 3 {
		return port
	}
	chars := []rune(port)
	chars[len(chars)-3] = rune('0' + replacement)
	return string(chars)
}

func allocateSessionNumber(sessions []SessionData, max int) int {
	used := map[int]bool{}
	for _, session := range sessions {
		id := getString(session, "session_id")
		if len(id) > 0 {
			last := id[len(id)-1]
			if last >= '0' && last <= '9' {
				used[int(last-'0')] = true
			}
		}
	}
	for i := 1; i <= max; i++ {
		if !used[i] {
			return i
		}
	}
	return 1
}

func extractSegmentSequence(path string) (int, bool) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return 0, false
	}
	if idx := strings.Index(trimmed, "?"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	if slash := strings.LastIndex(trimmed, "/"); slash >= 0 {
		trimmed = trimmed[slash+1:]
	}
	if trimmed == "" {
		return 0, false
	}
	if dot := strings.LastIndex(trimmed, "."); dot > 0 {
		trimmed = trimmed[:dot]
	}
	if trimmed == "" {
		return 0, false
	}
	tokens := segmentSequenceDigitsRegex.FindAllString(trimmed, -1)
	if len(tokens) == 0 {
		return 0, false
	}
	seq, err := strconv.Atoi(tokens[len(tokens)-1])
	if err != nil || seq < 0 {
		return 0, false
	}
	return seq, true
}

func (a *App) resetServerLoopState(sessionID string) {
	if strings.TrimSpace(sessionID) == "" {
		return
	}
	a.loopStateMu.Lock()
	a.loopStateBySession[sessionID] = ServerLoopState{}
	a.loopStateMu.Unlock()
}

func (a *App) removeServerLoopState(sessionID string) {
	if strings.TrimSpace(sessionID) == "" {
		return
	}
	a.loopStateMu.Lock()
	delete(a.loopStateBySession, sessionID)
	a.loopStateMu.Unlock()
}

func (a *App) observeServerSegmentLoop(session SessionData, requestPath string) {
	if session == nil {
		return
	}
	pathLower := strings.ToLower(strings.TrimSpace(requestPath))
	if strings.Contains(pathLower, "/audio/") {
		return
	}
	sessionID := getString(session, "session_id")
	if sessionID == "" {
		return
	}
	seq, ok := extractSegmentSequence(requestPath)
	if !ok {
		return
	}

	a.loopStateMu.Lock()
	state := a.loopStateBySession[sessionID]
	prevLast := state.LastSegmentSeq
	prevMax := state.MaxSegmentSeq
	loopDetected := state.MaxSegmentSeq >= 5 && state.LastSegmentSeq > 0 && seq+5 < state.LastSegmentSeq
	if seq > state.MaxSegmentSeq {
		state.MaxSegmentSeq = seq
	}
	state.LastSegmentSeq = seq
	a.loopStateBySession[sessionID] = state
	a.loopStateMu.Unlock()

	log.Printf(
		"LOOP_COUNTER_SERVER_OBS session_id=%s seq=%d prev_last=%d prev_max=%d new_last=%d new_max=%d detected=%t loop_count_server=%d path=%s",
		sessionID,
		seq,
		prevLast,
		prevMax,
		state.LastSegmentSeq,
		state.MaxSegmentSeq,
		loopDetected,
		getInt(session, "loop_count_server"),
		requestPath,
	)

	if loopDetected {
		count := getInt(session, "loop_count_server") + 1
		session["loop_count_server"] = count
		session["loop_count_server_last_at"] = nowISO()
		log.Printf("LOOP_SERVER session_id=%s loop_count_server=%d segment_seq=%d", sessionID, count, seq)
	}
}

func findSessionByPlayerID(sessions []SessionData, ids ...string) SessionData {
	for _, session := range sessions {
		player := getString(session, "player_id")
		headerID := getString(session, "headers_player_id")
		headerAlt := getString(session, "headers_player-ID")
		playbackID := getString(session, "headers_x_playback_session_id")
		for _, id := range ids {
			if id == "" {
				continue
			}
			if player == id || headerID == id || headerAlt == id || playbackID == id {
				return session
			}
		}
	}
	return nil
}

func hostPortOrDefault(hostport, fallback string) string {
	_, port, err := net.SplitHostPort(hostport)
	if err != nil {
		if strings.Contains(hostport, ":") {
			parts := strings.Split(hostport, ":")
			return parts[len(parts)-1]
		}
		return fallback
	}
	return port
}

// shouldScopeSessionsByRequesterIP scopes per-requester sessions on
// public-facing deployments (set via INFINITE_STREAM_PUBLIC_HOST). Off by
// default so single-tenant local installs see all sessions.
func shouldScopeSessionsByRequesterIP(r *http.Request) bool {
	publicHost := strings.ToLower(strings.TrimSpace(os.Getenv("INFINITE_STREAM_PUBLIC_HOST")))
	if publicHost == "" {
		return false
	}
	return strings.ToLower(hostWithoutPort(r.Host)) == publicHost
}

func filterSessionsByPlayerID(sessions []SessionData, playerID string) []SessionData {
	if playerID == "" {
		return sessions
	}
	filtered := make([]SessionData, 0, 1)
	for _, session := range sessions {
		if getString(session, "player_id") == playerID {
			filtered = append(filtered, session)
		}
	}
	return filtered
}

func buildActiveSessionsSummary(sessions []SessionData) []ActiveSessionInfo {
	out := make([]ActiveSessionInfo, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, ActiveSessionInfo{
			SessionID: getString(s, "session_id"),
			PlayerID:  getString(s, "player_id"),
			GroupID:   getString(s, "group_id"),
			Port:      getString(s, "x_forwarded_port_external"),
		})
	}
	return out
}

func filterSessionsByOriginationIP(sessions []SessionData, requesterIP string) []SessionData {
	requesterIP = strings.TrimSpace(requesterIP)
	if requesterIP == "" {
		return []SessionData{}
	}
	filtered := make([]SessionData, 0, len(sessions))
	for _, session := range sessions {
		if strings.TrimSpace(getString(session, "origination_ip")) == requesterIP {
			filtered = append(filtered, session)
		}
	}
	return filtered
}

func countActiveSessionsForIP(sessions []SessionData, requesterIP string) int {
	requesterIP = strings.TrimSpace(requesterIP)
	if requesterIP == "" {
		return 0
	}
	count := 0
	for _, session := range sessions {
		originIP := strings.TrimSpace(getString(session, "origination_ip"))
		if originIP == requesterIP {
			count++
			continue
		}
		playerIP := strings.TrimSpace(getString(session, "player_ip"))
		if playerIP == requesterIP {
			count++
		}
	}
	return count
}

func hostWithoutPort(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		if strings.Contains(hostport, ":") {
			parts := strings.Split(hostport, ":")
			return parts[0]
		}
		return hostport
	}
	return host
}

func remoteIP(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// extractClientIP extracts the client IP considering X-Forwarded-For header
// Note: X-Forwarded-For can be spoofed by clients. This function assumes
// the application is deployed behind a trusted reverse proxy (nginx).
// For production use, configure the trusted proxy to strip client-provided
// X-Forwarded-For headers and only use the proxy-set value.
func extractClientIP(remoteAddr, xForwardedFor string) string {
	clientIP := ""
	// First, check X-Forwarded-For header (takes precedence when behind trusted proxy)
	if xForwardedFor != "" {
		parts := strings.Split(xForwardedFor, ",")
		if len(parts) > 0 {
			clientIP = strings.TrimSpace(parts[0])
		}
	}
	// Fallback to RemoteAddr
	if clientIP == "" {
		host, _, err := net.SplitHostPort(remoteAddr)
		if err == nil {
			clientIP = host
		} else {
			clientIP = remoteAddr
		}
	}
	return clientIP
}

// isExternalIP determines if an IP address is external (not private, loopback, etc.)
// Returns false for invalid IPs and logs them for debugging
func isExternalIP(ip string) bool {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		// Log invalid IP addresses for debugging
		if ip != "" && ip != "unknown" {
			log.Printf("[GO-PROXY][WARN] Invalid IP address for external check: %q", ip)
		}
		return false
	}
	if parsed.IsLoopback() || parsed.IsUnspecified() || parsed.IsLinkLocalUnicast() || parsed.IsLinkLocalMulticast() {
		return false
	}
	if parsed.IsPrivate() {
		return false
	}
	return true
}

func pathBase(path string) string {
	parts := strings.Split(path, "/")
	return parts[len(parts)-1]
}

func pathParent(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2]
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getenvAny(keys []string, fallback string) string {
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func getenvIntAny(keys []string, fallback int) int {
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			parsed, err := strconv.Atoi(value)
			if err == nil {
				return parsed
			}
		}
	}
	return fallback
}

func getenvBoolAny(keys []string, fallback bool) bool {
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			switch strings.TrimSpace(strings.ToLower(value)) {
			case "1", "true", "yes", "y", "on":
				return true
			case "0", "false", "no", "n", "off":
				return false
			default:
				return fallback
			}
		}
	}
	return fallback
}

type FailureHandler struct {
	failureType      string
	failureUnits     string
	consecutiveUnits string
	frequencyUnits   string
	failureFrequency int
	consecutive      int
	failureAt        interface{}
	failureRecoverAt interface{}
	resetFailureType interface{}
}

// refreshFailureStateFromLatest copies the fault-decision-relevant
// fields from the latest published session snapshot onto the
// per-request `dst` clone. Called inside `sessionStateMu` so the
// snapshot and the read are consistent: every goroutine entering
// the failure-decision critical section sees the same view of
// `<prefix>_failure_at` / `<prefix>_failure_recover_at`,
// regardless of when its outer clone was taken.
func refreshFailureStateFromLatest(a *App, dst SessionData, sessionID string) {
	if a == nil || dst == nil || sessionID == "" {
		return
	}
	latest := a.getSessionList()
	for _, s := range latest {
		if getString(s, "session_id") != sessionID {
			continue
		}
		// Counters and timestamps that gate the dedup. Order matters
		// only in the sense that all of these need to come from the
		// same snapshot.
		for _, k := range []string{
			"segments_count", "manifest_requests_count", "master_manifest_requests_count",
			"all_requests_count",
			"segment_failure_at", "segment_failure_recover_at",
			"manifest_failure_at", "manifest_failure_recover_at",
			"master_manifest_failure_at", "master_manifest_failure_recover_at",
			"all_failure_at", "all_failure_recover_at",
		} {
			if v, ok := s[k]; ok {
				dst[k] = v
			}
		}
		return
	}
}

func NewFailureHandler(prefix string, session SessionData) *FailureHandler {
	failureUnits := getString(session, prefix+"_failure_units")
	consecutiveUnits := getString(session, prefix+"_consecutive_units")
	frequencyUnits := getString(session, prefix+"_frequency_units")
	if consecutiveUnits == "" {
		consecutiveUnits = failureUnits
	}
	if frequencyUnits == "" {
		frequencyUnits = failureUnits
	}
	// Defaults match the dashboard's visible default Mode
	// ("Failures / Seconds"), which maps to consecutiveUnits=requests
	// and frequencyUnits=seconds. The dashboard only PATCHes Mode
	// when the user actively changes it, so a session whose
	// `<prefix>_failure_mode` field was never set still has empty
	// units here. Defaulting to the same shape as the visible Mode
	// avoids "rate limit doesn't fire as expected" on first use.
	if consecutiveUnits == "" {
		consecutiveUnits = "requests"
	}
	if frequencyUnits == "" {
		frequencyUnits = "seconds"
	}
	resetFailureType := session[prefix+"_reset_failure_type"]
	if resetString, ok := resetFailureType.(string); ok {
		resetFailureType = normalizeRequestFailureType(resetString)
	}
	return &FailureHandler{
		failureType:      normalizeRequestFailureType(getString(session, prefix+"_failure_type")),
		failureUnits:     failureUnits,
		consecutiveUnits: consecutiveUnits,
		frequencyUnits:   frequencyUnits,
		failureFrequency: getInt(session, prefix+"_failure_frequency"),
		consecutive:      getInt(session, prefix+"_consecutive_failures"),
		failureAt:        session[prefix+"_failure_at"],
		failureRecoverAt: session[prefix+"_failure_recover_at"],
		resetFailureType: resetFailureType,
	}
}

func (h *FailureHandler) HandleFailure(count int, now time.Time) string {
	if h.failureType == "" {
		h.failureType = "none"
	}
	if h.failureType == "none" {
		return "none"
	}
	if h.frequencyUnits == "seconds" {
		h.handleFailureTime(count, now)
	} else {
		h.handleFailureCount(count, now)
	}
	return h.failureType
}

func (h *FailureHandler) handleFailureCount(count int, now time.Time) {
	if h.consecutive <= 0 {
		return
	}
	if h.failureAt == nil {
		h.failureAt = count
	}
	failureAt := intFromInterface(h.failureAt)
	if count < failureAt {
		h.failureType = "none"
		return
	}
	if h.consecutiveUnits == "seconds" {
		if h.failureRecoverAt == nil {
			h.failureRecoverAt = now.Add(time.Duration(h.consecutive) * time.Second).Format("2006-01-02T15:04:05.000")
			return
		}
		failureRecover := timeFromInterface(h.failureRecoverAt)
		if now.Before(failureRecover) {
			return
		}
		if h.failureFrequency > 0 {
			// Mixed-units case (counts overall, seconds on-window).
			// Schedule next fault `freq` requests from now — the on-
			// window cost in counts is unknown so we don't subtract.
			h.failureAt = count + h.failureFrequency
			h.failureType = "none"
			h.failureRecoverAt = nil
			return
		}
		h.failureType = "none"
		h.resetFailureType = "none"
		h.failureRecoverAt = nil
		h.failureAt = nil
		return
	}
	if h.failureRecoverAt == nil {
		h.failureRecoverAt = count + h.consecutive
		return
	}
	failureRecover := intFromInterface(h.failureRecoverAt)
	if count < failureRecover {
		return
	}
	if h.failureFrequency > 0 {
		// Frequency = full cycle length (fault start → next fault
		// start). Subtract the on-window in counts so the gap after
		// recovery makes the cycle exactly `freq` requests. Clamp ≥0
		// in case the user set freq < consec.
		gap := h.failureFrequency - h.consecutive
		if gap < 0 {
			gap = 0
		}
		h.failureAt = count + gap
		h.failureType = "none"
		h.failureRecoverAt = nil
		return
	}
	h.failureType = "none"
	h.resetFailureType = "none"
	h.failureRecoverAt = nil
	h.failureAt = nil
}

func (h *FailureHandler) handleFailureTime(count int, now time.Time) {
	if h.consecutive <= 0 {
		return
	}
	if h.failureAt == nil {
		h.failureAt = nowISO()
	}
	failureAt := timeFromInterface(h.failureAt)
	if now.Before(failureAt) {
		h.failureType = "none"
		return
	}
	if h.consecutiveUnits == "seconds" {
		if h.failureRecoverAt == nil {
			h.failureRecoverAt = now.Add(time.Duration(h.consecutive) * time.Second).Format("2006-01-02T15:04:05.000")
			return
		}
		failureRecover := timeFromInterface(h.failureRecoverAt)
		if now.Before(failureRecover) {
			return
		}
		if h.failureFrequency > 0 {
			// Frequency = full cycle length (fault start → next fault
			// start). Subtract the on-window so the gap after recovery
			// makes the cycle exactly `freq` seconds. Clamp ≥0 in case
			// the user set freq < consec (would mean continuous fault).
			gapSec := h.failureFrequency - h.consecutive
			if gapSec < 0 {
				gapSec = 0
			}
			h.failureAt = now.Add(time.Duration(gapSec) * time.Second).Format("2006-01-02T15:04:05.000")
			h.failureType = "none"
			h.failureRecoverAt = nil
			return
		}
		h.failureType = "none"
		h.resetFailureType = "none"
		h.failureRecoverAt = nil
		h.failureAt = nil
		return
	}
	if h.failureRecoverAt == nil {
		h.failureRecoverAt = count + h.consecutive
		return
	}
	failureRecover := intFromInterface(h.failureRecoverAt)
	if count < failureRecover {
		return
	}
	if h.failureFrequency > 0 {
		// Mixed-units case: fault on-window is counts but the gap
		// scheduling is in seconds. Keep the existing semantics —
		// next fault scheduled `freq` seconds from recovery wallclock.
		h.failureAt = now.Add(time.Duration(h.failureFrequency) * time.Second).Format("2006-01-02T15:04:05.000")
		h.failureType = "none"
		h.failureRecoverAt = nil
		return
	}
	h.failureType = "none"
	h.resetFailureType = "none"
	h.failureRecoverAt = nil
	h.failureAt = nil
}

func intFromInterface(val interface{}) int {
	switch v := val.(type) {
	case int:
		return v
	case float64:
		return int(v)
	case string:
		i, _ := strconv.Atoi(v)
		return i
	}
	return 0
}

func int64FromInterface(val interface{}) int64 {
	switch v := val.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	case string:
		i, _ := strconv.ParseInt(v, 10, 64)
		return i
	}
	return 0
}

func floatFromInterface(val interface{}) float64 {
	switch v := val.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	case json.Number:
		f, _ := v.Float64()
		return f
	}
	return 0
}

func timeFromInterface(val interface{}) time.Time {
	switch v := val.(type) {
	case string:
		parsed, _ := time.Parse("2006-01-02T15:04:05.000", v)
		return parsed
	}
	return time.Time{}
}

// extractGroupId extracts group ID from player_id with pattern _G###
// e.g., "hlsjs_G001" returns "G001", "safari_G001" returns "G001"
func extractGroupId(playerID string) string {
	if playerID == "" {
		return ""
	}
	// Look for _G### pattern
	matches := segmentGroupRegex.FindStringSubmatch(playerID)
	if len(matches) > 1 {
		return "G" + matches[1]
	}
	return ""
}

// getSessionsByGroupId returns all sessions that belong to the specified group
func (a *App) getSessionsByGroupId(groupID string) []SessionData {
	if groupID == "" {
		return []SessionData{}
	}
	sessions := a.getSessionList()
	var groupSessions []SessionData
	for _, session := range sessions {
		sessionGroupID := getString(session, "group_id")
		if sessionGroupID == groupID {
			groupSessions = append(groupSessions, session)
		}
	}
	return groupSessions
}

// getGroupIdByPort returns the group ID for sessions on the specified port.
// If sessions is nil, fetches from the canonical list.
func (a *App) getGroupIdByPort(port int, sessions ...[]SessionData) string {
	list := a.resolveSessionList(sessions)
	for _, session := range list {
		if !a.sessionMatchesPort(session, port) {
			continue
		}
		groupID := getString(session, "group_id")
		if groupID != "" {
			return groupID
		}
	}
	return ""
}

// getPortsForGroup returns all ports used by sessions in the specified group.
// If sessions is nil, fetches from the canonical list.
func (a *App) getPortsForGroup(groupID string, sessions ...[]SessionData) []int {
	if groupID == "" {
		return []int{}
	}
	list := a.resolveSessionList(sessions)
	portMap := make(map[int]bool)
	for _, session := range list {
		if getString(session, "group_id") == groupID {
			if portStr := getString(session, "x_forwarded_port"); portStr != "" {
				if port, ok := a.sessionPortToInternal(portStr); ok {
					portMap[port] = true
				}
			}
			if portStr := getString(session, "x_forwarded_port_external"); portStr != "" {
				if port, ok := a.sessionPortToInternal(portStr); ok {
					portMap[port] = true
				}
			}
		}
	}
	var ports []int
	for port := range portMap {
		ports = append(ports, port)
	}
	return ports
}

func (a *App) resolveSessionList(sessions [][]SessionData) []SessionData {
	if len(sessions) > 0 && sessions[0] != nil {
		return sessions[0]
	}
	return a.getSessionList()
}

// updateSessionGroup updates all sessions in a group with the given updates
func (a *App) updateSessionGroup(groupID string, updates map[string]interface{}) {
	if groupID == "" {
		return
	}
	sessions := a.getSessionList()
	changed := false
	for _, session := range sessions {
		sessionGroupID := getString(session, "group_id")
		if sessionGroupID == groupID {
			for key, value := range updates {
				session[key] = value
			}
			changed = true
		}
	}
	if changed {
		a.saveSessionList(sessions)
	}
}

type RequestHandler struct {
	mode       string
	session    SessionData
	failureKey string
}

func NewRequestHandler(isSegment, isUpdateManifest, isMasterManifest bool, session SessionData) *RequestHandler {
	if isSegment {
		return &RequestHandler{mode: "segment", session: session}
	}
	if isMasterManifest {
		return &RequestHandler{mode: "master_manifest", session: session}
	}
	if isUpdateManifest {
		return &RequestHandler{mode: "manifest", session: session}
	}
	return &RequestHandler{mode: "unknown", session: session}
}

func (h *RequestHandler) HandleRequest(filename string) string {
	// "All" override — when set, every HTTP request runs through the
	// single all-rule and the per-kind tabs (segment/manifest/master)
	// are bypassed. The dashboard reflects this by disabling those
	// tabs and showing an "All override active" banner.
	if getString(h.session, "all_failure_type") != "" &&
		getString(h.session, "all_failure_type") != "none" {
		return h.handleAllFailure(filename)
	}
	switch h.mode {
	case "segment":
		return h.handleSegmentFailure(filename)
	case "manifest":
		return h.handleManifestFailure(filename)
	case "master_manifest":
		return h.handleFailure("master_manifest", "master_manifest_requests_count")
	default:
		return "none"
	}
}

func (h *RequestHandler) handleAllFailure(filename string) string {
	h.session["all_requests_count"] = getInt(h.session, "all_requests_count") + 1
	allURLs := getStringSlice(h.session, "all_failure_urls")
	if len(allURLs) > 0 {
		if !shouldApplyFailure(allURLs, filename, pathParent(filename)) {
			return "none"
		}
	}
	preFailureAt := h.session["all_failure_at"]
	preFailureRecoverAt := h.session["all_failure_recover_at"]
	failure := NewFailureHandler("all", h.session)
	count := getInt(h.session, "all_requests_count")
	failureType := failure.HandleFailure(count, time.Now())
	log.Printf(
		"ALL FAILURE DEBUG count=%d type_in=%s type_out=%s units=%s consecutiveUnits=%s frequencyUnits=%s freq=%d consecutive=%d preFailureAt=%v preFailureRecoverAt=%v postFailureAt=%v postFailureRecoverAt=%v file=%s",
		count,
		getString(h.session, "all_failure_type"),
		failureType,
		failure.failureUnits,
		failure.consecutiveUnits,
		failure.frequencyUnits,
		failure.failureFrequency,
		failure.consecutive,
		preFailureAt,
		preFailureRecoverAt,
		failure.failureAt,
		failure.failureRecoverAt,
		filename,
	)
	h.session["all_failure_at"] = failure.failureAt
	h.session["all_failure_recover_at"] = failure.failureRecoverAt
	if failure.resetFailureType != nil {
		h.session["all_failure_type"] = failure.resetFailureType
		h.session["all_reset_failure_type"] = nil
		h.session["control_revision"] = newControlRevision()
	}
	return failureType
}

func (h *RequestHandler) handleFailure(prefix, countKey string) string {
	count := getInt(h.session, countKey) + 1
	h.session[countKey] = count
	failure := NewFailureHandler(prefix, h.session)
	failureType := failure.HandleFailure(count, time.Now())
	if prefix == "segment" {
		log.Printf(
			"SEGMENT FAILURE DEBUG count=%d type=%s units=%s consecutiveUnits=%s frequencyUnits=%s freq=%d consecutive=%d failureAt=%v recoverAt=%v resetType=%v",
			count,
			failure.failureType,
			failure.failureUnits,
			failure.consecutiveUnits,
			failure.frequencyUnits,
			failure.failureFrequency,
			failure.consecutive,
			failure.failureAt,
			failure.failureRecoverAt,
			failure.resetFailureType,
		)
	}
	h.session[prefix+"_failure_at"] = failure.failureAt
	h.session[prefix+"_failure_recover_at"] = failure.failureRecoverAt
	if failure.resetFailureType != nil {
		h.session[prefix+"_failure_type"] = failure.resetFailureType
		h.session[prefix+"_reset_failure_type"] = nil
		h.session["control_revision"] = newControlRevision()
	}
	return failureType
}

func (h *RequestHandler) handleManifestFailure(filename string) string {
	h.session["manifest_requests_count"] = getInt(h.session, "manifest_requests_count") + 1
	manifestURLs := getStringSlice(h.session, "manifest_failure_urls")
	match := shouldApplyFailure(manifestURLs, filename, pathParent(filename))
	if !match {
		return "none"
	}
	failure := NewFailureHandler("manifest", h.session)
	failureType := failure.HandleFailure(getInt(h.session, "manifest_requests_count"), time.Now())
	h.session["manifest_failure_at"] = failure.failureAt
	h.session["manifest_failure_recover_at"] = failure.failureRecoverAt
	if failure.resetFailureType != nil {
		h.session["manifest_failure_type"] = failure.resetFailureType
		h.session["manifest_reset_failure_type"] = nil
		h.session["control_revision"] = newControlRevision()
	}
	return failureType
}

func (h *RequestHandler) handleSegmentFailure(filename string) string {
	h.session["segments_count"] = getInt(h.session, "segments_count") + 1
	segmentURLs := getStringSlice(h.session, "segment_failure_urls")
	match := shouldApplyFailure(segmentURLs, filename, pathParent(filename))
	if !match {
		return "none"
	}
	failure := NewFailureHandler("segment", h.session)
	failureType := failure.HandleFailure(getInt(h.session, "segments_count"), time.Now())
	log.Printf(
		"SEGMENT FAILURE DEBUG count=%d type=%s units=%s consecutiveUnits=%s frequencyUnits=%s freq=%d consecutive=%d failureAt=%v recoverAt=%v resetType=%v",
		getInt(h.session, "segments_count"),
		failure.failureType,
		failure.failureUnits,
		failure.consecutiveUnits,
		failure.frequencyUnits,
		failure.failureFrequency,
		failure.consecutive,
		failure.failureAt,
		failure.failureRecoverAt,
		failure.resetFailureType,
	)
	h.session["segment_failure_at"] = failure.failureAt
	h.session["segment_failure_recover_at"] = failure.failureRecoverAt
	if failure.resetFailureType != nil {
		h.session["segment_failure_type"] = failure.resetFailureType
		h.session["segment_reset_failure_type"] = nil
		h.session["control_revision"] = newControlRevision()
	}
	return failureType
}

func shouldApplyFailure(entries []string, filename, variant string) bool {
	if len(entries) == 0 {
		return false
	}
	decodedFilename := filename
	if unescaped, err := url.PathUnescape(filename); err == nil {
		decodedFilename = unescaped
	}
	base := pathBase(decodedFilename)
	decodedVariant := variant
	if unescaped, err := url.PathUnescape(variant); err == nil {
		decodedVariant = unescaped
	}
	for _, entry := range entries {
		if entry == "" {
			continue
		}
		decodedEntry := entry
		if unescaped, err := url.PathUnescape(entry); err == nil {
			decodedEntry = unescaped
		}
		entryBase := pathBase(decodedEntry)
		if entryBase == "All" || decodedEntry == "All" {
			return true
		}
		if decodedEntry == decodedVariant || entry == variant {
			return true
		}
		if decodedEntry == base || entry == base {
			return true
		}
		if strings.Contains(decodedFilename, decodedEntry) || strings.Contains(filename, entry) {
			return true
		}
		if strings.Contains(entryBase, "playlist_") {
			trimmed := strings.TrimSuffix(entryBase, ".m3u8")
			parts := strings.Split(trimmed, "_")
			if len(parts) > 0 {
				candidate := parts[len(parts)-1]
				if candidate != "" && strings.Contains(decodedFilename, "/"+candidate+"/") {
					return true
				}
			}
		}
	}
	return false
}

func updateSessionTraffic(session SessionData, bytesIn, bytesOut int64) {
	// Read-modify-write on byte counters; without the lock concurrent
	// requests lose bytes and the derived mbps values jitter.
	// updateSessionTrafficAverages also writes to the same map under
	// this lock — fine, it's called from within this critical section
	// and never recursively re-enters bumpFaultCounter / itself.
	sessionStateMu.Lock()
	defer sessionStateMu.Unlock()
	now := time.Now()
	totalIn := int64FromInterface(session["bytes_in_total"]) + bytesIn
	totalOut := int64FromInterface(session["bytes_out_total"]) + bytesOut
	lastIn := int64FromInterface(session["bytes_in_last"])
	lastOut := int64FromInterface(session["bytes_out_last"])
	lastTs := int64FromInterface(session["bytes_last_ts"])
	if lastTs > 0 {
		deltaTime := now.Sub(time.Unix(lastTs, 0)).Seconds()
		if deltaTime > 0 {
			mbpsIn := (float64(totalIn-lastIn) * 8) / (deltaTime * 1024 * 1024)
			mbpsOut := (float64(totalOut-lastOut) * 8) / (deltaTime * 1024 * 1024)
			session["mbps_in"] = math.Round(mbpsIn*100) / 100
			session["mbps_out"] = math.Round(mbpsOut*100) / 100
			session["measurement_window_io"] = math.Round(deltaTime*10) / 10
		}
	}
	session["bytes_in_total"] = totalIn
	session["bytes_out_total"] = totalOut
	session["bytes_in_last"] = totalIn
	session["bytes_out_last"] = totalOut
	session["bytes_last_ts"] = now.Unix()
	updateSessionTrafficAverages(session, totalIn, totalOut, now)
	log.Printf("SESSIONNET bytes_in=%d bytes_out=%d mbps_in=%v mbps_out=%v window=%v",
		bytesIn,
		bytesOut,
		session["mbps_in"],
		session["mbps_out"],
		session["measurement_window_io"],
	)
}

func updateSessionTrafficAverages(session SessionData, totalIn, totalOut int64, now time.Time) {
	const windowSeconds = 18
	const shortWindowSeconds = 1
	cutoff := now.Add(-time.Duration(windowSeconds) * time.Second).Unix()
	shortCutoff := now.Add(-time.Duration(shortWindowSeconds) * time.Second).Unix()
	samples := make([]map[string]interface{}, 0)
	if raw, ok := session["io_samples"]; ok && raw != nil {
		switch v := raw.(type) {
		case []map[string]interface{}:
			samples = v
		case []interface{}:
			for _, item := range v {
				if m, ok := item.(map[string]interface{}); ok {
					samples = append(samples, m)
				}
			}
		}
	}
	var prevSample map[string]interface{}
	if len(samples) > 0 {
		prevSample = samples[len(samples)-1]
	}
	activeSamples := make([]map[string]interface{}, 0)
	if raw, ok := session["active_io_samples"]; ok && raw != nil {
		switch v := raw.(type) {
		case []map[string]interface{}:
			activeSamples = v
		case []interface{}:
			for _, item := range v {
				if m, ok := item.(map[string]interface{}); ok {
					activeSamples = append(activeSamples, m)
				}
			}
		}
	}
	if prevSample != nil {
		prevTs := int64FromInterface(prevSample["ts"])
		nowTs := now.Unix()
		if nowTs > prevTs {
			deltaTime := float64(nowTs - prevTs)
			deltaIn := totalIn - int64FromInterface(prevSample["in"])
			deltaOut := totalOut - int64FromInterface(prevSample["out"])
			if deltaIn > 0 || deltaOut > 0 {
				activeSamples = append(activeSamples, map[string]interface{}{
					"ts":  nowTs,
					"dt":  deltaTime,
					"in":  deltaIn,
					"out": deltaOut,
				})
			}
		}
	}
	filtered := make([]map[string]interface{}, 0, len(samples))
	filteredShort := make([]map[string]interface{}, 0, len(samples))
	for _, sample := range samples {
		ts := int64FromInterface(sample["ts"])
		if ts >= cutoff {
			filtered = append(filtered, sample)
		}
		if ts >= shortCutoff {
			filteredShort = append(filteredShort, sample)
		}
	}
	if len(filtered) > 120 {
		filtered = filtered[len(filtered)-120:]
	}
	session["io_samples"] = filtered

	filteredActive := make([]map[string]interface{}, 0, len(activeSamples))
	for _, sample := range activeSamples {
		ts := int64FromInterface(sample["ts"])
		if ts >= cutoff {
			filteredActive = append(filteredActive, sample)
		}
	}
	if len(filteredActive) > 120 {
		filteredActive = filteredActive[len(filteredActive)-120:]
	}
	session["active_io_samples"] = filteredActive

	if len(filtered) >= 2 {
		oldest := filtered[0]
		newest := filtered[len(filtered)-1]
		oldTs := int64FromInterface(oldest["ts"])
		newTs := int64FromInterface(newest["ts"])
		if newTs > oldTs {
			deltaTime := float64(newTs - oldTs)
			deltaIn := int64FromInterface(newest["in"]) - int64FromInterface(oldest["in"])
			deltaOut := int64FromInterface(newest["out"]) - int64FromInterface(oldest["out"])
			session["measurement_window_io"] = math.Round(deltaTime*10) / 10
			if deltaTime >= 12 {
				mbpsInAvg := (float64(deltaIn) * 8) / (deltaTime * 1024 * 1024)
				mbpsOutAvg := (float64(deltaOut) * 8) / (deltaTime * 1024 * 1024)
				session["mbps_in_avg"] = math.Round(mbpsInAvg*100) / 100
				session["mbps_out_avg"] = math.Round(mbpsOutAvg*100) / 100
			} else {
				session["mbps_in_avg"] = nil
				session["mbps_out_avg"] = nil
			}
		}
	}

	if len(filteredShort) >= 2 {
		oldest := filteredShort[0]
		newest := filteredShort[len(filteredShort)-1]
		oldTs := int64FromInterface(oldest["ts"])
		newTs := int64FromInterface(newest["ts"])
		if newTs > oldTs {
			deltaTime := float64(newTs - oldTs)
			deltaIn := int64FromInterface(newest["in"]) - int64FromInterface(oldest["in"])
			deltaOut := int64FromInterface(newest["out"]) - int64FromInterface(oldest["out"])
			session["measurement_window_io_1s"] = math.Round(deltaTime*100) / 100
			if deltaTime >= 1 {
				mbpsInAvg := (float64(deltaIn) * 8) / (deltaTime * 1024 * 1024)
				mbpsOutAvg := (float64(deltaOut) * 8) / (deltaTime * 1024 * 1024)
				session["mbps_in_1s"] = math.Round(mbpsInAvg*100) / 100
				session["mbps_out_1s"] = math.Round(mbpsOutAvg*100) / 100
			} else {
				session["mbps_in_1s"] = nil
				session["mbps_out_1s"] = nil
			}
		}
	}

	if len(filteredActive) >= 2 {
		var sumDt float64
		var sumIn int64
		var sumOut int64
		for _, sample := range filteredActive {
			sumDt += floatFromInterface(sample["dt"])
			sumIn += int64FromInterface(sample["in"])
			sumOut += int64FromInterface(sample["out"])
		}
		session["measurement_window_active"] = math.Round(sumDt*10) / 10
		if sumDt >= 12 {
			mbpsInActive := (float64(sumIn) * 8) / (sumDt * 1024 * 1024)
			mbpsOutActive := (float64(sumOut) * 8) / (sumDt * 1024 * 1024)
			session["mbps_in_active"] = math.Round(mbpsInActive*100) / 100
			session["mbps_out_active"] = math.Round(mbpsOutActive*100) / 100
		} else {
			session["mbps_in_active"] = nil
			session["mbps_out_active"] = nil
		}
	}
}
