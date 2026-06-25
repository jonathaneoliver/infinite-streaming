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

	v2server "github.com/jonathaneoliver/infinite-streaming/go-proxy/internal/v2/server"
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
	// The player generates a fresh UUID at every content-selection
	// boundary (new video / fresh page-load / app launch) and passes
	// it as `?play_id=...` on every URL. Stable across in-app restart
	// events (user-reload, auto-recovery). Issue #280.
	PlayID string `json:"play_id,omitempty"`

	// AttemptID identifies which playback attempt within a play this
	// request belongs to. The player initialises it to 1 on every
	// new play and increments by 1 at every `restart` event
	// (user-reload OR auto-recovery), passing it as `?attempt_id=N`
	// on every URL. PlayID stays the same across these attempts;
	// AttemptID ticks up. Lets analytics ask both "how did this play
	// perform" (group by play_id) and "how many recovery attempts
	// within the play" (max attempt_id GROUP BY play_id).
	AttemptID uint32 `json:"attempt_id,omitempty"`

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
	// #740: the in-memory session list is now mutated lock-free via
	// mutateSessions (immutable copy-on-write + CompareAndSwap). The former
	// sessionsMu (guarded only the publish, never the read-modify-write) and
	// createMu (#739 bootstrap-allocation lock, subsumed by reserve-then-fill)
	// are gone — see mutateSessions and the handleProxy reserve CAS.
	sessionsSnap   atomic.Pointer[[]SessionData]
	throughputMu   sync.RWMutex
	throughputData map[int]map[string]interface{}
	sessionEvents  *SessionEventStore
	traffic        *TcTrafficManager
	upstreamHost   string
	upstreamPort   string
	maxSessions    int
	// defaultRateMbps is the baseline rate cap (Mbps) applied to every
	// new player session via setDefault on `nftables_bandwidth_mbps` in
	// normalizeSessionsForResponse. Read once at boot from
	// INFINITE_STREAM_DEFAULT_RATE_MBPS. 0 = no cap (today's behaviour);
	// non-zero = the deployment's interpretation of "no operator
	// override." See issue #480.
	defaultRateMbps    int
	client             *http.Client
	portMap            PortMapping
	shapeMu            sync.Mutex
	shapeLoops         map[int]context.CancelFunc
	shapeStates        map[int]NftShapePattern
	shapeApplyMu       sync.Mutex
	shapeApply         map[int]ShapeApplyState
	faultMu            sync.Mutex
	faultLoops         map[int]context.CancelFunc
	networkLogsMu      sync.RWMutex
	networkLogs        map[string]*NetworkLogRingBuffer // sessionId -> ring buffer
	loopStateMu        sync.Mutex
	loopStateBySession map[string]ServerLoopState
	sessionsHub        *SessionEventHub
	// Monotonic revision stamped on each /api/sessions/stream frame
	// (handleSessionStream initial frame + emitSessionEvent per-event
	// frames). Was named sessionsBroadcastSeq when the debounced
	// full-state broadcast lived here; kept stable as the wire
	// revision is consumer-visible.
	sessionsBroadcastSeq uint64
	networkHub           *NetworkEventHub
	// controlHub broadcasts proxy/harness control events to subscribers
	// (forwarder + any dashboard SSE client). Issue #474 Milestone B.
	controlHub *ControlEventHub
	// avmetricsHub broadcasts iOS 18 AVMetrics raw events posted by the
	// player to dashboard + forwarder subscribers. Issue #486 spike.
	avmetricsHub         *AVMetricEventHub
	uiStateVersionSeq    uint64
	segmentFlightMu      sync.Mutex
	segmentFlight        map[int]segmentFlightInfo // internal port -> segment transfer info
	segmentFlightSeq     uint64                    // atomic generation counter for flight IDs
	segmentRunMu         sync.Mutex
	segmentRun           map[int]segmentRunRecord // internal port -> last completed run record
	drainActiveMu        sync.Mutex
	drainActive          map[int]bool // per-port: true while awaitSocketDrain is running
	tcSamplesMu          sync.Mutex
	tcSamples            map[int][]tcSample
	wireRateMu           sync.Mutex
	wireRate             map[int]wireRateSample // latest byte-change-gated rate per port
	tcCacheMu            sync.Mutex
	tcCache              map[int]*tcStatsCache // per-port TC stats cache
	transferCompleteMu   sync.Mutex
	transferCompleteMbps map[int]float64   // latest completed segment Mbps per port
	transferCompleteAt   map[int]time.Time // when the drain completed
	// metricsPostMu serialises `handlePostSessionMetrics` per session_id.
	// Without this, two near-simultaneous POSTs run in independent
	// goroutines that race for `sessionsMu`; the loser writes after the
	// winner and would clobber a fresher event_time with stale (the
	// stale-guard in saveSessionByID prevents the bad outcome but loses
	// the older POST's data entirely). Per-session mutex preserves
	// arrival order — Go's sync.Mutex grants in approximately FIFO
	// order under contention since 1.9, which is what TCP delivery
	// guarantees per connection. Issue #403 follow-up.
	metricsPostMu sync.Map // session_id -> *sync.Mutex
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

// ControlEventHub fans out control_events to dashboard + forwarder
// subscribers. Same drop-oldest policy as NetworkEventHub (slow
// clients lose old events rather than blocking the proxy hot path).
// Issue #474 Milestone B.
type ControlEventHub struct {
	mu      sync.Mutex
	nextID  int
	clients map[int]*ControlClient
	// lastServerStart holds the most recent server_start boot marker so it
	// can be replayed to clients that subscribe AFTER boot. The restart
	// that produces the marker is the same event that drops the forwarder's
	// SSE subscription, and Broadcast to zero clients is a no-op — so
	// without replay the marker would be lost before the forwarder
	// reconnects. Set via BroadcastServerStart, replayed in AddClient. #671.
	lastServerStart *ControlEvent
}

type ControlClient struct {
	ch      chan ControlEvent
	dropped uint64
}

// ControlEvent is one emitted action. JSON-tagged for the SSE body —
// the forwarder's ctrlEnt mirrors this shape.
type ControlEvent struct {
	Ts        time.Time `json:"ts"`
	SessionID string    `json:"session_id"`
	PlayerID  string    `json:"player_id"`
	PlayID    string    `json:"play_id"`
	AttemptID uint32    `json:"attempt_id"`
	Source    string    `json:"source"`
	Event     string    `json:"event"`
	Info      string    `json:"info"`
}

func NewControlEventHub() *ControlEventHub {
	return &ControlEventHub{clients: map[int]*ControlClient{}}
}

func (h *ControlEventHub) AddClient(buffer int) (int, <-chan ControlEvent) {
	if buffer <= 0 {
		buffer = 256
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	id := h.nextID
	c := &ControlClient{ch: make(chan ControlEvent, buffer)}
	h.clients[id] = c
	// Replay the sticky boot marker so a forwarder reconnecting after a
	// restart still archives the server_start. The channel was just created
	// with room, so this never blocks. #671.
	if h.lastServerStart != nil {
		select {
		case c.ch <- *h.lastServerStart:
		default:
		}
	}
	return id, c.ch
}

func (h *ControlEventHub) RemoveClient(id int) {
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

func (h *ControlEventHub) Broadcast(ev ControlEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) == 0 {
		return
	}
	for _, c := range h.clients {
		select {
		case c.ch <- ev:
		default:
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

// BroadcastServerStart records the boot marker as sticky (replayed to every
// future subscriber by AddClient) and broadcasts it to any client already
// connected. Separate from Broadcast so only the boot marker is retained —
// ordinary control events are not stickied. #671.
func (h *ControlEventHub) BroadcastServerStart(ev ControlEvent) {
	h.mu.Lock()
	stored := ev
	h.lastServerStart = &stored
	h.mu.Unlock()
	h.Broadcast(ev)
}

// AVMetricEventHub fans out iOS 18 AVMetrics raw events (issue #486) to
// dashboard + forwarder subscribers. Parallel to ControlEventHub so the
// spike's comparison stream stays separable from existing channels. Same
// drop-oldest fanout policy: slow clients lose old events rather than
// blocking the proxy hot path.
type AVMetricEventHub struct {
	mu      sync.Mutex
	nextID  int
	clients map[int]*AVMetricClient
}

type AVMetricClient struct {
	ch      chan AVMetricEvent
	dropped uint64
}

// AVMetricEvent is one row's worth of an AVMetrics-emitted event. The
// raw subclass name (e.g. AVMetricPlayerItemLikelyToKeepUpEvent) is in
// `EventType`; the unmodified SDK payload is in `Raw` so the forwarder
// can persist it verbatim and the dashboard can project new fields
// without an iOS rebuild. `EventTsMs` is the AVMetrics-side timeline
// stamp (separate from `Ts` so causality plots don't mix clocks).
type AVMetricEvent struct {
	Ts        time.Time       `json:"ts"`
	SessionID string          `json:"session_id"`
	PlayerID  string          `json:"player_id"`
	PlayID    string          `json:"play_id"`
	AttemptID uint32          `json:"attempt_id"`
	EventType string          `json:"event_type"`
	EventTsMs int64           `json:"event_ts_ms"`
	Raw       json.RawMessage `json:"raw"`
}

func NewAVMetricEventHub() *AVMetricEventHub {
	return &AVMetricEventHub{clients: map[int]*AVMetricClient{}}
}

func (h *AVMetricEventHub) AddClient(buffer int) (int, <-chan AVMetricEvent) {
	if buffer <= 0 {
		buffer = 256
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	id := h.nextID
	c := &AVMetricClient{ch: make(chan AVMetricEvent, buffer)}
	h.clients[id] = c
	return id, c.ch
}

func (h *AVMetricEventHub) RemoveClient(id int) {
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

func (h *AVMetricEventHub) Broadcast(ev AVMetricEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) == 0 {
		return
	}
	for _, c := range h.clients {
		select {
		case c.ch <- ev:
		default:
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
	URL              string `json:"url"`
	Bandwidth        int    `json:"bandwidth"`
	AverageBandwidth int    `json:"average_bandwidth,omitempty"`
	Resolution       string `json:"resolution"`
}

type TcTrafficManager struct {
	interfaceName string
	debug         bool
	nlMu          sync.Mutex
	nlHandle      *netlink.Handle // persistent netlink handle, created lazily
	nlLink        netlink.Link    // resolved once from interfaceName
	// tcMu serialises ALL tc tree mutations — the shared-root ensure (root
	// qdisc 1: + root class 1:1), the per-port leaf HTB class add/change, the
	// per-port filter install, and the per-port clear sweep. Every one of these
	// is a check-then-act against the SAME tc tree, so concurrent
	// config-on-connects otherwise interleave and wipe each other's leaf
	// classes: a leaf-class add racing a clear (or another port's root ensure)
	// leaves that port running uncapped through the 10 Gbps default class.
	// Guarding only the shared root (the pre-#746 scope) was not enough — the
	// leaf add/change, filter install, and ClearPortShaping all ran OUTSIDE the
	// lock and clobbered each other. This was masked pre-#740 by the bootstrap
	// createMu (held across the kernel apply); the reserve-then-fill that
	// replaced it removed that serialization. See #745 / #746.
	//
	// Deadlock-free by construction: Go sync.Mutex is NOT reentrant, so we use
	// the public-wrapper / lock-free-core split. Public methods acquire tcMu
	// once and delegate to a lock-free *Core helper; *Core helpers NEVER take
	// the lock and only call other lock-free helpers — never a public (locking)
	// method.
	tcMu sync.Mutex
	// Per-port ICMP filter state (issue #404). Tracks the last
	// player_ip we installed an ICMP-routing filter for so the
	// path-ping sampler's per-tick ApplyPlayerICMPFilter call
	// becomes a no-op when nothing changed. Also doubles as the
	// "is a filter currently installed?" check for cleanup.
	icmpFilterMu       sync.Mutex
	icmpFilterIPByPort map[int]string
}

type ShapeApplyState struct {
	rate     float64
	delay    int
	loss     float64
	jitter   int     // #826 explicit jitter (delay stddev)
	lossCorr float64 // #826 loss burst correlation %
	delCorr  float64 // #826 delay-distribution correlation %
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

// clearShapeApplyState forgets the last-applied shaping for a port. It MUST be
// called whenever the port's kernel tc rule is wiped out-of-band (ClearPortShaping
// at session-start / session-delete on a reused port). Otherwise the cached
// state still matches a subsequent apply of the same rate and applyShapeIfChanged
// SKIPS the re-install — leaving the port uncapped (config present, no kernel
// rule). This bit config-on-connect (#712), which clears the port then re-applies
// the materialized cap before the 302: on a reused port whose prior session had
// the same rate (e.g. the pyramid's 1.048 Mbps floor every run), the re-apply was
// skipped and the player cold-started unshaped on 4K.
func (a *App) clearShapeApplyState(port int) {
	a.shapeApplyMu.Lock()
	delete(a.shapeApply, port)
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
	if a == nil {
		return
	}
	sessionID := getString(session, "session_id")
	if a.sessionEvents != nil {
		startAt := timeFromInterface(session["session_start_time"])
		if startAt.IsZero() {
			startAt = timeFromInterface(session["first_request_time"])
		}
		if err := a.sessionEvents.RecordStart(session, manifestURL, startAt); err != nil {
			log.Printf("session event start failed session_id=%s err=%v", sessionID, err)
		}
	}
	a.emitControlEventForSession(sessionID, "proxy", "session_start", manifestURL)
}

func (a *App) recordSessionEnd(session SessionData, reason string) {
	if a == nil {
		return
	}
	sessionID := getString(session, "session_id")
	if a.sessionEvents != nil {
		if err := a.sessionEvents.RecordEnd(session, time.Now().UTC(), reason); err != nil {
			log.Printf("session event end failed session_id=%s reason=%s err=%v", sessionID, reason, err)
		}
	}
	// Mirror the lifecycle into control_events. `proxy` is the source
	// for inactive_timeout/cleared; explicit operator deletes still
	// flow through here too, so we tag the reason in `info` and let
	// downstream filter by it. Issue #474 Milestone B.
	source := "proxy"
	switch reason {
	case "deleted", "cleared":
		source = "harness"
	}
	a.emitControlEventForSession(sessionID, source, "session_end", reason)

	// #556 — for a silent death (the player stopped POSTing without a
	// clean client play-terminal event), synthesize a terminal
	// session_events frame from the last-known snapshot so the play still
	// gets an outcome row + QoE outcome labels (vsf/msf/ebvs/tier). Only
	// for inactive_timeout: operator delete/clear is administrative
	// teardown, not a play outcome, and a clean client play_end (or legacy
	// session_end) already covers the happy path (and is deduped below).
	if reason == "inactive_timeout" {
		a.synthesizeTerminalSessionEvent(session, reason)
	}
}

// synthesizeTerminalSessionEvent publishes one session_events frame
// stamped as the play's terminal row, derived from the last-known
// session snapshot. No-op when the client already delivered its own
// play-terminal event (dedupe on last_event) so we never double-stamp.
//
// playback_status: respect a terminal status the client managed to set
// before going silent; otherwise classify by whether playback ever
// started — pre-first-frame => abandoned_start (qoe_ebvs), post-first-
// frame => user_stopped. We deliberately do NOT fabricate a failure
// (mid_stream_failure): the proxy can't prove one, and a silent
// disappearance is almost always the user leaving.
func (a *App) synthesizeTerminalSessionEvent(session SessionData, reason string) {
	if a == nil {
		return
	}
	if frame, ok := terminalFrameForSession(session, reason); ok {
		a.emitSessionEvent(frame)
	}
}

// terminalFrameForSession derives the synthesized terminal frame from a
// last-known session snapshot. Pure (no App state) so it's unit-testable.
// Returns ok=false when no frame should be emitted: a nil session, or
// the client already delivered its own play-terminal event (dedupe on
// last_event).
//
// #554: the play-terminal event is `play_end` (renamed from
// `session_end`). The dedupe accepts BOTH names so a client that ended
// cleanly with either is not double-stamped — clients migrate one at a
// time and historical rows still carry `session_end`. The synthesized
// frame itself stamps the new canonical `play_end`. (Distinct from the
// proxy session-lifecycle `session_end` CONTROL event in
// recordSessionEnd, which is unchanged.)
func terminalFrameForSession(session SessionData, reason string) (SessionData, bool) {
	if session == nil {
		return nil, false
	}
	if isPlayTerminalLastEvent(getString(session, "player_metrics_last_event")) {
		return nil, false // client ended cleanly — don't double-stamp
	}
	frame := cloneSession(session)
	frame["player_metrics_last_event"] = "play_end"
	frame["player_metrics_trigger_type"] = "play_end"
	// #634 — the cloned snapshot still carries the dead session's FINAL
	// HEARTBEAT event_time. Emitting the terminal frame at that exact
	// instant ties with the real heartbeat row in session_events (the
	// forwarder anchors `ts` to event_time), and the plays aggregate's
	// argMax(playback_status, ts) then breaks the tie arbitrarily —
	// playback_status flaps between in_progress and user_stopped from
	// one read to the next. Stamp the synthetic row 1ms after the
	// snapshot so it strictly wins ordering, without distorting the
	// play's timeline by the reap delay (~60s). Unparseable/missing
	// event_time is left alone — the merge chokepoint and the forwarder
	// fallback already stamp wall clock in that case.
	if t, ok := parseEventTime(getString(session, "player_metrics_event_time")); ok {
		frame["player_metrics_event_time"] = t.Add(time.Millisecond).UTC().Format(time.RFC3339Nano)
	}
	if status := getString(session, "player_metrics_playback_status"); status == "" || status == "in_progress" {
		if getInt(session, "player_metrics_video_first_frame_time_ms") > 0 {
			frame["player_metrics_playback_status"] = "user_stopped"
		} else {
			frame["player_metrics_playback_status"] = "abandoned_start"
		}
		frame["player_metrics_playback_reason"] = reason
	}
	return frame, true
}

// isPlayTerminalLastEvent reports whether a player_metrics_last_event
// value marks the play's terminal row. Accepts both the new canonical
// `play_end` (#554) and the legacy `session_end` so the migration is
// tolerant in both directions and historical rows keep deduping.
// Mirrors the forwarder's qoe_labels.go isPlayTerminalEvent.
func isPlayTerminalLastEvent(lastEvent string) bool {
	return lastEvent == "play_end" || lastEvent == "session_end"
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
	return &TcTrafficManager{
		interfaceName:      interfaceName,
		debug:              debug,
		icmpFilterIPByPort: map[int]string{},
	}
}

func (t *TcTrafficManager) IsActive() bool {
	cmd := exec.Command("tc", "qdisc", "show", "dev", t.interfaceName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), "htb")
}

// tcAddAlreadyExists reports whether a `tc … add` failure is the benign
// "the object already exists" outcome of a concurrent apply having created
// the SAME shared object first. RTNETLINK reports a duplicate class/qdisc as
// "File exists"; the root-qdisc replace path can also report "Exclusivity
// flag on, cannot modify". Either way the shared root now exists — that's
// success, not failure. Matching the kernel's English message is unavoidable
// here: `tc` shells out and only surfaces RTNETLINK strings on stderr. See #745.
func tcAddAlreadyExists(out []byte) bool {
	s := strings.ToLower(string(out))
	return strings.Contains(s, "file exists") || strings.Contains(s, "exclusivity flag on")
}

// ensureRootQdiscCore is the lock-free core of the root-qdisc ensure. The
// caller MUST hold t.tcMu (#746). It NEVER takes the lock itself and is only
// ever called from tcMu-holding paths (updateRateLimitCore, updateNetemCore,
// EnsureClass).
func (t *TcTrafficManager) ensureRootQdiscCore() error {
	show := exec.Command("tc", "qdisc", "show", "dev", t.interfaceName)
	if out, err := show.CombinedOutput(); err == nil {
		if strings.Contains(string(out), "qdisc htb 1:") || strings.Contains(string(out), "root htb") {
			return nil
		}
	}
	// NOTE: no `tc qdisc del root` here — deleting the root nukes EVERY port's
	// leaf class + filter at once (the #746 footgun). The "root already exists"
	// check above plus the idempotent add (tcAddAlreadyExists below) are
	// sufficient to converge on a single shared root.
	cmd := exec.Command("tc", "qdisc", "add", "dev", t.interfaceName, "root", "handle", "1:", "htb", "default", "999")
	if out, err := cmd.CombinedOutput(); err != nil {
		// A concurrent installer (or an out-of-band one) won the race — the
		// htb root now exists, which is what we wanted.
		if tcAddAlreadyExists(out) {
			return nil
		}
		return fmt.Errorf("tc qdisc add failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureRootClassCore is the lock-free core of the root-class 1:1 ensure. The
// caller MUST hold t.tcMu (#746). It NEVER takes the lock itself and is only
// ever called from tcMu-holding paths.
func (t *TcTrafficManager) ensureRootClassCore() error {
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
		// The root class already exists (a concurrent apply created it) →
		// success. Previously this returned fatal, so the caller bailed before
		// installing the per-port leaf class and that port ran uncapped (#745).
		if tcAddAlreadyExists(out) {
			return nil
		}
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

// UpdateRateLimit is the public, lock-acquiring entrypoint. It holds t.tcMu
// for the whole leaf-class mutation (#746) so a concurrent config-on-connect
// can't wipe this port's leaf class mid-apply, then delegates to the lock-free
// core.
func (t *TcTrafficManager) UpdateRateLimit(port int, rateMbps float64) error {
	t.tcMu.Lock()
	defer t.tcMu.Unlock()
	return t.updateRateLimitCore(port, rateMbps)
}

// updateRateLimitCore is the lock-free core of UpdateRateLimit. The caller MUST
// hold t.tcMu. It only calls other lock-free helpers (ensureRootQdiscCore,
// ensureRootClassCore, updateNetemCore, RemoveFilter, RemoveClass,
// ensurePortFilter, ensurePrioLeafForPort) — never a public locking method.
func (t *TcTrafficManager) updateRateLimitCore(port int, rateMbps float64) error {
	if err := t.ensureRootQdiscCore(); err != nil {
		return err
	}
	if err := t.ensureRootClassCore(); err != nil {
		return err
	}
	if rateMbps <= 0 {
		log.Printf(
			"NETSHAPE throughput_set ts=%s port=%d rate_mbps=0 action=clear",
			time.Now().UTC().Format(time.RFC3339Nano),
			port,
		)
		_ = t.updateNetemCore(port, NetemParams{})
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
	// Make sure the per-port prio+netem-per-band leaf is in place so
	// the path-ping probe (issue #404) actually jumps the bulk queue
	// even when shaping was set up via a rate-only call (no netem).
	// Without this the class falls back to the kernel's default
	// pfifo leaf and ICMP routed in via ApplyPlayerICMPFilter would
	// queue behind segment data.
	if err := t.ensurePrioLeafForPort(port); err != nil {
		log.Printf("NETSHAPE prio leaf install failed port=%d: %v", port, err)
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
	// Hold tcMu for the whole clear sweep (#746): the class-del here otherwise
	// races a concurrent leaf-class add/change on another port's apply against
	// the shared tc tree. The icmpFilterMu acquired below is a DIFFERENT mutex
	// (guards only the per-port ICMP tracking map) and never overlaps tcMu's
	// scope, so no deadlock. This method runs raw tc commands only — no *Core
	// split needed.
	t.tcMu.Lock()
	defer t.tcMu.Unlock()
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
	// Delete THIS port's u32 filter by its exact handle (#816). This previously
	// inlined `tc filter del … prio 1 u32 match ip sport <port>`, which at the
	// shared prio 1 collaterally removed OTHER live sessions' filters — and since
	// ClearPortShaping is the config-on-start sweep, it was the production
	// trigger of the cross-session uncap. RemoveFilter resolves the exact handle
	// (no-op when absent). Safe under the tcMu we already hold — RemoveFilter
	// takes no lock.
	_ = t.RemoveFilter(port)
	// Also clear the per-port ICMP-to-player_ip filter installed for
	// the path-ping prio routing (issue #404). Per-port pref makes
	// this a single by-attribute delete; the tracking map is then
	// pruned so a future install fires fresh tc commands.
	_ = exec.Command("tc", "filter", "del", "dev", t.interfaceName,
		"parent", "1:", "pref", icmpFilterPref(port)).Run()
	t.icmpFilterMu.Lock()
	delete(t.icmpFilterIPByPort, port)
	t.icmpFilterMu.Unlock()
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
//
//	class htb 1:181 parent 1:1 prio 0 rate 15414Kbit ceil 15414Kbit ...
//
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

// RemoveFilter deletes ONLY this port's u32 classifier filter, resolved to its
// exact kernel handle first. The previous implementation deleted by match-spec
// at the shared `prio 1` (`tc filter del … prio 1 u32 match ip sport <port>`),
// which the kernel/iproute2 can apply to the WRONG filter at that prio —
// collaterally removing ANOTHER live session's filter. The victim's traffic
// then fell through to the 10 Gbps HTB `default 999` class (uncapped) until its
// next rate-set re-added its filter. This surfaced when a concurrent session's
// config-on-start clear sweep (ClearPortShaping) ran while peers were streaming
// (#816). Deleting by the resolved handle touches exactly one filter — or
// nothing, when this port has no filter (a fresh session's sweep), which must
// NOT fall back to a match-spec delete (that is the over-deletion being fixed).
func (t *TcTrafficManager) RemoveFilter(port int) error {
	show := exec.Command("tc", "filter", "show", "dev", t.interfaceName, "parent", "1:0")
	out, _ := show.CombinedOutput()
	handle := u32HandleForPort(string(out), port)
	if handle == "" {
		return nil // no filter classifies this port — nothing to remove
	}
	cmd := exec.Command(
		"tc", "filter", "del", "dev", t.interfaceName, "parent", "1:0", "prio", "1",
		"handle", handle, "u32",
	)
	if outDel, err := cmd.CombinedOutput(); err != nil {
		log.Printf("NETSHAPE tc filter del failed port=%d handle=%s: %s", port, handle, strings.TrimSpace(string(outDel)))
	}
	return nil
}

// u32HandleForPort scans `tc filter show … parent 1:0` output and returns the
// handle (e.g. "800::800") of the u32 filter whose selector matches the given
// port — as a source port (the primary form ensurePortFilter installs) or a
// destination port (the dport fallback). Returns "" when no filter classifies
// the port. Pure/string-only so it is unit-testable off-box.
//
// iproute2 prints each u32 leaf filter as a header line carrying
// `fh <handle> … flowid 1:<minor>` followed by one or more
// `  match <hex>/<mask> at <off>` lines. ensurePortFilter encodes sport at
// offset 20 as `<port>0000/ffff0000` and the dport fallback as
// `0000<port>/0000ffff`, so each match is associated with the most recent leaf
// handle line (the `fh 800:` hashtable line is skipped — only `::` leaf handles
// classify traffic).
func u32HandleForPort(filterShow string, port int) string {
	sportHex := "match " + fmt.Sprintf("%04x0000/ffff0000", port) // sport at offset 20
	dportHex := "match " + fmt.Sprintf("0000%04x/0000ffff", port) // dport at offset 20
	handle := ""
	for _, line := range strings.Split(filterShow, "\n") {
		if i := strings.Index(line, "fh "); i >= 0 {
			if tok := strings.Fields(line[i+3:]); len(tok) > 0 && strings.Contains(tok[0], "::") {
				handle = tok[0]
			}
		}
		if handle != "" && (strings.Contains(line, sportHex) || strings.Contains(line, dportHex)) {
			return handle
		}
	}
	return ""
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

// EnsureClass is a public, lock-acquiring entrypoint (App + updateNetemCore
// callers). It holds t.tcMu for the whole ensure (#746) and delegates to
// lock-free cores. NOTE: updateNetemCore calls ensureClassCore directly (it
// already holds tcMu); only external/App callers go through this wrapper.
func (t *TcTrafficManager) EnsureClass(port int, rateMbps float64) error {
	t.tcMu.Lock()
	defer t.tcMu.Unlock()
	return t.ensureClassCore(port, rateMbps)
}

// ensureClassCore is the lock-free core of EnsureClass. The caller MUST hold
// t.tcMu. Only calls other lock-free helpers.
func (t *TcTrafficManager) ensureClassCore(port int, rateMbps float64) error {
	if err := t.ensureRootQdiscCore(); err != nil {
		return err
	}
	if err := t.ensureRootClassCore(); err != nil {
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
	return t.updateRateLimitCore(port, rateMbps)
}

// ensurePrioLeafForPort installs the prio+netem-per-band leaf inside
// the per-port HTB class if it isn't already there (issue #404).
// MUST run whenever the class is created/updated — including the
// rate-limit-only path (UpdateRateLimit) and the netem path
// (UpdateNetem) — otherwise the class falls back to the default
// pfifo leaf and the path-ping probe queues behind bulk regardless
// of what `IP_TOS` the probe socket set.
//
// Layout:
//
//	HTB class 1:<suffix>
//	└── prio  <suffix>0:        (3 bands, default priomap)
//	    ├── netem <suffix>1:    (band 1 — TC_PRIO_INTERACTIVE → probe lane)
//	    ├── netem <suffix>2:    (band 2 — TC_PRIO_BESTEFFORT → bulk lane)
//	    └── netem <suffix>3:    (band 3 — unused)
//
// Initial install gives each band a no-op netem (delay=0, loss=0);
// UpdateNetem subsequently replaces the per-band netems with user-
// configured values. Idempotent — exits fast when the prio handle
// already shows up under `tc qdisc show parent <classid>`.
// addFairLeaf attaches sfq (Stochastic Fair Queuing) UNDER a prio band's netem
// qdisc, so that concurrent flows sharing the same rate-capped class — e.g. a
// player's separate video + audio HTTP/1.1 connections — are dequeued FAIRLY
// per flow (round-robin over a 5-tuple hash) instead of one starving the other
// in netem's internal FIFO. sfq is chosen over fq_codel deliberately: fq_codel's
// CoDel AQM fights the shaper's *intentional* queue (the throttle pushes latency
// past CoDel's 5ms target, so it ECN-marks/drops aggressively → TCP backs off →
// throughput falls below the shaped rate). sfq gives the same per-flow fairness
// with NO latency-based AQM, so it doesn't interfere with the rate shaping.
// `perturb 10` re-hashes periodically so a hash collision can't persist.
// netem applies any delay/loss first, then hands off to sfq. Must be re-applied
// whenever the band's netem is (re)placed, since replacing a qdisc drops its
// children. Best-effort: logs on failure.
func (t *TcTrafficManager) addFairLeaf(port, band int) {
	// SFQ is OFF by default: the prio band falls back to netem's internal FIFO,
	// where the byte-heavy video flow dominates the queue and audio backs up
	// behind it (no per-flow round-robin). The A/B test concluded the SFQ-driven
	// audio fairness was feeding AVPlayer's bandwidth over-read / variant
	// over-selection, so FIFO is the default. Opt back IN with PROXY_ENABLE_SFQ=1
	// to restore the per-flow fair-queue leaf.
	if os.Getenv("PROXY_ENABLE_SFQ") != "1" {
		return
	}
	portSuffix := fmt.Sprintf("%03d", port%1000)
	netemHandle := fmt.Sprintf("%s%d:", portSuffix, band)  // e.g. 1811:
	fairHandle := fmt.Sprintf("%s%d:", portSuffix, band+4) // PPP5:/6:/7: — distinct from prio (PPP0:) + netem (PPP1-3:)
	// delete-then-add, NOT replace: `tc qdisc replace` does an in-place CHANGE
	// when a qdisc already exists at that parent, which tc rejects across a
	// qdisc-TYPE switch (e.g. a leftover fq_codel from a prior build — host tc
	// state survives container restarts). del is best-effort (errors harmlessly
	// when nothing is there).
	_ = exec.Command("tc", "qdisc", "del", "dev", t.interfaceName, "parent", netemHandle).Run()
	if out, err := exec.Command(
		"tc", "qdisc", "add", "dev", t.interfaceName,
		"parent", netemHandle, "handle", fairHandle, "sfq", "perturb", "10",
	).CombinedOutput(); err != nil {
		log.Printf("NETSHAPE sfq leaf install failed port=%d band=%d: %s", port, band, strings.TrimSpace(string(out)))
	}
}

func (t *TcTrafficManager) ensurePrioLeafForPort(port int) error {
	portSuffix := fmt.Sprintf("%03d", port%1000)
	classid := fmt.Sprintf("1:%s", portSuffix)
	prioHandle := fmt.Sprintf("%s0:", portSuffix)
	show := exec.Command("tc", "qdisc", "show", "dev", t.interfaceName, "parent", classid)
	out, _ := show.CombinedOutput()
	if strings.Contains(string(out), "qdisc prio "+prioHandle) {
		return nil
	}
	// `replace` is destructive — wipes any existing leaf qdisc
	// (e.g. the kernel default pfifo) atomically and installs prio
	// in its place.
	if installOut, err := exec.Command(
		"tc", "qdisc", "replace", "dev", t.interfaceName,
		"parent", classid, "handle", prioHandle,
		"prio", "bands", "3",
	).CombinedOutput(); err != nil {
		return fmt.Errorf("tc prio replace failed: %s", strings.TrimSpace(string(installOut)))
	}
	for band := 1; band <= 3; band++ {
		bandParent := fmt.Sprintf("%s0:%d", portSuffix, band)
		bandHandle := fmt.Sprintf("%s%d:", portSuffix, band)
		if out, err := exec.Command(
			"tc", "qdisc", "replace", "dev", t.interfaceName,
			"parent", bandParent, "handle", bandHandle, "netem",
		).CombinedOutput(); err != nil {
			return fmt.Errorf("tc netem (band %d) replace failed: %s", band, strings.TrimSpace(string(out)))
		}
		t.addFairLeaf(port, band)
	}
	return nil
}

// UpdateNetem replaces each band's netem qdisc inside the prio leaf
// with the new delay/loss params. Issue #404. The leaf itself is
// installed (or confirmed present) before the netem replacements so
// this works even when called on a class created by UpdateRateLimit
// without netem ever previously being touched.
// NetemParams bundles the link-impairment knobs applied to a port's netem
// qdisc (#826). Zero values mean "unset": DelayMs/LossPct 0 ⇒ that axis off;
// JitterMs 0 ⇒ server auto-jitter (a tight 5% of delay); LossCorrelationPct 0
// ⇒ independent-uniform loss (legacy); JitterCorrelationPct 0 ⇒ no explicit
// delay correlation. DelayMs is one-way (observed RTT ≈ DelayMs since only the
// proxy's egress is shaped). See the proxy.yaml Shape doc + issue #826.
type NetemParams struct {
	DelayMs              int
	LossPct              float64
	JitterMs             int
	LossCorrelationPct   float64
	JitterCorrelationPct float64
}

// netemDelayLoss is the legacy two-axis constructor — the common
// "delay + loss, default jitter/correlation" case. Kept so simple call
// sites (clears, loss-only) stay readable.
func netemDelayLoss(delayMs int, lossPct float64) NetemParams {
	return NetemParams{DelayMs: delayMs, LossPct: lossPct}
}

// netemParamsFromSession reads the full #826 impairment knob set off a session
// map's nftables_* keys, so the static-shape and pattern paths re-apply jitter +
// correlations consistently with delay + loss. Missing keys read as zero (legacy
// clean-link / uniform behaviour).
func netemParamsFromSession(session map[string]interface{}) NetemParams {
	return NetemParams{
		DelayMs:              getInt(session, "nftables_delay_ms"),
		LossPct:              getFloat(session, "nftables_packet_loss"),
		JitterMs:             getInt(session, "nftables_jitter_ms"),
		LossCorrelationPct:   getFloat(session, "nftables_loss_correlation_pct"),
		JitterCorrelationPct: getFloat(session, "nftables_jitter_correlation_pct"),
	}
}

// netemImpairmentArgs builds the netem delay/loss argument list (everything
// after the literal `netem` token) for a NetemParams. Pure + side-effect-free
// so the #826 correlated-loss / jitter-distribution wiring is unit-testable
// without tc or Linux. Empty slice ⇒ a no-op netem (clean link).
//
// Delay: an explicit JitterMs wins (named link profiles set it directly);
// otherwise fall back to the legacy auto-jitter of 5% of the mean (a tight
// Gaussian — for delay=25 ms that's ~1 ms stddev, ~99.7% of per-packet delays
// in [22, 28] ms). Integer-divide rounds delays ≤19 ms to zero auto-jitter,
// which is fine: those low-RTT configs want jitter noise out of the signal.
// Emits `delay TIME JITTER [CORRELATION] distribution normal`; correlation
// (~25% ≈ real link) keeps successive delays correlated so netem doesn't
// reorder packets into nonsense (#826 caveat 2).
//
// Loss: a correlation term turns netem's independent-uniform loss into
// correlated/bursty loss (#826 caveat 1) — real loss clusters, and uniform
// loss at the same percentage over-punishes TCP. 0 ⇒ legacy uniform loss.
func netemImpairmentArgs(p NetemParams) []string {
	var args []string
	if p.DelayMs > 0 {
		jitter := p.JitterMs
		if jitter <= 0 {
			jitter = p.DelayMs / 20
		}
		if jitter > 0 {
			args = append(args, "delay", fmt.Sprintf("%dms", p.DelayMs), fmt.Sprintf("%dms", jitter))
			if p.JitterCorrelationPct > 0 {
				args = append(args, fmt.Sprintf("%.0f%%", p.JitterCorrelationPct))
			}
			args = append(args, "distribution", "normal")
		} else {
			args = append(args, "delay", fmt.Sprintf("%dms", p.DelayMs))
		}
	}
	if p.LossPct > 0 {
		if p.LossCorrelationPct > 0 {
			args = append(args, "loss", fmt.Sprintf("%.2f%%", p.LossPct), fmt.Sprintf("%.0f%%", p.LossCorrelationPct))
		} else {
			args = append(args, "loss", fmt.Sprintf("%.2f%%", p.LossPct))
		}
	}
	return args
}

// UpdateNetem is the public, lock-acquiring entrypoint. It holds t.tcMu for the
// whole netem mutation (#746) and delegates to the lock-free core.
func (t *TcTrafficManager) UpdateNetem(port int, p NetemParams) error {
	t.tcMu.Lock()
	defer t.tcMu.Unlock()
	return t.updateNetemCore(port, p)
}

// updateNetemCore is the lock-free core of UpdateNetem. The caller MUST hold
// t.tcMu. Only calls other lock-free helpers (ensureRootQdiscCore,
// ensureRootClassCore, ensureClassCore, ensurePrioLeafForPort) — never a public
// locking method. (Also called directly from updateRateLimitCore's clear
// branch, which already holds tcMu.)
func (t *TcTrafficManager) updateNetemCore(port int, p NetemParams) error {
	delayMs := p.DelayMs
	lossPct := p.LossPct
	if err := t.ensureRootQdiscCore(); err != nil {
		return err
	}
	if err := t.ensureRootClassCore(); err != nil {
		return err
	}
	portSuffix := fmt.Sprintf("%03d", port%1000)
	classid := fmt.Sprintf("1:%s", portSuffix)
	if delayMs <= 0 && lossPct <= 0 {
		// Clearing netem when no class exists is a no-op — don't
		// create one just to set zero netem on it.
		showClass := exec.Command("tc", "class", "show", "dev", t.interfaceName)
		classOut, _ := showClass.CombinedOutput()
		if !strings.Contains(string(classOut), classid) {
			t.logTcState("netem_clear", port)
			return nil
		}
	} else {
		if err := t.ensureClassCore(port, 10000); err != nil {
			return err
		}
	}
	if err := t.ensurePrioLeafForPort(port); err != nil {
		return err
	}
	for band := 1; band <= 3; band++ {
		bandParent := fmt.Sprintf("%s0:%d", portSuffix, band) // e.g. 1810:1
		bandHandle := fmt.Sprintf("%s%d:", portSuffix, band)  // e.g. 1811:
		args := []string{"qdisc", "replace", "dev", t.interfaceName,
			"parent", bandParent, "handle", bandHandle, "netem"}
		args = append(args, netemImpairmentArgs(p)...)
		if out, err := exec.Command("tc", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("tc netem band %d failed: %s", band, strings.TrimSpace(string(out)))
		}
		t.addFairLeaf(port, band)
	}
	if delayMs <= 0 && lossPct <= 0 {
		t.logTcState("netem_clear", port)
	} else {
		t.logTcState("netem_apply", port)
	}
	return nil
}

// icmpFilterPref returns a unique tc filter pref for the per-port
// ICMP-to-player_ip filter. Per-port pref means deletion can be
// done by attribute (no need to parse handles out of `tc filter
// show` output).
func icmpFilterPref(port int) string {
	return fmt.Sprintf("%d", 1000+(port%1000))
}

// ApplyPlayerICMPFilter installs (install=true) or removes
// (install=false) a `tc filter` that routes ICMP packets destined
// for `playerIP` into the per-port HTB class. Issue #404 — combined
// with the prio+netem leaf installed by UpdateNetem and IP_TOS=0x10
// on the path-ping socket, this is what lets the probe see the
// configured netem delay while jumping the bulk queue.
//
// Idempotent: tracks the last-installed IP per port and skips the
// tc invocation when nothing changed. Handles the player-IP-changed
// case (rare but possible if a player reconnects from a different
// IP under the same session) by deleting and re-adding.
func (t *TcTrafficManager) ApplyPlayerICMPFilter(port int, playerIP string, install bool) error {
	if t == nil {
		return nil
	}
	t.icmpFilterMu.Lock()
	defer t.icmpFilterMu.Unlock()
	current := t.icmpFilterIPByPort[port]
	pref := icmpFilterPref(port)
	if !install || playerIP == "" {
		if current == "" {
			return nil
		}
		_ = exec.Command("tc", "filter", "del", "dev", t.interfaceName,
			"parent", "1:", "pref", pref).Run()
		delete(t.icmpFilterIPByPort, port)
		return nil
	}
	if current == playerIP {
		return nil
	}
	if current != "" {
		_ = exec.Command("tc", "filter", "del", "dev", t.interfaceName,
			"parent", "1:", "pref", pref).Run()
	}
	portSuffix := fmt.Sprintf("%03d", port%1000)
	classid := fmt.Sprintf("1:%s", portSuffix)
	cmd := exec.Command(
		"tc", "filter", "add", "dev", t.interfaceName,
		"protocol", "ip", "parent", "1:", "pref", pref, "u32",
		"match", "ip", "protocol", "1", "0xff",
		"match", "ip", "dst", playerIP+"/32",
		"flowid", classid,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tc icmp filter add port=%d ip=%s: %s",
			port, playerIP, strings.TrimSpace(string(out)))
	}
	t.icmpFilterIPByPort[port] = playerIP
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
	defaultRateMbps := getenvInt("INFINITE_STREAM_DEFAULT_RATE_MBPS", 0)
	if defaultRateMbps < 0 {
		log.Printf("INFINITE_STREAM_DEFAULT_RATE_MBPS=%d invalid (negative); using 0", defaultRateMbps)
		defaultRateMbps = 0
	}
	if defaultRateMbps > 0 {
		log.Printf("baseline rate cap: %d Mbps (issue #480; new sessions default to this rate)", defaultRateMbps)
	} else {
		log.Printf("baseline rate cap: unlimited (INFINITE_STREAM_DEFAULT_RATE_MBPS=0 or unset)")
	}
	interfaceName := getenvAny([]string{"INFINITE_STREAM_TC_INTERFACE", "INFINITE_TC_INTERFACE", "TC_INTERFACE"}, "eth0")
	tcDebug := getenvBoolAny([]string{"INFINITE_STREAM_TC_DEBUG", "INFINITE_TC_DEBUG", "TC_DEBUG"}, false)
	eventStore, eventStoreErr := newSessionEventStore(getenv("GO_PROXY_SESSION_EVENTS_DB", defaultSessionEventsDB))
	if eventStoreErr != nil {
		log.Printf("session event store disabled: %v", eventStoreErr)
	}

	emptySessions := []SessionData{}
	app := &App{
		throughputData:  map[int]map[string]interface{}{},
		sessionEvents:   eventStore,
		traffic:         NewTcTrafficManager(interfaceName, tcDebug),
		upstreamHost:    upstreamHost,
		upstreamPort:    upstreamPort,
		maxSessions:     maxSessions,
		defaultRateMbps: defaultRateMbps,
		portMap:         loadPortMapping(),
		client: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 6 * time.Second}).DialContext,
				ResponseHeaderTimeout: 6 * time.Second,
			},
		},
		shapeLoops:           map[int]context.CancelFunc{},
		shapeStates:          map[int]NftShapePattern{},
		shapeApply:           map[int]ShapeApplyState{},
		faultLoops:           map[int]context.CancelFunc{},
		sessionsHub:          NewSessionEventHub(),
		networkHub:           NewNetworkEventHub(),
		controlHub:           NewControlEventHub(),
		avmetricsHub:         NewAVMetricEventHub(),
		networkLogs:          map[string]*NetworkLogRingBuffer{},
		loopStateBySession:   map[string]ServerLoopState{},
		segmentFlight:        map[int]segmentFlightInfo{},
		segmentRun:           map[int]segmentRunRecord{},
		drainActive:          map[int]bool{},
		tcSamples:            map[int][]tcSample{},
		wireRate:             map[int]wireRateSample{},
		tcCache:              map[int]*tcStatsCache{},
		transferCompleteMbps: map[int]float64{},
		transferCompleteAt:   map[int]time.Time{},
	}

	app.sessionsSnap.Store(&emptySessions)

	go app.trackPortThroughput()
	app.restoreTransportFaultSchedules()
	// Re-install tc rate/delay/loss state for every session that
	// survived the proxy restart. Without this, pre-existing sessions
	// keep their session-map values but the kernel forgot — they end
	// up uncapped. Issue #480.
	restoredShapes, skippedShapes := app.restoreShapeApplication()
	// Record the restart as an archivable control_event so a cap-drop spike
	// landing in the boot restore window is attributable to a redeploy rather
	// than a shaper bug. Sticky-replayed to the forwarder on reconnect. #671.
	app.emitServerStart(restoredShapes, skippedShapes)
	// 100 ms TCP_INFO sampler — folds smoothed RTT / jitter / lifetime
	// min / RTO into per-session windows that get drained on each
	// snapshot broadcast (issue #401). Linux-only kernel read; the
	// !linux stub keeps the dev build green on macOS.
	app.startRTTSampler(context.Background())
	// 1 Hz out-of-band ICMP probe to each session's player_ip
	// (issue #404). Surfaces path latency independent of the
	// streaming connection's queue contribution — the line that
	// stays put when shaping kicks in, while TCP_INFO RTT climbs.
	app.startPathPingSampler(context.Background())

	router := mux.NewRouter()
	router.Use(corsMiddleware)

	router.HandleFunc("/index.html", app.handleIndex).Methods(http.MethodGet)
	router.HandleFunc("/api/sessions", app.handleGetSessions).Methods(http.MethodGet)
	router.HandleFunc("/api/sessions/stream", app.handleSessionStream).Methods(http.MethodGet)
	router.HandleFunc("/api/session/{id}", app.handleSession).Methods(http.MethodGet, http.MethodDelete)
	router.HandleFunc("/api/session/{id}", app.handlePatchSession).Methods(http.MethodPatch)
	router.HandleFunc("/api/session/{id}/metrics", app.handlePostSessionMetrics).Methods(http.MethodPost)
	// iOS 18 AVMetrics raw event stream (issue #486 spike). Kept on its
	// own POST + SSE pair so the comparison against /metrics stays clean.
	router.HandleFunc("/api/session/{id}/avmetrics", app.handlePostSessionAVMetrics).Methods(http.MethodPost)
	router.HandleFunc("/api/avmetrics/stream", app.handleAVMetricsStream).Methods(http.MethodGet)
	router.HandleFunc("/api/session/{id}/network", app.handleGetNetworkLog).Methods(http.MethodGet)
	router.HandleFunc("/api/network/stream", app.handleNetworkStream).Methods(http.MethodGet)
	// Control events SSE — issue #474 Milestone B. Mirrors
	// /api/network/stream's shape; one JSON envelope per emitted
	// action.
	router.HandleFunc("/api/control/stream", app.handleControlStream).Methods(http.MethodGet)
	router.HandleFunc("/api/external-ips", app.handleGetExternalIPs).Methods(http.MethodGet)
	router.HandleFunc("/api/clear-sessions", app.handleClearSessions).Methods(http.MethodPost)
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

	// v2 harness API. Mounts every /api/v2/* route. v1 paths above stay
	// unchanged. Phase B: read-only handlers backed by the v1 adapter;
	// mutation/SSE endpoints still 501.
	v2server.Mount(router, v2server.New(NewV2Adapter(app)))

	router.PathPrefix("/").HandlerFunc(app.handleProxy)

	ports := []int{30081, 30181, 30281, 30381, 30481, 30581, 30681, 30781, 30881}

	// TLS + HTTP/2 on every listener so the dashboard (served HTTPS
	// by nginx at port 21000/30000) can embed HLS playback URLs
	// pointing here without browsers blocking mixed content. Cert
	// is the same self-signed pair launch.sh writes for nginx; we
	// reuse it so there's exactly one identity to trust per dev
	// machine. h2 turns on automatically once TLS is in play.
	const tlsCertFile = "/etc/nginx/certs/localhost.pem"
	const tlsKeyFile = "/etc/nginx/certs/localhost-key.pem"
	// INFINITE_STREAM_TLS=off (or 0/false/no) serves plain HTTP on the
	// shaper ports instead of TLS, matching nginx's listener so an HTTP
	// dashboard embeds HTTP playback (no mixed content). Default: on.
	tlsEnabled := true
	switch strings.ToLower(strings.TrimSpace(os.Getenv("INFINITE_STREAM_TLS"))) {
	case "off", "0", "false", "no":
		tlsEnabled = false
	}
	errorCh := make(chan error, len(ports))
	for _, port := range ports {
		addr := fmt.Sprintf(":%d", port)
		go func(bind string, p int) {
			srv := &http.Server{
				Addr:    bind,
				Handler: router,
				// Stamp the underlying *net.TCPConn on the per-
				// connection context so handleProxy can attach
				// it to its session for the RTT sampler to
				// read. Issue #401.
				ConnContext: withTCPConnContext,
			}
			// Disable HTTP/2 on the per-session MEDIA ports so a player's video
			// and audio fetch over SEPARATE HTTP/1.1 connections rather than
			// multiplexing on one rate-capped h2 connection — where audio starves
			// behind video in the kernel/tc FIFO (the origin fetch is instant, but
			// the audio response queues ~the whole video drain). A non-nil empty
			// TLSNextProto suppresses the stdlib's automatic h2 ALPN. Keep h2 on
			// the API port (30081), where the dashboard/forwarder SSE streams rely
			// on multiplexing many event-streams over one connection.
			if p != 30081 {
				srv.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
			}
			if tlsEnabled {
				log.Printf("go-proxy listening on %s (TLS)", bind)
				errorCh <- srv.ListenAndServeTLS(tlsCertFile, tlsKeyFile)
			} else {
				log.Printf("go-proxy listening on %s (plain HTTP)", bind)
				errorCh <- srv.ListenAndServe()
			}
		}(addr, port)
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

// handleControlStream emits each control_events row as it lands.
// Body shape mirrors /api/network/stream: one SSE `data:` line per
// action, `{"session_id":"...","entry":{...}}`. Forwarder subscribes
// to this and writes to ClickHouse. Issue #474 Milestone B.
func (a *App) handleControlStream(w http.ResponseWriter, r *http.Request) {
	if a.controlHub == nil {
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

	clientID, ch := a.controlHub.AddClient(1024)
	defer a.controlHub.RemoveClient(clientID)

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
				SessionID string       `json:"session_id"`
				Entry     ControlEvent `json:"entry"`
			}{ev.SessionID, ev}
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

// emitControlEventForSession is the single entry point proxy code calls
// to record a control_events action. Looks up sticky identity
// (player_id, play_id, attempt_id) from the session map so the row
// joins to session_events / network_requests on the same fields. Issue
// #474 Milestone B.
func (a *App) emitControlEventForSession(sessionID, source, event, info string) {
	if a == nil || a.controlHub == nil || event == "" {
		return
	}
	var (
		playerID  string
		playID    string
		attemptID uint32
	)
	if sessionID != "" {
		playerID = a.sessionStickyPlayerID(sessionID)
		playID = a.sessionStickyPlayID(sessionID)
		attemptID = a.sessionStickyAttemptID(sessionID)
	}
	a.controlHub.Broadcast(ControlEvent{
		Ts:        time.Now().UTC(),
		SessionID: sessionID,
		PlayerID:  playerID,
		PlayID:    playID,
		AttemptID: attemptID,
		Source:    source,
		Event:     event,
		Info:      info,
	})
}

// emitServerStart records a global (session-less) boot marker into
// control_events so a proxy restart is correlatable with the cap-drop spikes
// it can produce (the restore window before restoreShapeApplication re-installs
// each port's tc filter — see #671). source=auto: server-driven, not operator
// or harness. Carries the shape-restoration counts so an operator can see how
// many sessions were re-capped on boot. Goes through BroadcastServerStart so
// the forwarder still archives it after its SSE reconnects post-restart.
func (a *App) emitServerStart(restored, skipped int) {
	if a == nil || a.controlHub == nil {
		return
	}
	info := fmt.Sprintf("restored=%d;skipped=%d;baseline_mbps=%d", restored, skipped, a.defaultRateMbps)
	a.controlHub.BroadcastServerStart(ControlEvent{
		Ts:     time.Now().UTC(),
		Source: "auto",
		Event:  "server_start",
		Info:   info,
	})
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

	for {
		select {
		case <-r.Context().Done():
			return
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
	// Per-session lock serialises this handler so concurrent POSTs
	// for the same session merge in arrival order rather than racing
	// on the global sessionsMu. See `metricsPostMu` for the rationale.
	muIface, _ := a.metricsPostMu.LoadOrStore(id, &sync.Mutex{})
	mu := muIface.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
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
	// Propagate play_id + attempt_id from the URL query (issue #280).
	// The metrics POST is the only signal we receive between manifest
	// requests on long-running iOS sessions; without picking up the
	// player's current ids here, attempt_id increments would lag
	// until the next manifest fetch.
	if v := strings.TrimSpace(r.URL.Query().Get("play_id")); v != "" {
		metricsOnly["play_id"] = v
	}
	if v := strings.TrimSpace(r.URL.Query().Get("attempt_id")); v != "" {
		metricsOnly["attempt_id"] = v
	}
	// Play-scoped client start (#587) — picked up here too so it lands
	// on long-running iOS sessions between manifest fetches, same as
	// play_id/attempt_id.
	if v := strings.TrimSpace(r.URL.Query().Get("start_time")); v != "" {
		metricsOnly["start_time"] = v
	}
	merged, ok := a.saveSessionByIDReturning(id, metricsOnly)
	// Issue #470: emit one SSE frame per metrics POST. Every POST —
	// heartbeat or otherwise — flows through so the forwarder writes
	// exactly one snapshot row per event the player produced. The
	// significance gate (`isSignificantPlayerEvent`) is gone because
	// the previous "debounce + significance" pair was the source of
	// the cadence aliasing and the stale-marker leak that produced
	// duplicate session_events rows. Each frame stands on its own.
	if ok {
		a.emitSessionEvent(merged)
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

// avmetricsBatchPayload is the wire shape iOS posts to
// /api/session/{id}/avmetrics. The player batches AVMetrics events
// (~50 per POST or every ~500 ms, whichever fires first) so we don't
// fan a separate request out per LikelyToKeepUp / Stall / Variant
// switch frame. `Raw` is a passthrough of the SDK event payload so
// the projection can evolve server-side without an iOS rebuild.
type avmetricsBatchPayload struct {
	Events []struct {
		EventType string          `json:"event_type"`
		EventTsMs int64           `json:"event_ts_ms"`
		Raw       json.RawMessage `json:"raw"`
	} `json:"events"`
}

// handlePostSessionAVMetrics receives a batch of iOS 18 AVMetrics events
// for one session and broadcasts each as an SSE frame to
// /api/avmetrics/stream. Issue #486 spike.
//
// Stays out of the SessionData map / control_revision flow on purpose:
// AVMetrics is a parallel observation stream for comparison against the
// existing /metrics heartbeat, not a replacement, and we want both
// streams independently subscribable + independently disablable.
func (a *App) handlePostSessionAVMetrics(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if a.avmetricsHub == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{"error": "avmetrics hub unavailable"})
		return
	}
	var payload avmetricsBatchPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid json"})
		return
	}
	if len(payload.Events) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "no events"})
		return
	}
	// Resolve sticky identity once per batch — same lookup the
	// control_events emitter uses so AVMetric rows join to
	// session_events / network_requests on the same fields.
	var (
		playerID  string
		playID    string
		attemptID uint32
	)
	if id != "" {
		playerID = a.sessionStickyPlayerID(id)
		playID = a.sessionStickyPlayID(id)
		attemptID = a.sessionStickyAttemptID(id)
	}
	// Per-request play_id / attempt_id overrides via query string, mirroring
	// handlePostSessionMetrics (#280) — keeps the row's identity in sync
	// with the player's current attempt even between manifest fetches.
	if v := strings.TrimSpace(r.URL.Query().Get("play_id")); v != "" {
		playID = v
	}
	if v := strings.TrimSpace(r.URL.Query().Get("attempt_id")); v != "" {
		if parsed, err := strconv.ParseUint(v, 10, 32); err == nil {
			attemptID = uint32(parsed)
		}
	}
	now := time.Now().UTC()
	for _, ev := range payload.Events {
		if ev.EventType == "" {
			continue
		}
		a.avmetricsHub.Broadcast(AVMetricEvent{
			Ts:        now,
			SessionID: id,
			PlayerID:  playerID,
			PlayID:    playID,
			AttemptID: attemptID,
			EventType: ev.EventType,
			EventTsMs: ev.EventTsMs,
			Raw:       ev.Raw,
		})
	}
	w.WriteHeader(http.StatusOK)
	writeJSON(w, map[string]int{"accepted": len(payload.Events)})
}

// handleAVMetricsStream emits each AVMetrics event as it lands. Body
// shape mirrors /api/control/stream: one SSE `data:` line per event,
// `{"session_id":"...","entry":{...}}`. Issue #486 spike.
func (a *App) handleAVMetricsStream(w http.ResponseWriter, r *http.Request) {
	if a.avmetricsHub == nil {
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

	clientID, ch := a.avmetricsHub.AddClient(1024)
	defer a.avmetricsHub.RemoveClient(clientID)

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
				SessionID string        `json:"session_id"`
				Entry     AVMetricEvent `json:"entry"`
			}{ev.SessionID, ev}
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
	shapeRateFields := []string{"nftables_bandwidth_mbps", "nftables_delay_ms", "nftables_packet_loss", "nftables_jitter_ms", "nftables_loss_correlation_pct", "nftables_jitter_correlation_pct"}
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
			getInt(target, "player_metrics_loop_count_delta"),
			getInt(target, "loop_count_server"),
		)
	} else if _, ok := payload["player_metrics_loop_count_delta"]; ok {
		log.Printf(
			"LOOP_COUNTER_PATCH session_id=%s source=%s event=%s player_loop_count=%d loop_increment=%d server_loop_count=%d",
			id,
			getString(target, "player_metrics_source"),
			getString(target, "player_metrics_last_event"),
			getInt(target, "player_metrics_loop_count_player"),
			getInt(target, "player_metrics_loop_count_delta"),
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
	resetFailureWindowState(payload, target)
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
			resetFailureWindowState(payload, session)
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
					np := netemParamsFromSession(target)
					log.Printf("SESSION_PATTERN_START source=session_patch session_id=%s port=%d steps=%d", id, portNum, len(steps))
					if err := a.applyShapePattern(portNum, steps, np); err != nil {
						log.Printf("SESSION_PATTERN_START_FAILED session_id=%s port=%d: %v", id, portNum, err)
					}
				}
			} else {
				log.Printf("SESSION_PATTERN_STOP source=session_patch session_id=%s port=%d", id, portNum)
				a.stopShapeLoop(portNum)
			}
		}
	}

	// Emit a control_event summarising the operator's mutation (issue
	// #474 Milestone B). Classify the change by the field touched and
	// fall back to `control_change` when nothing more specific maps.
	// Source is always `harness` here — every caller of
	// applySessionSettingsUpdate is the dashboard / harness PATCH path.
	a.emitHarnessSettingsChange(id, payload)

	return target, http.StatusOK, ""
}

// emitHarnessSettingsChange classifies a settings PATCH into one of
// the control_events vocab entries and broadcasts it. Multiple field
// touches in one payload emit multiple events — that matches operator
// intent (one PATCH can change rate + pattern + labels, all three are
// distinct surfaces).
func (a *App) emitHarnessSettingsChange(sessionID string, payload map[string]interface{}) {
	if a == nil || a.controlHub == nil || len(payload) == 0 {
		return
	}
	emitted := false
	emit := func(event, info string) {
		a.emitControlEventForSession(sessionID, "harness", event, info)
		emitted = true
	}
	// Labels first — cheapest test. Carry the new labels payload in
	// `info` so the forwarder can stamp each KV pair onto the row's
	// labels[] array, making them queryable via the existing
	// `--label-has` Sessions filter (issue #482 follow-up).
	if v, ok := payload["labels"]; ok {
		emit("label_changed", labelsInfoJSON(v))
	}
	if _, ok := payload["content_id"]; ok {
		emit("content_changed", "")
	}
	if _, ok := payload["manifest_url"]; ok {
		emit("content_changed", "")
	}
	// Transport fault rule.
	transportTouched := false
	for _, k := range []string{
		"transport_failure_type", "transport_fault_type",
		"transport_failure_frequency", "transport_failure_units",
		"transport_consecutive_failures", "transport_consecutive_seconds",
		"transport_consecutive_units", "transport_frequency_seconds",
		"transport_fault_on_seconds", "transport_fault_off_seconds",
	} {
		if _, ok := payload[k]; ok {
			transportTouched = true
			break
		}
	}
	if transportTouched {
		// Distinguish enable vs disable from the type field if present.
		ft := normalizeTransportFaultType(getString(payload, "transport_fault_type"))
		if ft == "none" {
			ft = normalizeTransportFaultType(getString(payload, "transport_failure_type"))
		}
		switch ft {
		case "none":
			emit("fault_rule_disabled", "")
		case "":
			emit("fault_rule_config_change", "")
		default:
			emit("fault_rule_enabled", ft)
		}
	}
	// Pattern enable / disable is emitted by applyShapePattern /
	// disablePatternForPort directly so EVERY toggle path is
	// covered (PATCH, switch-to-sliders, session release, group
	// reset, …). Skip the redundant hook here to avoid double-emit.
	// Pure config edits (steps array, template mode, margin) that
	// stay within an already-enabled pattern still surface via
	// pattern_config_change.
	patternTouched := false
	for _, k := range []string{"nftables_pattern_steps", "nftables_pattern_template_mode", "nftables_pattern_margin_pct"} {
		if _, ok := payload[k]; ok {
			patternTouched = true
			break
		}
	}
	if patternTouched {
		emit("pattern_config_change", "")
	}
	// Shaper rate / delay / loss + #826 jitter/correlation knobs.
	shaperTouched := false
	for _, k := range []string{"nftables_bandwidth_mbps", "nftables_delay_ms", "nftables_packet_loss", "nftables_jitter_ms", "nftables_loss_correlation_pct", "nftables_jitter_correlation_pct"} {
		if _, ok := payload[k]; ok {
			shaperTouched = true
			break
		}
	}
	if shaperTouched {
		emit("shaper_config_change", "")
	}
	// Timeouts.
	timeoutsTouched := false
	for _, k := range []string{
		"transfer_active_timeout_seconds", "transfer_idle_timeout_seconds",
		"transfer_timeout_applies_manifests", "transfer_timeout_applies_master",
		"transfer_timeout_applies_segments",
	} {
		if _, ok := payload[k]; ok {
			timeoutsTouched = true
			break
		}
	}
	if timeoutsTouched {
		emit("timeouts_changed", "")
	}
	if !emitted {
		// Generic fallback so analytics never miss a mutation.
		emit("control_change", "")
	}
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

func (a *App) handleSession(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if r.Method == http.MethodDelete {
		// Collect the removed session(s) inside the (re-runnable) CAS
		// closure; loop-state teardown / recordSessionEnd / kernel teardown
		// run once on the committed result (mutateSessions side-effect rule).
		var removed []SessionData
		a.mutateSessions(func(sessions []SessionData) ([]SessionData, bool) {
			removed = removed[:0]
			filtered := make([]SessionData, 0, len(sessions))
			for _, session := range sessions {
				if getString(session, "session_id") != id {
					filtered = append(filtered, session)
					continue
				}
				removed = append(removed, session)
			}
			return filtered, len(removed) > 0
		})
		removedPorts := map[int]struct{}{}
		for _, session := range removed {
			a.removeServerLoopState(id)
			a.recordSessionEnd(session, "deleted")
			if port, err := strconv.Atoi(getString(session, "x_forwarded_port")); err == nil {
				removedPorts[port] = struct{}{}
			}
		}
		for port := range removedPorts {
			a.disablePatternForPort(port)
			a.armTransportFaultLoop(port, "none", 1, transportUnitsSeconds, 0)
			// Tear down any tc rate limit / netem / filter on this
			// port so a future session that reuses the slot starts
			// clean — pairs with the ClearPortShaping at session-
			// allocation time as belt-and-braces (issue #352).
			if a.traffic != nil {
				_ = a.traffic.UpdateNetem(port, NetemParams{})
				a.traffic.ClearPortShaping(port)
				a.clearShapeApplyState(port)
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
	sessionList := a.sessionsView() // #740 read-only: builds ExternalIPEntry view, no mutation
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
	// Capture the cleared sessions inside the (re-runnable) CAS closure;
	// loop-state teardown / recordSessionEnd / kernel teardown run once on
	// the committed result (mutateSessions side-effect rule).
	var removed []SessionData
	a.mutateSessions(func(sessions []SessionData) ([]SessionData, bool) {
		removed = append(removed[:0], sessions...)
		return []SessionData{}, true
	})
	portSet := map[int]struct{}{}
	a.shapeMu.Lock()
	for port := range a.shapeLoops {
		portSet[port] = struct{}{}
	}
	a.shapeMu.Unlock()
	for _, session := range removed {
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
	writeJSON(w, map[string]string{"message": "All sessions cleared successfully"})
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

func (a *App) applyShapePattern(port int, steps []NftShapeStep, np NetemParams) error {
	if a.traffic == nil {
		return fmt.Errorf("traffic manager not initialized")
	}
	delayMs, loss := np.DelayMs, np.LossPct
	cleanSteps := sanitizeShapeSteps(steps)
	if len(cleanSteps) == 0 {
		a.stopShapeLoop(port)
		a.updateSessionsByPortWithControl(port, map[string]interface{}{
			"nftables_pattern_enabled": false,
			"nftables_pattern_steps":   []NftShapeStep{},
		}, "")
		// Empty-steps branch counts as a pattern_disabled — the
		// caller asked for "no pattern". Without this, an operator
		// who cleared the steps table would see no Control row.
		a.emitControlEventForPort(port, "proxy", "pattern_disabled", "")
		// Disarming the pattern loop leaves the kernel at whatever
		// rate the last pattern step applied. Reassert the session's
		// static shape so the user-visible "Limit" matches reality —
		// without this the proxy reports pattern_enabled=false +
		// bandwidth=N but the kernel keeps shaping at the old pattern
		// rate (issue: ramp-pattern → sliders=0 didn't tear down the
		// shaper). Walks every session bound to this port so a port
		// hosting a group still drops the rate cleanly. Runs after
		// the session update above so applySessionShaping sees the
		// freshly cleared pattern_enabled flag and doesn't no-op via
		// its "pattern owns the rate" guard.
		for _, sess := range a.sessionsView() { // #740 read-only: applySessionShaping reads sess, drives kernel
			if portStr := getString(sess, "x_forwarded_port"); portStr != "" {
				if p, err := strconv.Atoi(portStr); err == nil && p == port {
					a.applySessionShaping(sess, port)
				}
			}
		}
		return nil
	}
	if err := a.traffic.UpdateNetem(port, np); err != nil {
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
		"nftables_pattern_enabled":        true,
		"nftables_pattern_steps":          cleanSteps,
		"nftables_delay_ms":               delayMs,
		"nftables_packet_loss":            loss,
		"nftables_jitter_ms":              np.JitterMs,
		"nftables_loss_correlation_pct":   np.LossCorrelationPct,
		"nftables_jitter_correlation_pct": np.JitterCorrelationPct,
	}, "")
	// Emit pattern_enabled (per session on this port) so the
	// dashboard's PlayLog Control bucket surfaces operator toggles
	// regardless of which endpoint triggered them. Info carries the
	// step count + first rate + template mode (rampUp / stairs /
	// custom …); the mode flows through to a per-pattern label so
	// the Sessions filter can distinguish them.
	stepCount := len(cleanSteps)
	firstRate := 0.0
	if stepCount > 0 {
		firstRate = cleanSteps[0].RateMbps
	}
	// template_mode was just stamped by updateSessionsByPortWithControl
	// above when the caller included it in the payload, so any session
	// on this port has the freshest value.
	mode := ""
	for _, sess := range a.sessionsView() { // #740 read-only: reads pattern template mode
		if portStr := getString(sess, "x_forwarded_port"); portStr != "" {
			if pn, err := strconv.Atoi(portStr); err == nil && pn == port {
				mode = getString(sess, "nftables_pattern_template_mode")
				break
			}
		}
	}
	info := fmt.Sprintf(`{"mode":%q,"steps":%d,"rate_mbps_first":%.3f,"delay_ms":%d,"packet_loss":%.3f}`,
		mode, stepCount, firstRate, delayMs, loss)
	a.emitControlEventForPort(port, "proxy", "pattern_enabled", info)
	go a.runShapePatternLoop(ctx, port, cleanSteps, np)
	return nil
}

func (a *App) runShapePatternLoop(ctx context.Context, port int, steps []NftShapeStep, np NetemParams) {
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
	// Resolve the owner labels once (master identity + template) for the slave
	// "driven by master" markers fanned out each tick.
	drivenBy, drivenTemplate := a.patternOwnerLabels(port)
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
		if err := a.applyShapeIfChanged(port, step.RateMbps, np); err != nil {
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
			"nftables_pattern_master":            true,
		})
		// Single-owner group shaping: mirror this step's cap onto the rest of the
		// group so every member tracks the master in lock-step (no per-member loop).
		a.fanPatternRateToGroup(port, step.RateMbps, np, drivenBy, drivenTemplate)
		// Emit pattern_step as a control_event for every session on
		// this port (issue #474 Milestone B). Info is a tiny JSON
		// blob so downstream (graphs, harness archive) can read
		// step / rate / duration without re-fetching pattern config.
		for _, sess := range a.sessionsView() { // #740 read-only: builds control-event info string
			if portStr := getString(sess, "x_forwarded_port"); portStr != "" {
				if pn, err := strconv.Atoi(portStr); err == nil && pn == port {
					info := fmt.Sprintf(`{"step":%d,"rate_mbps":%.3f,"duration_s":%.1f}`,
						stepIndex+1, step.RateMbps, step.DurationSeconds)
					a.emitControlEventForSession(getString(sess, "session_id"), "proxy", "pattern_step", info)
				}
			}
		}
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
	// Emit pattern_disabled to control_events for every session on
	// this port. Without this hook the dashboard's PlayLog "Control"
	// bucket stayed silent on toggle paths that bypass
	// applySessionSettingsUpdate (e.g. switching to sliders mode,
	// session release, group reset). Issue #474 follow-up.
	a.emitControlEventForPort(port, "proxy", "pattern_disabled", "")
}

// fanPatternRateToGroup applies the master's current pattern rate to every OTHER
// port in the master's group — the single-owner model. This pattern loop is the
// only engine; each tick mirrors its cap onto the group's members so they track
// in lock-step with zero drift. Members get the kernel cap via applyShapeIfChanged
// (change-detected → an unchanged member is a no-op) plus display markers
// (rate_runtime + driven_by + driven_template) so the dashboard shows "driven by
// master" WITHOUT arming a per-member pattern (no steps, no step-clock). The group
// is re-enumerated every call, so a member that connected/reattached after the
// master armed is picked up on the next tick (no dependence on instantiation order).
func (a *App) fanPatternRateToGroup(originPort int, rate float64, np NetemParams, drivenBy, drivenTemplate string) {
	snap := a.sessionsView() // #740 read-only: group/port lookups only
	gid := a.getGroupIdByPort(originPort, snap)
	if gid == "" {
		log.Printf("NETSHAPE group fan-out skipped port=%d reason=no_group_id sessions=%d", originPort, len(snap))
		return
	}
	ports := a.getPortsForGroup(gid, snap)
	fanned := 0
	for _, gp := range ports {
		if gp == originPort {
			continue
		}
		if err := a.applyShapeIfChanged(gp, rate, np); err != nil {
			log.Printf("NETSHAPE group pattern fan-out failed port=%d group=%s rate_mbps=%.3f: %v", gp, gid, rate, err)
			continue
		}
		// Display only — the member's chart Limit line + slider track the master's
		// cap. Deliberately NO setShapeRuntimeStep / nftables_pattern_steps: members
		// stay template-less and step-clock-less (single owner = the master).
		a.updateSessionsByPort(gp, map[string]interface{}{
			"nftables_pattern_rate_runtime_mbps": rate,
			"nftables_pattern_driven_by":         drivenBy,
			"nftables_pattern_driven_template":   drivenTemplate,
		})
		fanned++
	}
	// #single-owner debug: surface what the fan-out resolved each tick so a
	// non-firing group (slaves stuck at their connect cap) is diagnosable from
	// the proxy log — origin port, resolved group, ALL member ports the group
	// enumerated, and how many were actually fanned.
	log.Printf("NETSHAPE group fan-out port=%d group=%s member_ports=%v fanned=%d rate_mbps=%.3f", originPort, gid, ports, fanned, rate)
}

// patternOwnerLabels reads the master (origin) session for the labels the slave
// UI shows: the master's display label and the active template name. Best-effort —
// empty strings just mean the slave badge omits that detail.
func (a *App) patternOwnerLabels(originPort int) (drivenBy, drivenTemplate string) {
	for _, s := range a.sessionsView() { // #740 read-only
		if !a.sessionMatchesPort(s, originPort) {
			continue
		}
		drivenBy = getString(s, "display_id")
		if drivenBy == "" {
			pid := getString(s, "player_id")
			if len(pid) > 8 {
				pid = pid[:8]
			}
			drivenBy = pid
		}
		if raw, ok := s["_v2_shape_pattern"]; ok {
			if m, ok := raw.(map[string]interface{}); ok {
				drivenTemplate = getString(m, "template")
			}
		}
		break
	}
	return drivenBy, drivenTemplate
}

// emitControlEventsForDiff inspects a (before, after) session-state
// pair and emits one control_event per detected operator-driven
// change. Used by paths that mutate the session map directly (v2
// PATCH via MutatePlayer) and so bypass applySessionSettingsUpdate's
// PATCH-payload hook. source is always `harness` here because every
// caller is a dashboard / harness PATCH. Issue #474 follow-up.
func (a *App) emitControlEventsForDiff(sessionID string, before, after map[string]interface{}) {
	if a == nil || a.controlHub == nil || sessionID == "" {
		return
	}
	changed := func(k string) bool {
		return !sessionFieldsEqual(before[k], after[k])
	}
	emit := func(event, info string) {
		a.emitControlEventForSession(sessionID, "harness", event, info)
	}

	// Labels — any change to the v1 `labels` slot (legacy direct PATCH)
	// or the v2 `_v2_labels` slot (PATCH /api/v2/players Merge Patch
	// writes here, see internal/v2/server/handlers_mutate.go § applyLabelsPatch).
	// Both surface as `info=<key>_<value>` row labels via the forwarder's
	// kvLabelsFromInfo helper (issue #487 — fixes the v2 path which was
	// silently dropping labels because the diff check only looked at v1).
	if changed("labels") || changed("_v2_labels") {
		payload := after["_v2_labels"]
		if payload == nil {
			payload = after["labels"]
		}
		emit("label_changed", labelsInfoJSON(payload))
	}
	// Content selection.
	if changed("content_id") || changed("manifest_url") {
		emit("content_changed", "")
	}
	// Transport fault rule.
	transportKeys := []string{
		"transport_failure_type", "transport_fault_type",
		"transport_failure_frequency", "transport_failure_units",
		"transport_consecutive_failures", "transport_consecutive_seconds",
		"transport_consecutive_units", "transport_frequency_seconds",
		"transport_fault_on_seconds", "transport_fault_off_seconds",
	}
	transportTouched := false
	for _, k := range transportKeys {
		if changed(k) {
			transportTouched = true
			break
		}
	}
	if transportTouched {
		ft := normalizeTransportFaultType(getString(after, "transport_fault_type"))
		if ft == "none" {
			ft = normalizeTransportFaultType(getString(after, "transport_failure_type"))
		}
		prev := normalizeTransportFaultType(getString(before, "transport_fault_type"))
		if prev == "none" {
			prev = normalizeTransportFaultType(getString(before, "transport_failure_type"))
		}
		switch {
		case prev == "none" && ft != "none":
			emit("fault_rule_enabled", ft)
		case prev != "none" && ft == "none":
			emit("fault_rule_disabled", prev)
		default:
			emit("fault_rule_config_change", ft)
		}
	}
	// Per-surface failure rules (master, manifest, segment, all).
	for _, surface := range []string{"master_manifest", "manifest", "segment", "all"} {
		typeKey := surface + "_failure_type"
		freqKey := surface + "_failure_frequency"
		modeKey := surface + "_failure_mode"
		if !(changed(typeKey) || changed(freqKey) || changed(modeKey)) {
			continue
		}
		prev := getString(before, typeKey)
		cur := getString(after, typeKey)
		switch {
		case (prev == "" || prev == "none") && cur != "" && cur != "none":
			emit("fault_rule_enabled", surface+":"+cur)
		case prev != "" && prev != "none" && (cur == "" || cur == "none"):
			emit("fault_rule_disabled", surface+":"+prev)
		default:
			emit("fault_rule_config_change", surface+":"+cur)
		}
	}
	// Shaper (rate / delay / loss + #826 jitter / correlations) — sliders.
	if changed("nftables_bandwidth_mbps") || changed("nftables_delay_ms") || changed("nftables_packet_loss") ||
		changed("nftables_jitter_ms") || changed("nftables_loss_correlation_pct") || changed("nftables_jitter_correlation_pct") {
		emit("shaper_config_change",
			fmt.Sprintf(`{"rate_mbps":%v,"delay_ms":%v,"packet_loss":%v,"jitter_ms":%v,"loss_correlation_pct":%v,"jitter_correlation_pct":%v}`,
				after["nftables_bandwidth_mbps"], after["nftables_delay_ms"], after["nftables_packet_loss"],
				after["nftables_jitter_ms"], after["nftables_loss_correlation_pct"], after["nftables_jitter_correlation_pct"]))
	}
	// Pattern enable/disable is emitted by applyShapePattern /
	// disablePatternForPort directly — skip here. Config-only edits
	// to steps still need surfacing.
	if changed("nftables_pattern_steps") || changed("nftables_pattern_template_mode") || changed("nftables_pattern_margin_pct") {
		emit("pattern_config_change", "")
	}
	// Transfer timeouts.
	for _, k := range []string{
		"transfer_active_timeout_seconds", "transfer_idle_timeout_seconds",
		"transfer_timeout_applies_manifests", "transfer_timeout_applies_master",
		"transfer_timeout_applies_segments",
	} {
		if changed(k) {
			emit("timeouts_changed", "")
			break
		}
	}
}

// sessionFieldsEqual compares two interface{} values pulled out of a
// labelsInfoJSON marshals the labels map for embedding in a
// label_changed control_event's Info string. Accepts the raw
// session-map value (interface{}) — usually map[string]any or
// map[string]string — and returns a stable JSON object string the
// forwarder can parse to extract KV pairs. Empty / nil / wrong-type
// input renders as "" (the forwarder treats that as "labels cleared").
func labelsInfoJSON(v interface{}) string {
	if v == nil {
		return ""
	}
	flat := map[string]string{}
	switch m := v.(type) {
	case map[string]string:
		for k, val := range m {
			flat[k] = val
		}
	case map[string]any:
		for k, val := range m {
			if s, ok := val.(string); ok {
				flat[k] = s
			}
		}
	default:
		return ""
	}
	if len(flat) == 0 {
		return ""
	}
	b, err := json.Marshal(flat)
	if err != nil {
		return ""
	}
	return string(b)
}

// session map for the diff-based control_event emitter. Strings,
// numbers, bools compare by ==; arrays / objects compare by JSON-
// round-trip. Cheap because session-map values are small.
func sessionFieldsEqual(a, b interface{}) bool {
	if a == nil && b == nil {
		return true
	}
	// Same-type primitive shortcut.
	switch va := a.(type) {
	case string:
		if vb, ok := b.(string); ok {
			return va == vb
		}
	case bool:
		if vb, ok := b.(bool); ok {
			return va == vb
		}
	case float64:
		if vb, ok := b.(float64); ok {
			return va == vb
		}
	case int:
		if vb, ok := b.(int); ok {
			return va == vb
		}
	}
	// Fall through to JSON for arrays / nested maps / mixed
	// number types.
	ja, ea := json.Marshal(a)
	jb, eb := json.Marshal(b)
	if ea != nil || eb != nil {
		return false
	}
	return string(ja) == string(jb)
}

// emitControlEventForPort emits one control_event per session bound to
// the given proxy port. Used by pattern enable/disable + shaper paths
// that act on a port (not a single session). Empty info when the
// event has no extras worth surfacing.
func (a *App) emitControlEventForPort(port int, source, event, info string) {
	if a == nil || a.controlHub == nil || event == "" {
		return
	}
	for _, sess := range a.sessionsView() { // #740 read-only: matches port, emits control event
		portStr := getString(sess, "x_forwarded_port")
		if portStr == "" {
			continue
		}
		if pn, err := strconv.Atoi(portStr); err == nil && pn == port {
			a.emitControlEventForSession(getString(sess, "session_id"), source, event, info)
		}
	}
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
	// #740: scan the no-clone view, but return a clone of the single match —
	// callers may mutate the result, so it must not alias the live snapshot.
	for _, session := range a.sessionsView() {
		if getString(session, "x_forwarded_port") == portStr {
			return cloneSession(session)
		}
	}
	return nil
}

func (a *App) setTransportFaultSessionState(port int, faultType string, active bool, startedAt string, phaseSeconds float64, cycleSeconds float64) {
	phaseRounded := math.Round(phaseSeconds*1000) / 1000
	cycleRounded := math.Round(cycleSeconds*1000) / 1000
	controlRevision := newControlRevision()
	// fault active-edge sessions captured inside the (re-runnable) CAS
	// closure; the fault_on/fault_off control_events are emitted once on
	// the committed result (mutateSessions side-effect rule).
	var edges []string
	a.mutateSessions(func(sessions []SessionData) ([]SessionData, bool) {
		edges = edges[:0]
		changed := false
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
					applyControlRevision(session, controlRevision)
				}
				if prevActive != active {
					edges = append(edges, getString(session, "session_id"))
				}
				changed = true
			}
		}
		return sessions, changed
	})
	// Emit control_event on the fault active edge — fault_on / fault_off
	// (issue #474 Milestone B). Replaces the snapshot_failures classifier's
	// transport_fault edge.
	ev := "fault_off"
	if active {
		ev = "fault_on"
	}
	for _, sessionID := range edges {
		a.emitControlEventForSession(sessionID, "proxy", ev, faultType)
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
	for _, session := range a.sessionsView() { // #740 read-only: re-arms transport faults from config
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

// restoreShapeApplication re-applies the tc rate/delay/loss state for
// every session in the loaded session map. Required on boot because
// the container's network namespace is recreated on restart — tc
// classes/filters don't survive.
//
// CAVEAT (#686): this is currently a NO-OP across a real restart. The
// session map is in-memory only (saveSessionList → publishSnapshot;
// there is no disk persistence), so at boot the list is empty and this
// restores nothing (the server_start marker reports restored=0). Until
// #686 adds disk persistence, sessions that pre-existed a restart run
// uncapped at the deployment baseline until shaping is re-applied —
// which is the restore-window rate spike #686 tracks. Matches
// restoreTransportFaultSchedules' pattern (and shares its limitation).
func (a *App) restoreShapeApplication() (restored, skipped int) {
	if a.traffic == nil {
		return 0, 0
	}
	seenPorts := map[int]struct{}{}
	for _, session := range a.sessionsView() { // #740 read-only: re-applies shape from config
		portStr := getString(session, "x_forwarded_port")
		if portStr == "" {
			skipped++
			continue
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			skipped++
			continue
		}
		if _, ok := seenPorts[port]; ok {
			continue
		}
		seenPorts[port] = struct{}{}
		// applySessionShaping reads nftables_bandwidth_mbps + delay + loss
		// from the session map, runs them through a.effectiveRate (so
		// rate=0 resolves to the deployment baseline), and installs the
		// kernel state. Pattern is owned by applySessionShaping's early
		// return when pattern_enabled is set; we don't need to special-
		// case it here.
		a.applySessionShaping(session, port)
		restored++
	}
	log.Printf("shape restoration on boot: restored=%d skipped=%d baseline_mbps=%d", restored, skipped, a.defaultRateMbps)
	return restored, skipped
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
		JitterMs               int            `json:"jitter_ms"`              // #826
		LossCorrelationPct     float64        `json:"loss_correlation_pct"`   // #826
		JitterCorrelationPct   float64        `json:"jitter_correlation_pct"` // #826
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
	case "sliders", "square_wave", "ramp_up", "ramp_down", "pyramid", "valley", "transient_shock":
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
	patternNetem := NetemParams{
		DelayMs: payload.DelayMs, LossPct: payload.LossPct,
		JitterMs: payload.JitterMs, LossCorrelationPct: payload.LossCorrelationPct, JitterCorrelationPct: payload.JitterCorrelationPct,
	}
	if err := a.applyShapePattern(port, cleanSteps, patternNetem); err != nil {
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

	// Single-owner group shaping: the origin port's pattern loop fans each tick's
	// rate to the group's members (see fanPatternRateToGroup in runShapePatternLoop).
	// We deliberately do NOT arm a second pattern loop on each member here — that was
	// the double-arm: N independent per-member loops that drift apart and show the
	// pattern template on every member instead of just the master.

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
	lossCorr := getFloat(payload, "loss_correlation_pct")
	a.disablePatternForPort(port)
	if err := a.traffic.UpdateNetem(port, NetemParams{LossPct: loss, LossCorrelationPct: lossCorr}); err != nil {
		log.Printf("NETSHAPE packet loss failed port=%d loss=%.2f corr=%.1f: %v", port, loss, lossCorr, err)
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": "Failed to update packet loss", "details": err.Error()})
		return
	}
	a.updateSessionsByPortWithControl(port, map[string]interface{}{
		"nftables_packet_loss":          loss,
		"nftables_loss_correlation_pct": lossCorr,
	}, "")
	writeJSON(w, map[string]interface{}{"success": true, "port": port, "loss_pct": loss, "loss_correlation_pct": lossCorr})
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
	// Storage holds the operator's raw intent (rate_mbps=0 stays 0 — the
	// slider position is "no override"). The kernel sees the deployment
	// baseline when the operator didn't override; a.effectiveRate does
	// the translation at the UpdateRateLimit call sites below. Issue
	// #480.
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
	// #826 link-impairment knobs: jitter (delay stddev) + burst correlations.
	// All optional; absent ⇒ 0 ⇒ server auto-jitter / uniform loss (legacy).
	jitterMs := getInt(payload, "jitter_ms")
	lossCorr := getFloat(payload, "loss_correlation_pct")
	jitterCorr := getFloat(payload, "jitter_correlation_pct")
	np := NetemParams{
		DelayMs: delayMs, LossPct: loss,
		JitterMs: jitterMs, LossCorrelationPct: lossCorr, JitterCorrelationPct: jitterCorr,
	}
	a.disablePatternForPort(port)
	effectiveMbps := a.effectiveRate(rateMbps)
	if err := a.traffic.UpdateRateLimit(port, effectiveMbps); err != nil {
		log.Printf("NETSHAPE rate limit failed port=%d rate=%g (effective=%g): %v", port, rateMbps, effectiveMbps, err)
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": "Failed to update rate limit", "details": err.Error()})
		return
	}
	if err := a.traffic.UpdateNetem(port, np); err != nil {
		log.Printf("NETSHAPE netem failed port=%d delay=%d loss=%.2f jitter=%d loss_corr=%.1f del_corr=%.1f: %v", port, delayMs, loss, jitterMs, lossCorr, jitterCorr, err)
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": "Failed to update delay/loss", "details": err.Error()})
		return
	}
	a.updateSessionsByPortWithControl(port, map[string]interface{}{
		"nftables_bandwidth_mbps":         rateMbps, // operator intent; 0 = no override
		"nftables_delay_ms":               delayMs,
		"nftables_packet_loss":            loss,
		"nftables_jitter_ms":              jitterMs,
		"nftables_loss_correlation_pct":   lossCorr,
		"nftables_jitter_correlation_pct": jitterCorr,
	}, "")

	// Propagate to group members
	snap2 := a.sessionsView() // #740 read-only: group/port lookups only
	groupID := a.getGroupIdByPort(port, snap2)
	if groupID != "" {
		groupPorts := a.getPortsForGroup(groupID, snap2)
		for _, groupPort := range groupPorts {
			if groupPort == port {
				continue // Skip the original port
			}
			a.disablePatternForPort(groupPort)
			if err := a.traffic.UpdateRateLimit(groupPort, effectiveMbps); err != nil {
				log.Printf("NETSHAPE group propagation rate limit failed port=%d rate=%g (effective=%g): %v", groupPort, rateMbps, effectiveMbps, err)
				continue
			}
			if err := a.traffic.UpdateNetem(groupPort, np); err != nil {
				log.Printf("NETSHAPE group propagation netem failed port=%d delay=%d loss=%.2f: %v", groupPort, delayMs, loss, err)
				continue
			}
			a.updateSessionsByPortWithControl(groupPort, map[string]interface{}{
				"nftables_bandwidth_mbps":         rateMbps,
				"nftables_delay_ms":               delayMs,
				"nftables_packet_loss":            loss,
				"nftables_jitter_ms":              jitterMs,
				"nftables_loss_correlation_pct":   lossCorr,
				"nftables_jitter_correlation_pct": jitterCorr,
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

// applySocketFault hijacks the client TCP connection and emits the
// wire shape for the named fault. Each fault produces a SPECIFIC,
// CONTRACTUAL on-the-wire pattern that characterization tests (in
// particular tests/characterization/modes/abort_test.go) interpret
// against. Subtle behaviour changes silently invalidate that test's
// results.
//
// **DO NOT CHANGE WIRE BEHAVIOURS OF EXISTING FAULT TYPES.**
// If a different shape is needed, add a new fault-type name; don't
// repurpose an old one.
//
// The canonical reference for every fault type's wire shape AND the
// real-world failure mode it models is:
//
//	.claude/standards/fault-injection-wire-contract.md
//
// Read it before editing this function or any of the case branches
// below. The doc lists: TCP-level shape, what the client OS surfaces,
// and which real failure scenarios each shape reproduces.
//
// Related: isSocketFaultType (keep allowlist in sync), the
// `corrupted` and `transfer_active_timeout` paths which model
// different failure surfaces (see the standards doc).
func applySocketFault(w http.ResponseWriter, faultType, contentType, upstreamURL string) (string, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return "", fmt.Errorf("hijack unsupported")
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return "", err
	}
	// For body_* fault types we send a chunk of REAL upstream bytes
	// (a valid prefix of the segment / playlist response) before the
	// fault closes the socket. This makes the failure shape match
	// real-world mid-transfer aborts: the client receives parseable
	// media data, then a clean close or RST. Fake "X" filler used to
	// be written here, but that's the `corrupted` failure type's
	// territory — `request_body_*` is for "the connection died
	// mid-stream while real data was flowing." See abort
	// characterization test (modes/abort_test.go).
	//
	// On upstream-fetch failure we fall back to "X" filler so the
	// fault still applies — losing some realism but preserving the
	// close behaviour the rule promises.
	var midBody []byte
	if needsRealBodyBytes(faultType) {
		midBody = fetchUpstreamBodyPrefix(upstreamURL, socketMidBodyBytes)
		if len(midBody) == 0 {
			midBody = bytes.Repeat([]byte("X"), socketMidBodyBytes)
		}
	}
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

// needsRealBodyBytes reports whether the named socket fault writes a
// prefix of the upstream body to the client before the close behaviour
// fires. The connect_* and first_byte_* shapes never write any body
// bytes; only the body_* shapes do.
func needsRealBodyBytes(faultType string) bool {
	switch faultType {
	case "request_body_reset", "request_body_hang", "request_body_delayed":
		return true
	}
	return false
}

// fetchUpstreamBodyPrefix issues a short-timeout GET to the upstream
// URL and returns up to `limit` bytes of the response body. Returns
// nil on any failure (DNS, connect, non-2xx, timeout); caller falls
// back to synthetic filler when the prefix isn't available.
//
// Bounded by a 2s timeout so a stuck upstream doesn't extend the
// fault application latency past what the operator armed the rule for.
func fetchUpstreamBodyPrefix(upstreamURL string, limit int) []byte {
	if upstreamURL == "" || limit <= 0 {
		return nil
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(upstreamURL)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil
	}
	buf := make([]byte, limit)
	n, _ := io.ReadFull(resp.Body, buf)
	if n <= 0 {
		return nil
	}
	return buf[:n]
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
	// Extract the player's `play_id` + `attempt_id` query params (issue
	// #280). Used to scope HAR snapshots to a single playback episode
	// and to track recovery attempts within it. Stamped onto every
	// NetworkLogEntry created in this handler via the logEntry closure
	// below.
	playID := strings.TrimSpace(r.URL.Query().Get("play_id"))
	attemptIDStr := strings.TrimSpace(r.URL.Query().Get("attempt_id"))
	// Client-supplied, play-scoped start (#587). Rotates with play_id;
	// the proxy just carries it through to the session map so it reaches
	// PlayRecord.start_time (live) and the session_events CH column.
	startTime := strings.TrimSpace(r.URL.Query().Get("start_time"))
	var attemptID uint32
	if attemptIDStr != "" {
		if n, err := strconv.ParseUint(attemptIDStr, 10, 32); err == nil {
			attemptID = uint32(n)
		}
	}
	logEntry := func(sessionID string, entry NetworkLogEntry) {
		if entry.PlayID == "" {
			entry.PlayID = playID
		}
		if entry.AttemptID == 0 {
			entry.AttemptID = attemptID
		}
		// #613: TotalMs is provisionally set at upstream-headers-complete
		// (~TTFB) by the fetch helper, before the body transfer happens.
		// Lift it to TTFB+Transfer here — the single chokepoint every
		// logged row passes through — so no response-serving path can ship
		// a row with the pre-transfer value. Idempotent (max), so paths
		// that already set TotalMs ≥ TTFB+Transfer (e.g. fault rows) are
		// untouched.
		mergeTotalTiming(&entry)
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
				// Preserve the request's scheme — go-proxy now serves
				// TLS on every shaper port (issue TS11), so an HTTPS
				// dashboard must redirect to https:// to avoid mixed
				// content. Plain HTTP requests redirect to http://.
				scheme := requestScheme(r)
				newURL := fmt.Sprintf("%s://%s:%s/%s", scheme, host, newPort, escapedPath)
				// #712: drop any proxy.* config args on reattach — config is
				// materialized once on first bind; a loop/auto-recovery restart
				// re-hitting the base port must never re-apply or leak proxy.*
				// onto the session port.
				if stripped := stripProxyArgs(r.URL.RawQuery); stripped != "" {
					newURL = newURL + "?" + stripped
				}
				log.Printf("Redirecting to existing session URL: %s %s -> %s", newURL, externalPort, newPort)
				http.Redirect(w, r, newURL, http.StatusFound)
				return
			}
		}
		// #712 config-on-connect: parse proxy.* args before allocating a
		// session so a malformed config is a 400 that consumes no session
		// slot. Reattach (existing-session) requests already redirected above
		// and never reach here, so config is materialized exactly once — on
		// the player's first bind.
		configPatch, hasConfig, cfgErr := parseProxyArgs(r.URL.Query())
		if cfgErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]interface{}{"error": "invalid proxy config args", "detail": cfgErr.Error()})
			return
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
		// #740 reserve-then-fill: atomically claim a session slot via CAS
		// instead of serialising the whole bootstrap under createMu. The
		// closure re-reads the committed list and re-picks `allocated` on every
		// retry, so two concurrent config-on-connect bootstraps (a fleet) can
		// never claim the same slot — superseding #739's createMu and the
		// snapshot→allocate→reserve lost-update that let one session's rate
		// config land on the other's port (the loser created config-less →
		// nftk=100 baseline leak). The reservation is a minimal placeholder
		// carrying a stable session_id (so a concurrent allocateSessionNumber
		// sees the slot used) plus player_id/group_id (so a concurrent
		// same-player reattach still de-dupes) and a fresh last_request (so
		// removeInactiveSessions won't evict it mid-bootstrap). The full
		// session is CAS-filled below once the port-derived work and config
		// materialization succeed; a rejected config triggers a cleanup CAS so
		// no slot leaks.
		createdAt := nowISO()
		groupID := extractGroupId(playerID)
		// #fleet-group: an explicit group_id connect param wins over the legacy
		// `_G<num>` player_id suffix. Lets the harness born-group a fleet while
		// keeping player_id a clean UUID — so the analytics layer doesn't derive
		// a divergent v5 id from a non-UUID player_id (the suffix's fatal flaw).
		if g := r.URL.Query().Get("group_id"); g != "" {
			groupID = g
		}
		// #fleet-group display-only: group_broadcast=false makes the group a
		// pure DISPLAY link — members share group_id (so the dashboard charts
		// them together and the archive groups them) but a member PATCH is NOT
		// mirrored to the other members. Used by the startup fleet, where every
		// device runs its own cold-start plan and a broadcast would corrupt the
		// per-device measurements. Default (absent / any non-false value) keeps
		// the pyramid-style auto-broadcast group.
		groupBroadcast := true
		if gb := r.URL.Query().Get("group_broadcast"); gb == "false" || gb == "0" {
			groupBroadcast = false
		}
		var allocated int
		_, reserved := a.mutateSessions(func(sessions []SessionData) ([]SessionData, bool) {
			if len(sessions) >= a.maxSessions {
				return sessions, false
			}
			allocated = allocateSessionNumber(sessions, a.maxSessions)
			idStr := fmt.Sprintf("%d", allocated)
			return append(sessions, SessionData{
				"session_id":         idStr,
				"session_number":     idStr,
				"sid":                idStr,
				"player_id":          playerID,
				"group_id":           groupID,
				"last_request":       createdAt,
				"session_start_time": createdAt,
				"_reserved":          true,
			}), true
		})
		if !reserved {
			// Only false path is at-capacity (closure always changes otherwise).
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		idStr := fmt.Sprintf("%d", allocated)
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
		// Sweep any leftover nftables transport fault (drop/reject) on the
		// assigned internal port — the symmetric counterpart to the tc
		// ClearPortShaping above (issue #716). Config-on-connect (#712)
		// mints a fresh player_id per run, so tc state is well-isolated by
		// the sweep above, but a transport fault left armed by a prior
		// session whose teardown was skipped (crash / Ctrl-C / timeout
		// before Session.Release fired) is the one kernel surface that can
		// still carry over via port reuse inside the 5-min idle-reap window.
		// armTransportFaultLoop(…, "none", …) is the same teardown used on
		// session DELETE (above): it cancels any still-running fault-loop
		// goroutine (which would otherwise re-arm the rule after a bare
		// clear) AND deletes the leftover rule. Idempotent and quiet on a
		// clean port. Unlike the tc sweep (which keys on port%1000), this
		// must run *after* the external→internal mapping, because faults
		// are armed on x_forwarded_port (the internal port) — see the arm
		// calls keyed on x_forwarded_port elsewhere in this file.
		if internalPortInt, err := strconv.Atoi(assignedInternalPort); err == nil {
			a.armTransportFaultLoop(internalPortInt, "none", 1, transportUnitsSeconds, 0)
		}
		// Optional play_id from the client. iOS/tvOS/Roku don't mint one
		// (the v2 read path derives a stable fallback), but the v3 web
		// player (VideoPlayerFrame) does — surfacing it here lets the
		// forwarder archive each web play under its operator-known id
		// instead of the server-side derivation.
		playID := r.URL.Query().Get("play_id")
		sessionData := SessionData{
			"session_number":                 fmt.Sprintf("%d", allocated),
			"sid":                            fmt.Sprintf("%d", allocated),
			"session_id":                     fmt.Sprintf("%d", allocated),
			"player_id":                      playerID,
			"play_id":                        playID,
			"group_id":                       groupID,
			"group_broadcast":                groupBroadcast,
			"control_revision":               newControlRevision(),
			"headers_player_id":              playerHeader,
			"headers_player-ID":              playerHeaderAlt,
			"headers_x_playback_session_id":  playbackSessionHeader,
			"manifest_requests_count":        0,
			"master_manifest_requests_count": 0,
			"segments_count":                 0,
			"all_requests_count":             0,
			"last_request":                   createdAt,
			"first_request_time":             createdAt,
			"session_start_time":             createdAt,
			// Segment / manifest / master_manifest fault config —
			// initialise mode + units explicitly so both server
			// (NewFailureHandler) and dashboard (Mode dropdown) read
			// the same value from a single source of truth instead
			// of falling back to duplicated hard-coded defaults.
			// "failures_per_seconds" mode → consecutive=requests,
			// frequency=seconds, matching the dashboard's visible
			// default Mode for a fresh session.
			"segment_failure_type":                 "none",
			"segment_failure_frequency":            0,
			"segment_consecutive_failures":         0,
			"segment_failure_units":                "requests",
			"segment_consecutive_units":            "requests",
			"segment_frequency_units":              "seconds",
			"segment_failure_mode":                 "failures_per_seconds",
			"manifest_failure_type":                "none",
			"manifest_failure_frequency":           0,
			"manifest_failure_units":               "requests",
			"manifest_consecutive_units":           "requests",
			"manifest_frequency_units":             "seconds",
			"manifest_failure_mode":                "failures_per_seconds",
			"manifest_consecutive_failures":        0,
			"master_manifest_failure_type":         "none",
			"master_manifest_failure_frequency":    0,
			"master_manifest_failure_units":        "requests",
			"master_manifest_consecutive_units":    "requests",
			"master_manifest_frequency_units":      "seconds",
			"master_manifest_failure_mode":         "failures_per_seconds",
			"master_manifest_consecutive_failures": 0,
			// "All" fault override — when all_failure_type != "none",
			// HandleRequest uses this rule for every HTTP request and
			// ignores the per-kind tabs above. Same control shape as
			// segment, plus all_failure_urls for variant scoping.
			"all_failure_type":            "none",
			"all_failure_frequency":       0,
			"all_consecutive_failures":    0,
			"all_failure_units":           "requests",
			"all_consecutive_units":       "requests",
			"all_frequency_units":         "seconds",
			"all_failure_mode":            "failures_per_seconds",
			"current_failures":            0,
			"consecutive_failures_count":  0,
			"player_ip":                   requesterIP,
			"user_agent":                  "",
			"origination_ip":              requesterIP,
			"origination_time":            createdAt,
			"is_external_ip":              isExternalIP(requesterIP),
			"manifest_failure_at":         nil,
			"manifest_failure_recover_at": nil,
			// nil (not []string{}) so the dashboard can tell "fresh
			// session, default to all-URLs filter" from "user
			// explicitly cleared the list" — both serialize to JSON
			// the same when both are []string{}, which made unchecking
			// "All" silently snap back via the empty-defaults-to-all
			// rule on the dashboard (#409).
			"manifest_failure_urls":                    nil,
			"segment_failure_urls":                     nil,
			"segment_failure_at":                       nil,
			"segment_failure_recover_at":               nil,
			"master_manifest_failure_at":               nil,
			"master_manifest_failure_recover_at":       nil,
			"all_failure_at":                           nil,
			"all_failure_recover_at":                   nil,
			"all_failure_urls":                         nil,
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
			// nftables_bandwidth_mbps starts at 0 — "no operator
			// override." On a deployment with defaultRateMbps>0 the
			// kernel still gets capped at the baseline (via
			// a.effectiveRate at the apply site below) but the slider
			// in the dashboard stays at 0 so the operator sees "I
			// haven't touched this." The derived effective_rate_mbps
			// field surfaces what the kernel is actually enforcing.
			// Issue #480.
			"nftables_bandwidth_mbps": float64(0),
		}
		// #712: materialize proxy.* config onto the fresh SessionData before
		// it's published. ApplyConfigPatch runs the SAME translator the PATCH
		// API uses, so the URL-arg vocabulary can't drift from the API model.
		// Translation only — the kernel is driven below, after the save.
		if hasConfig {
			if aerr := v2server.ApplyConfigPatch(sessionData, configPatch); aerr != nil {
				// #740: a rejected config must not leak the reserved slot —
				// CAS the placeholder back out before returning the 400.
				a.removeReservedSession(idStr)
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(w, map[string]interface{}{"error": "proxy config rejected", "detail": aerr.Error()})
				return
			}
		}
		a.resetServerLoopState(idStr)
		// #740 fill: CAS the placeholder reservation up to the full session.
		a.fillReservedSession(idStr, sessionData)
		// Apply the deployment baseline to the kernel BEFORE the
		// redirect fires — the client reconnects on the new port
		// immediately and the first segment burst would otherwise run
		// uncapped. effectiveRate(0) returns the baseline (or 0 on
		// prod-style deployments). No-op when traffic is nil
		// (non-Linux dev). Issue #480.
		if hasConfig {
			// #712: drive the kernel from the just-materialized config before
			// the redirect fires — the client reconnects on the new port
			// immediately and the first segment burst would otherwise run
			// unshaped. applySessionShaping resolves the deployment baseline
			// via effectiveRate, so the no-rate-override case (e.g. labels- or
			// fault_rules-only config) still gets the baseline cap — this
			// supersedes the plain baseline apply in the else branch.
			if port, err := strconv.Atoi(assignedInternalPort); err == nil {
				// The session-start sweep (ClearPortShaping, above) wiped any
				// leftover kernel tc rule on this reused port, but the
				// apply-state cache still holds the prior session's rate.
				// Invalidate it so the apply below actually fires tc instead of
				// being skipped as "unchanged" — otherwise the player cold-starts
				// unshaped (config present, no kernel rule). Regression from #712
				// re-applying at session-start after #352's ClearPortShaping.
				a.clearShapeApplyState(port)
				if steps := v2server.PatternStepsFromSession(sessionData); len(steps) > 0 {
					v1steps := make([]NftShapeStep, 0, len(steps))
					for _, s := range steps {
						v1steps = append(v1steps, NftShapeStep{
							RateMbps:        s.RateMbps,
							DurationSeconds: s.DurationSeconds,
							Enabled:         s.Enabled,
						})
					}
					np := netemParamsFromSession(sessionData)
					if perr := a.applyShapePattern(port, v1steps, np); perr != nil {
						log.Printf("config-on-connect pattern apply failed port=%d: %v", port, perr)
					}
				} else {
					a.applySessionShaping(sessionData, port)
				}
				if ft := getString(sessionData, "transport_failure_type"); ft != "" && ft != "none" {
					consec := getInt(sessionData, "transport_consecutive_failures")
					if consec < 1 {
						consec = 1
					}
					units := getString(sessionData, "transport_consecutive_units")
					if units == "" {
						units = "seconds"
					}
					freq := getInt(sessionData, "transport_failure_frequency")
					a.armTransportFaultLoop(port, ft, consec, units, freq)
				}
			}
		} else if a.defaultRateMbps > 0 && a.traffic != nil {
			if internalPortInt, err := strconv.Atoi(assignedInternalPort); err == nil {
				effective := a.effectiveRate(0)
				if err := a.traffic.UpdateRateLimit(internalPortInt, effective); err != nil {
					log.Printf("baseline rate cap apply failed port=%d rate=%g: %v", internalPortInt, effective, err)
				} else {
					log.Printf("baseline rate cap applied port=%d rate=%g Mbps (#480)", internalPortInt, effective)
				}
			}
		}
		manifestURL := "/" + escapedPath
		if r.URL.RawQuery != "" {
			manifestURL = manifestURL + "?" + r.URL.RawQuery
		}
		a.recordSessionStart(sessionData, manifestURL)
		host := hostWithoutPort(r.Host)
		scheme := requestScheme(r)
		newURL := fmt.Sprintf("%s://%s:%s/%s", scheme, host, assignedExternalPort, escapedPath)
		// #712: strip proxy.* config args from the redirect — config is already
		// materialized on the session; the player follows this clean URL and
		// resolves all child requests against it, so proxy.* never reach the
		// session port or the child-request space.
		if stripped := stripProxyArgs(r.URL.RawQuery); stripped != "" {
			newURL = newURL + "?" + stripped
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
	// #550 Phase 4: best-effort device taxonomy from UA for
	// non-instrumented clients (VLC, ffplay, hls.js, Roku channels,
	// etc.). Idempotent + non-overwriting — iOS-emitted DeviceInfo
	// values from the metrics POST channel take precedence by virtue
	// of stampDeviceFromUserAgent's setIfEmpty check.
	stampDeviceFromUserAgent(sessionData)
	// Stamp the player's current play_id + attempt_id on the session
	// so the SSE stream (and downstream analytics) can partition by
	// playback episode (play_id) and recovery attempt (attempt_id).
	// Both are player-supplied; the proxy never synthesises them.
	// When the player has not yet sent a value, the field stays empty
	// on the session map — downstream tables get blank rather than
	// the proxy guessing.
	if playID != "" {
		sessionData["play_id"] = playID
	}
	// Play-scoped client start (#587). Carried on the session map so
	// v2translate can project PlayRecord.start_time and the SSE
	// session_events frame can carry it to ClickHouse. The client
	// rotates it with play_id, so it always reflects THIS play.
	if startTime != "" {
		sessionData["start_time"] = startTime
	}
	// Store the raw string so sessionStickyField (a generic
	// type-asserts-as-string helper) can read it back uniformly
	// across the metrics POST + manifest GET paths.
	if attemptIDStr != "" {
		sessionData["attempt_id"] = attemptIDStr
	}

	// Extract client IP considering X-Forwarded-For
	clientIP := extractClientIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"))
	// Diagnostic: surface every overwrite where the new value looks
	// like a Docker bridge / loopback / private IP that's replacing
	// a previously-correct external value. The log gives us the URL,
	// method, and raw headers that triggered the bad overwrite.
	if prev := getString(sessionData, "player_ip"); prev != "" && prev != clientIP {
		newIsExternal := isExternalIP(clientIP)
		prevWasExternal := isExternalIP(prev)
		if prevWasExternal && !newIsExternal {
			log.Printf("[GO-PROXY][PLAYER-IP-OVERWRITE] %s %s session_id=%s prev=%s new=%s remote=%s xff=%q ua=%q",
				r.Method, r.URL.RequestURI(),
				sessionNumber,
				prev, clientIP,
				r.RemoteAddr,
				r.Header.Get("X-Forwarded-For"),
				r.Header.Get("User-Agent"),
			)
		}
	}
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
	// Bind the live TCP connection to the session so the 100 ms RTT
	// sampler can read TCP_INFO off it. Lazily creates the session's
	// RTT window on first request; subsequent requests just refresh
	// the conn pointer (atomic). Issue #401.
	if tcpConn := tcpConnFromContext(r.Context()); tcpConn != nil {
		sessionStoreTCPConn(sessionData, tcpConn)
		sessionGetOrCreateRTTWindow(sessionData)
	}
	// Path-ping holder for issue #404 — created here (not in the
	// sampler) so the snapshot map mutation rides on handleProxy's
	// normal save path. Sampler reads via the atomic; never writes
	// to the map itself.
	sessionGetOrCreatePingRTT(sessionData)
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
		// getContentType parsed the UNMANIPULATED upstream master, so playlistInfo
		// is the full ladder. Keep the FULL set as manifest_variants_all: the
		// config/control panels (fault injection, the content-manipulation variant
		// picker, the shaping pattern) must enumerate EVERY available rung so a
		// DESELECTED variant stays listed and can be re-selected — not just the
		// allowed subset. Then thin manifest_variants to the session's
		// allowed_variants so the bandwidth chart (#815) and the per-session compare
		// (#820) keep reflecting the MANIPULATED master the player receives.
		// filterPlaylistInfoByAllowed allocates a fresh slice, so the _all reference
		// retains the full ladder.
		sessionData["manifest_variants_all"] = playlistInfo
		if allowed := getStringSlice(sessionData, "content_allowed_variants"); len(allowed) > 0 {
			playlistInfo = filterPlaylistInfoByAllowed(playlistInfo, allowed)
		}
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
				logEntry(sessionID, netEntry)
				// #740: persist the fault-path mutations as a single-session
				// atomic merge instead of re-publishing the stale full list
				// captured at handler top (which clobbered concurrent sessions).
				a.saveSessionByID(sessionNumber, sessionData)
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
				logEntry(sessionID, *netEntry)
				// #740: persist the fault-path mutations as a single-session
				// atomic merge instead of re-publishing the stale full list
				// captured at handler top (which clobbered concurrent sessions).
				a.saveSessionByID(sessionNumber, sessionData)
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
				logEntry(sessionID, *netEntry)
				// #740: persist the fault-path mutations as a single-session
				// atomic merge instead of re-publishing the stale full list
				// captured at handler top (which clobbered concurrent sessions).
				a.saveSessionByID(sessionNumber, sessionData)
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
			// TotalMs lift now happens uniformly in the logEntry closure (#613).
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
			logEntry(sessionID, *netEntry)
			// #740: single-session atomic merge (see note above) — was a
			// stale full-list re-publish.
			a.saveSessionByID(sessionNumber, sessionData)
			return
		}
		if isSocketFaultType(failureType) {
			socketAction, err := applySocketFault(w, failureType, contentType, upstreamURL)
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
			logEntry(sessionID, netEntry)
			// #740: single-session atomic merge (see note above) — was a
			// stale full-list re-publish.
			a.saveSessionByID(sessionNumber, sessionData)
			return
		}
		updateSessionTraffic(sessionData, requestBytes, 0)
		// #740: single-session atomic merge (see note above) — was a
		// stale full-list re-publish.
		a.saveSessionByID(sessionNumber, sessionData)
		status := http.StatusInternalServerError
		switch failureType {
		case "404":
			actionTaken = "http_404"
			status = http.StatusNotFound
		case "403":
			actionTaken = "http_403"
			status = http.StatusForbidden
		case "500":
			actionTaken = "http_500"
			status = http.StatusInternalServerError
		case "timeout":
			actionTaken = "http_504_timeout"
			status = http.StatusGatewayTimeout
		case "connection_refused":
			actionTaken = "http_503_connection_refused"
			status = http.StatusServiceUnavailable
		case "dns_failure":
			actionTaken = "http_502_dns_failure"
			status = http.StatusBadGateway
		case "rate_limiting":
			actionTaken = "http_429_rate_limited"
			status = http.StatusTooManyRequests
		default:
			// Generic numeric status: any 4xx/5xx code passed as the
			// failure type (e.g. "503", "429") is honored directly, with
			// its standard reason phrase. This removes the silent-500
			// footgun where an unlisted numeric type fell through to 500.
			// Non-numeric / out-of-range types still fall back to 500.
			if code, err := strconv.Atoi(failureType); err == nil && code >= 400 && code <= 599 {
				actionTaken = "http_" + failureType
				status = code
			} else {
				actionTaken = "http_500_unknown_failure"
				status = http.StatusInternalServerError
			}
		}
		w.WriteHeader(status)
		bumpFaultCounter(sessionData, failureType)
		logFaultEvent(sessionData, externalPort, failureType, requestKind, actionTaken)
		// Log network entry for HTTP faults
		sessionID := getString(sessionData, "session_id")
		netEntry := createFaultLogEntry(playerURL, upstreamURL, requestKind, failureType, actionTaken, status, requestBytes, requestReceivedAt)
		stampNetMeta(&netEntry, requestHeaders, queryString, nil)
		logEntry(sessionID, netEntry)
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
		logEntry(sessionID, netEntry)
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
		logEntry(sessionID, *netEntry)
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
		logEntry(sessionID, *netEntry)
		return
	}
	copyUpstreamHeaders(w, resp)
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("X-Session-ID", getString(sessionData, "session_number"))

	var bytesOut int64

	// Apply content manipulation. Master playlists get the full set (strip /
	// overstate / live-offset EXT-X-START). Media (VARIANT) playlists get the
	// live-offset rewrite too — HOLD-BACK + EXT-X-START live in the variant and
	// are what players actually key off, so a master-only rewrite has no effect
	// (#793, regression caught by server_content_test master_live_offset).
	liveOffsetOnVariant := isManifest && !isMasterManifest && getInt(sessionData, "content_live_offset") > 0
	if (isMasterManifest && shouldApplyContentManipulation(sessionData)) || liveOffsetOnVariant {
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("ERROR: Failed to read playlist body for manipulation: %v", err)
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(proxyCtx.Err(), context.DeadlineExceeded) {
				bumpFaultCounter(sessionData, "transfer_active_timeout")
				logFaultEvent(sessionData, externalPort, "transfer_active_timeout", requestKind, "transfer_active_timeout_mid_body")
			}
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		var modifiedBody []byte
		if isMasterManifest {
			modifiedBody, err = a.applyContentManipulation(bodyBytes, sessionData, contentType)
			if err != nil {
				log.Printf("ERROR: Failed to manipulate master playlist: %v", err)
				modifiedBody = bodyBytes // fall back to original
			}
		} else {
			// Media (variant) playlist: live-offset only — rewrite HOLD-BACK +
			// EXT-X-START to the requested value.
			modifiedBody = rewriteVariantLiveOffsetTags(bodyBytes, getInt(sessionData, "content_live_offset"))
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
	logEntry(sessionID, *netEntry)
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
	if getBool(session, "content_strip_resolution") {
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
	if vo := getString(session, "content_variant_order"); vo != "" && vo != "default" {
		return true
	}
	return false
}

// ContentManipulation bundles the per-session master-playlist / manifest
// manipulation knobs into one value so adding a future option is a struct
// field rather than another positional parameter rippling through every
// manipulate* signature and its callers. Built once from the session's
// content_* fields via newContentManipulation.
//
// VariantOrder is HLS-only — manipulateDASHManifest ignores it.
type ContentManipulation struct {
	StripCodecs        bool
	StripAvgBandwidth  bool
	StripResolution    bool
	OverstateBandwidth bool
	LiveOffset         int
	AllowedVariants    []string
	VariantOrder       string
}

// newContentManipulation reads the session's content_* fields into a
// ContentManipulation struct.
func newContentManipulation(session SessionData) ContentManipulation {
	return ContentManipulation{
		StripCodecs:        getBool(session, "content_strip_codecs"),
		StripAvgBandwidth:  getBool(session, "content_strip_average_bandwidth"),
		StripResolution:    getBool(session, "content_strip_resolution"),
		OverstateBandwidth: getBool(session, "content_overstate_bandwidth"),
		LiveOffset:         getInt(session, "content_live_offset"),
		AllowedVariants:    getStringSlice(session, "content_allowed_variants"),
		VariantOrder:       getString(session, "content_variant_order"),
	}
}

// applyContentManipulation modifies master playlist/manifest content based on session settings
func (a *App) applyContentManipulation(body []byte, session SessionData, contentType string) ([]byte, error) {
	cm := newContentManipulation(session)

	// Handle HLS master playlists
	if strings.Contains(strings.ToLower(contentType), "mpegurl") || strings.Contains(strings.ToLower(contentType), "m3u8") {
		result, err := manipulateHLSMaster(body, cm)
		if err != nil {
			return nil, err
		}
		// Variant playlists carry HOLD-BACK / PART-HOLD-BACK and their own
		// EXT-X-START tag (not all players honor master-level inheritance —
		// notably hls.js, which would otherwise park at the oldest segment).
		// Master EXT-X-START is rewritten inside manipulateHLSMaster; this
		// pass handles the variant side. No-op on master playlists.
		if cm.LiveOffset > 0 {
			result = rewriteVariantLiveOffsetTags(result, cm.LiveOffset)
		}
		return result, nil
	}

	// Handle DASH manifests
	if strings.Contains(strings.ToLower(contentType), "dash") || strings.Contains(strings.ToLower(contentType), "mpd") {
		return manipulateDASHManifest(body, cm)
	}

	return body, nil
}

// manipulateHLSMaster modifies an HLS master playlist
// variantAllowed reports whether a master variant is whitelisted by
// allowed_variants. It matches either the exact served URI (e.g.
// "playlist_6s_360p.m3u8" — back-compat) OR the variant's resolution: the full
// "640x360", the bare height "360", or "360p". Resolution matching lets a
// keep-set expressed in resolution terms (e.g. derived from the content
// catalogue's variants[], which is resolution-keyed) survive across segment
// durations whose served URIs differ — the harness/dashboard need not know the
// per-segment URI scheme.
func variantAllowed(v *m3u8.Variant, allowed map[string]bool) bool {
	return variantSelectorAllowed(v.URI, v.Resolution, allowed)
}

// variantSelectorAllowed is the shared allowed_variants matcher: an entry is
// kept if the allow-set contains its served URI, its full resolution
// ("640x360"), its bare height ("360"), or "360p". Both the master rewrite
// (variantAllowed) and the manifest_variants metric (filterPlaylistInfoByAllowed)
// route through it so they agree on exactly which rungs the player ends up with.
func variantSelectorAllowed(uri, resolution string, allowed map[string]bool) bool {
	if uri != "" && allowed[uri] {
		return true
	}
	if resolution == "" || resolution == "unknown" {
		return false
	}
	if allowed[resolution] {
		return true
	}
	if i := strings.LastIndex(resolution, "x"); i >= 0 {
		h := resolution[i+1:] // "360"
		if allowed[h] || allowed[h+"p"] {
			return true
		}
	}
	return false
}

// filterPlaylistInfoByAllowed thins a parsed master variant list to the
// allowed_variants keep-set, so the `manifest_variants` metric reflects the
// MANIPULATED master the player actually receives — not the unmanipulated
// upstream getContentType parsed (#820). Returns the input unchanged when no
// allow-set is configured.
func filterPlaylistInfoByAllowed(infos []PlaylistInfo, allowed []string) []PlaylistInfo {
	if len(allowed) == 0 || len(infos) == 0 {
		return infos
	}
	allowedMap := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		allowedMap[a] = true
	}
	out := make([]PlaylistInfo, 0, len(infos))
	for _, p := range infos {
		if variantSelectorAllowed(p.URL, p.Resolution, allowedMap) {
			out = append(out, p)
		}
	}
	return out
}

func manipulateHLSMaster(body []byte, cm ContentManipulation) ([]byte, error) {
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

	// Filter variants if allowed_variants is specified
	if len(cm.AllowedVariants) > 0 {
		allowedMap := make(map[string]bool)
		for _, v := range cm.AllowedVariants {
			allowedMap[v] = true
		}

		filteredVariants := make([]*m3u8.Variant, 0)
		for _, variant := range master.Variants {
			if variant != nil && variantAllowed(variant, allowedMap) {
				filteredVariants = append(filteredVariants, variant)
			}
		}

		if len(filteredVariants) != len(master.Variants) {
			master.Variants = filteredVariants
			modified = true
		}
	}

	// Strip codecs if requested
	if cm.StripCodecs {
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
	if cm.StripAvgBandwidth {
		for _, variant := range master.Variants {
			if variant != nil && variant.AverageBandwidth > 0 {
				variant.AverageBandwidth = 0
				modified = true
			}
		}
	}

	// Strip RESOLUTION if requested (issue #486). Drops the
	// EXT-X-STREAM-INF RESOLUTION=WxH attribute, leaving the variant
	// playable but with empty `AVAssetVariant.video.size`. Useful for
	// testing how players (and the AVMetrics VariantSwitchEvent
	// payload) handle missing resolution metadata. Apple's HLS
	// validator (mediastreamvalidator) rejects this; AVPlayer
	// continues but loses resolution-aware ABR and UI badges.
	if cm.StripResolution {
		for _, variant := range master.Variants {
			if variant != nil && variant.Resolution != "" {
				variant.Resolution = ""
				modified = true
			}
		}
	}

	// Overstate BANDWIDTH and AVERAGE-BANDWIDTH by 10% if requested
	if cm.OverstateBandwidth {
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

	// Reorder video variants by BANDWIDTH (issue #682). Probes whether
	// the master-playlist order biases AVPlayer's initial-variant pick —
	// the pre-iOS-13 / startsOnFirstEligibleVariant path keys off
	// first-listed. Re-sorts master.Variants in place; the m3u8 encoder
	// emits EXT-X-STREAM-INF lines in slice order. EXT-X-MEDIA audio/
	// subtitle renditions travel on each variant's Alternatives and stay
	// glued to their owning variant, so they are unaffected.
	switch cm.VariantOrder {
	case "ascending":
		sortVariantsByBandwidth(master.Variants, true)
		modified = true
	case "descending":
		sortVariantsByBandwidth(master.Variants, false)
		modified = true
	case "first_4mbps":
		// Promote the variant nearest 4 Mbps to first-listed (rest ascending)
		// to force a mid-tier initial pick on the first-eligible-variant path.
		promoteVariantNearestBandwidth(master.Variants, 4_000_000)
		modified = true
	}

	// Inject #EXT-X-START with negative offset for live edge positioning
	if cm.LiveOffset > 0 {
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
	if cm.LiveOffset > 0 {
		encoded := buf.String()
		startTag := fmt.Sprintf("#EXT-X-START:TIME-OFFSET=-%d,PRECISE=YES\n", cm.LiveOffset)
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

// variantBandwidth returns a variant's BANDWIDTH (peak) as the sort key,
// 0 for a nil variant.
func variantBandwidth(v *m3u8.Variant) uint32 {
	if v == nil {
		return 0
	}
	return v.Bandwidth
}

// sortVariantsByBandwidth re-sorts the variants in place by BANDWIDTH,
// ascending (lowest first) or descending. Stable so equal-bandwidth
// variants keep their authored relative order.
func sortVariantsByBandwidth(vs []*m3u8.Variant, ascending bool) {
	sort.SliceStable(vs, func(i, j int) bool {
		if ascending {
			return variantBandwidth(vs[i]) < variantBandwidth(vs[j])
		}
		return variantBandwidth(vs[i]) > variantBandwidth(vs[j])
	})
}

// promoteVariantNearestBandwidth orders the variants ascending, then moves
// the one whose BANDWIDTH is closest to target to the front — leaving a
// master playlist whose first-listed variant is the ~target-bitrate rendition.
// Used by the "first_4mbps" probe (#682).
func promoteVariantNearestBandwidth(vs []*m3u8.Variant, target uint32) {
	if len(vs) < 2 {
		return
	}
	sortVariantsByBandwidth(vs, true)
	best, bestDelta := 0, absDiffUint32(variantBandwidth(vs[0]), target)
	for i := 1; i < len(vs); i++ {
		if d := absDiffUint32(variantBandwidth(vs[i]), target); d < bestDelta {
			best, bestDelta = i, d
		}
	}
	if best == 0 {
		return
	}
	chosen := vs[best]
	copy(vs[1:best+1], vs[0:best])
	vs[0] = chosen
}

func absDiffUint32(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
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

// manipulateDASHManifest modifies a DASH manifest.
// Note: the ContentManipulation knobs are reserved for a future DASH
// implementation. cm.VariantOrder is HLS-only and intentionally ignored here.
func manipulateDASHManifest(body []byte, cm ContentManipulation) ([]byte, error) {
	// DASH manifest manipulation would require XML parsing and manipulation
	// using libraries like encoding/xml or third-party XML processors.
	// This is deferred to keep the initial implementation focused on HLS.
	_ = cm // Silence unused parameter warning
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
	np := netemParamsFromSession(session)
	// rate=0 in storage means "operator did not override." Resolve to
	// the deployment baseline before pushing to the kernel. Issue #480.
	effective := a.effectiveRate(rate)
	if err := a.applyShapeIfChanged(port, effective, np); err != nil {
		log.Printf("NETSHAPE apply failed port=%d rate=%g (effective=%g) delay=%d loss=%.2f jitter=%d loss_corr=%.1f del_corr=%.1f: %v", port, rate, effective, np.DelayMs, np.LossPct, np.JitterMs, np.LossCorrelationPct, np.JitterCorrelationPct, err)
		return
	}
}

func almostEqualShape(a ShapeApplyState, b ShapeApplyState) bool {
	const eps = 0.0001
	return a.delay == b.delay &&
		a.jitter == b.jitter &&
		math.Abs(a.rate-b.rate) <= eps &&
		math.Abs(a.loss-b.loss) <= eps &&
		math.Abs(a.lossCorr-b.lossCorr) <= eps &&
		math.Abs(a.delCorr-b.delCorr) <= eps
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

func (a *App) applyShapeIfChanged(port int, rate float64, np NetemParams) error {
	const eps = 0.0001
	delay, loss := np.DelayMs, np.LossPct
	desired := ShapeApplyState{
		rate: rate, delay: delay, loss: loss,
		jitter: np.JitterMs, lossCorr: np.LossCorrelationPct, delCorr: np.JitterCorrelationPct,
	}
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
	netemChanged := !ok || last.delay != delay || last.jitter != np.JitterMs ||
		math.Abs(last.loss-loss) > eps || math.Abs(last.lossCorr-np.LossCorrelationPct) > eps ||
		math.Abs(last.delCorr-np.JitterCorrelationPct) > eps
	if netemChanged {
		log.Printf("NETSHAPE apply netem_change port=%d from_delay_ms=%d to_delay_ms=%d from_loss_pct=%.3f to_loss_pct=%.3f jitter_ms=%d loss_corr_pct=%.1f del_corr_pct=%.1f", port, last.delay, delay, last.loss, loss, np.JitterMs, np.LossCorrelationPct, np.JitterCorrelationPct)
		if err := a.traffic.UpdateNetem(port, np); err != nil {
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
							URL:              variant.URI,
							Bandwidth:        int(variant.Bandwidth),
							AverageBandwidth: int(variant.AverageBandwidth),
							Resolution:       resolution,
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
		bytes      int64
		timestamp  time.Time
		samples    []throughputSample
		a1sHistory []a1sSample // rolling buffer of a1s values for a6s averaging
	}
	const (
		sampleInterval      = 100 * time.Millisecond
		shortWindow         = 1 * time.Second
		mediumWindow        = 6 * time.Second
		transferRateWindow  = 400 * time.Millisecond
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
				bytes:      bytesValue,
				timestamp:  now,
				samples:    state.samples,
				a1sHistory: state.a1sHistory,
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
			"bytes":                  deltaBytes,
			"wire_tc_bytes_now":      bytesValue,
			"timestamp":              now.Unix(),
			"timestamp_ms":           now.UnixMilli(),
			"mbps_shaper_rate":       mbpsShaperRate,
			"mbps_shaper_avg":        mbpsShaperAvg,
			"mbps_transfer_rate":     mbpsTransferRate,
			"mbps_transfer_complete": mbpsTransferComplete,
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
			sessions := a.sessionsView() // #740 read-only: collects ports for throughput refresh
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
	// #740: scan the no-clone view, return a clone of the single match —
	// callers mutate this (and feed it to normalizeSessionsForResponse, which
	// writes in place), so it must not alias the live snapshot.
	for _, session := range a.sessionsView() {
		if getString(session, "session_id") == identifier {
			return cloneSession(session)
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
		"nftables_jitter_ms",
		"nftables_loss_correlation_pct",
		"nftables_jitter_correlation_pct",
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
// already does this implicitly via the "if playID != ”" guard at the
// session level; without this fallback the network_requests table ends
// up with most rows attributed to play_id=” and the session-viewer's
// play_id filter only catches the master manifest hits.
func (a *App) addNetworkLogEntry(sessionID string, entry NetworkLogEntry) {
	if sessionID == "" {
		return
	}
	if entry.PlayID == "" {
		entry.PlayID = a.sessionStickyPlayID(sessionID)
	}
	if entry.AttemptID == 0 {
		entry.AttemptID = a.sessionStickyAttemptID(sessionID)
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
	return a.sessionStickyField(sessionID, "play_id")
}

// sessionStickyPlayerID is the player_id counterpart — sourced from
// the same session snapshot so control_events and other server-side
// emissions can stamp the canonical UUID without a parallel cache.
// Issue #474 Milestone B.
func (a *App) sessionStickyPlayerID(sessionID string) string {
	return a.sessionStickyField(sessionID, "player_id")
}

// sessionStickyAttemptID is the attempt_id counterpart of
// sessionStickyPlayID. The player increments attempt_id on every
// restart event (user-restart or auto-recovery); the proxy stores
// the latest value and stamps it onto network log entries that
// don't carry one. Returns 0 when nothing is stamped yet — the
// uint32 zero-value harmlessly leaves the entry unstamped on the
// CH side.
func (a *App) sessionStickyAttemptID(sessionID string) uint32 {
	v := a.sessionStickyField(sessionID, "attempt_id")
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseUint(v, 10, 32); err == nil {
		return uint32(n)
	}
	return 0
}

func (a *App) sessionStickyField(sessionID, field string) string {
	snap := a.sessionsSnap.Load()
	if snap == nil {
		return ""
	}
	for _, s := range *snap {
		id, _ := s["session_id"].(string)
		if id != sessionID {
			continue
		}
		v, _ := s[field].(string)
		return v
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

	// Time the upstream fetch itself: a.client.Do returns once the response
	// HEADERS arrive, so do_ms is how long the origin (nginx/go-live) took to
	// start answering. If the audio request's do_ms is ~1.5s while video's is
	// ~10ms, the stall is the origin fetch — not go-proxy's streaming.
	doStart := time.Now()
	resp, err := a.client.Do(req)
	doMs := float64(time.Since(doStart).Microseconds()) / 1000.0
	if err != nil {
		entry.TotalMs = float64(time.Since(start).Microseconds()) / 1000.0
		log.Printf("[UPSTREAM_DO] url=%s do_ms=%.1f err=%v", req.URL.String(), doMs, err)
		return nil, entry, err
	}
	log.Printf("[UPSTREAM_DO] url=%s do_ms=%.1f upstream_ttfb_ms=%.1f status=%d", req.URL.String(), doMs, entry.TTFBMs, resp.StatusCode)

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
	rev := controlRevision
	if rev == "" {
		rev = newControlRevision()
	}
	// Diagnostics captured inside the (re-runnable) CAS closure and logged
	// once on the committed result — see mutateSessions' side-effect rule.
	type netshapeLog struct {
		sessionID  string
		fwdPort    string
		fwdPortExt string
		beforeBW   interface{}
		afterBW    interface{}
	}
	var captured []netshapeLog
	a.mutateSessions(func(sessions []SessionData) ([]SessionData, bool) {
		captured = captured[:0]
		changed := false
		for _, session := range sessions {
			if a.sessionMatchesPort(session, port) {
				entry := netshapeLog{
					sessionID:  getString(session, "session_id"),
					fwdPort:    getString(session, "x_forwarded_port"),
					fwdPortExt: getString(session, "x_forwarded_port_external"),
					beforeBW:   session["nftables_bandwidth_mbps"],
				}
				for key, value := range updates {
					session[key] = value
				}
				applyControlRevision(session, rev)
				entry.afterBW = session["nftables_bandwidth_mbps"]
				captured = append(captured, entry)
				changed = true
			}
		}
		return sessions, changed
	})
	for _, e := range captured {
		log.Printf("NETSHAPE session_match port=%d session_id=%s before: x_forwarded_port=%s x_forwarded_port_external=%s nftables_bandwidth_mbps=%v",
			port, e.sessionID, e.fwdPort, e.fwdPortExt, e.beforeBW)
		log.Printf("NETSHAPE session_updated port=%d session_id=%s after: nftables_bandwidth_mbps=%v",
			port, e.sessionID, e.afterBW)
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
	a.mutateSessions(func(sessions []SessionData) ([]SessionData, bool) {
		changed := false
		for _, session := range sessions {
			if a.sessionMatchesPort(session, port) {
				for key, value := range updates {
					session[key] = value
				}
				changed = true
			}
		}
		return sessions, changed
	})
}

func (a *App) getSessionList() []SessionData {
	snap := a.sessionsSnap.Load()
	if snap == nil {
		return []SessionData{}
	}
	return cloneSessionList(*snap)
}

// sessionsView returns the current session snapshot WITHOUT cloning — a
// read-only borrow of the immutable published slice (issue #740 Commit B).
// The dominant per-read cost was cloneSession (a deep copy of ~110 fields × N
// sessions) on every getSessionList, and ~half the call sites only read.
//
// CONTRACT — callers MUST treat the result, and every map inside it, as
// read-only. This is sound only because writers never mutate in place:
// mutateSessions always copy-on-writes and CAS-publishes a fresh slice, so the
// snapshot a caller borrows is frozen for its lifetime (a concurrent writer
// publishes a new slice; it never touches this one). A caller that needs to
// mutate a session — or hands maps to normalizeSessionsForResponse, which
// writes in place — must use getSessionList (which clones) or cloneSession the
// specific map it retains.
func (a *App) sessionsView() []SessionData {
	snap := a.sessionsSnap.Load()
	if snap == nil {
		return nil
	}
	return *snap
}

// mutateSessions is the lock-free read-modify-write primitive for the
// in-memory session list (issue #740). It loads the current immutable
// snapshot, hands `fn` a PRIVATE deep clone it may freely mutate, and
// publishes the result via atomic CompareAndSwap — retrying from a fresh
// clone on every conflict so no concurrent writer's update is lost. This
// replaces the old `getSessionList → mutate → saveSessionList` pattern,
// which held no lock across the read and the write and was therefore a
// last-writer-wins lost-update race against every other writer.
//
// CONTRACT — `fn` MUST be pure / re-runnable:
//   - It receives a private clone (never the live snapshot) and returns the
//     new list plus a `changed` flag. Returning changed=false aborts with no
//     store (and no ui-version bump).
//   - It MUST NOT perform side effects (kernel/tc calls, control-event emits,
//     recordSessionEnd, resetServerLoopState, logging of committed state):
//     fn can run multiple times under contention. Hoist side effects OUT and
//     run them once on the committed result this returns — the idiomatic shape
//     is to reset a captured slice at the TOP of fn and append to it as fn
//     walks the list, so only the committed run's captures survive.
//
// Returns the published slice (the committed clone) and true on a successful
// store, or (nil, false) when fn reported no change.
func (a *App) mutateSessions(fn func([]SessionData) ([]SessionData, bool)) ([]SessionData, bool) {
	for {
		oldPtr := a.sessionsSnap.Load()
		var current []SessionData
		if oldPtr != nil {
			current = *oldPtr
		}
		// Hand fn a private deep clone: a retry starts clean and the
		// committed snapshot is never aliased by the caller.
		next, changed := fn(cloneSessionList(current))
		if !changed {
			return nil, false
		}
		// Stamp the ui-version on the to-be-published slice — same shape
		// the retired publishSnapshot used. A burned version on a lost CAS
		// is harmless (the field is monotonic, never read for an exact value).
		uiVersion := atomic.AddUint64(&a.uiStateVersionSeq, 1)
		uiRevision := newControlRevision()
		for _, session := range next {
			session["ui_state_version"] = uiVersion
			session["ui_state_revision"] = uiRevision
		}
		if a.sessionsSnap.CompareAndSwap(oldPtr, &next) {
			return next, true
		}
		// Lost the race — another writer published between our Load and
		// CompareAndSwap. Re-clone from the new snapshot and re-run fn.
	}
}

// Issue #470: the proxy stopped broadcasting the full session list on every
// snapshot publish. /api/sessions/stream is now a per-event channel driven by
// emitSessionEvent; the debounced full-state path served only the forwarder
// and produced duplicates in session_events as stale `player_metrics_last_event`
// markers leaked across emissions. The in-memory snapshot (sessionsSnap) is
// still maintained because GET /api/sessions reads from it.
//
// Issue #740: publishSnapshot and saveSessionList were retired in favour of
// mutateSessions (lock-free CAS) — every full-list write now composes the
// read, mutate, ui-version stamp, and store into one atomic step, so a
// concurrent writer can no longer clobber another's update. sessionsMu and
// createMu went with them.

func (a *App) saveSessionByID(sessionID string, session SessionData) {
	a.saveSessionByIDReturning(sessionID, session)
}

// saveSessionByIDReturning is saveSessionByID's worker; returns the
// merged session post-merge so callers (the metrics POST handler) can
// emit a per-event SSE frame containing exactly the row that was
// just written. Returns nil + false when the merge was dropped as
// stale (see isStalePlayerMetricsUpdate) so the caller knows not to
// emit.
//
// Issue #470: the per-event emission path consumes this; the
// pre-existing debounced full-state broadcast path is no longer used
// for /api/sessions/stream — publishSnapshot still maintains the
// in-memory snapshot for GET /api/sessions, but no longer queues a
// hub broadcast.
func (a *App) saveSessionByIDReturning(sessionID string, session SessionData) (SessionData, bool) {
	// #740: the merge is now a mutateSessions CAS closure rather than an
	// RMW under sessionsMu. metricsPostMu (held by the caller) still
	// serialises same-session POSTs for arrival ordering; the CAS guards
	// cross-session writers. resetServerLoopState is the one side effect and
	// is hoisted out to run once on the committed result.
	var merged SessionData
	var found bool
	var playRotated bool
	a.mutateSessions(func(updated []SessionData) ([]SessionData, bool) {
		merged = nil
		found = false
		playRotated = false
		for i, s := range updated {
			if getString(s, "session_id") != sessionID {
				continue
			}
			// Drop the merge if it's a player_metrics POST whose
			// `player_metrics_event_time` predates what we already have.
			// One goroutine per request means two near-simultaneous
			// POSTs can be scheduled out of submission order — the
			// older one would otherwise overwrite the newer one's
			// state under last-writer-wins, producing backward
			// event_time jumps in session_snapshots and zigzag charts
			// at step boundaries (issue #403 follow-up).
			if isStalePlayerMetricsUpdate(s, session) {
				return updated, false
			}
			merged = cloneSession(s)
			for k, v := range session {
				merged[k] = v
			}
			// Don't let a thinner user_agent overwrite a richer one
			// already on the session (issue #471). AVPlayer's
			// segment/manifest fetches arrive with a device-family
			// token ("AppleCoreMedia/… (iPad; …)"); the app's
			// URLSession metrics POSTs and HAR snapshot requests used
			// to land with a thinner "CFNetwork/… Darwin/…" string
			// that erased the iPad/iPhone/AppleTV label. iOS now
			// stamps its own User-Agent on every request, but keep
			// this guard for clients (web, Roku, future) that may
			// not.
			if existingUA := getString(s, "user_agent"); existingUA != "" {
				if incomingUA, ok := session["user_agent"].(string); ok {
					if hasDeviceFamilyToken(existingUA) && !hasDeviceFamilyToken(incomingUA) {
						merged["user_agent"] = existingUA
					}
				}
			}
			existingRevision := getString(s, "control_revision")
			incomingRevision := getString(session, "control_revision")
			if isControlRevisionNewer(existingRevision, incomingRevision) {
				copySessionControlState(merged, s)
			}
			// If the update didn't carry its own player_metrics_event_time
			// (i.e. it's a server-side trigger like a URL request or a
			// pattern step bump, not an iOS POST), stamp the proxy's
			// current wall clock so the snapshot anchors to when it was
			// emitted — not to the last iOS heartbeat. Without this,
			// non-metric snapshots inherit a stale event_time and the
			// session-viewer charts plot proxy-side state changes (limit
			// line, shaper rate) at the wrong x. Issue #403 follow-up.
			// Only push forward — never regress past an existing event_time.
			if _, hasIncoming := session["player_metrics_event_time"]; !hasIncoming {
				nowStr := time.Now().UTC().Format(time.RFC3339Nano)
				nowT, _ := parseEventTime(nowStr)
				if existingT, ok := parseEventTime(getString(s, "player_metrics_event_time")); !ok || nowT.After(existingT) {
					merged["player_metrics_event_time"] = nowStr
				}
			}
			// #587 — play_id rotation resets the proxy-accumulated per-play
			// counters so the new play measures from zero, mirroring the
			// clients' own per-play reset. Detected at this single merge
			// chokepoint (every GET/POST that stamps play_id flows through
			// here). retry()/auto-recovery bumps attempt_id but keeps play_id
			// stable, so this does NOT fire on recovery. Fault/shaping config,
			// session identity/timing, and control state are preserved.
			prevPlay := getString(s, "play_id")
			newPlay := getString(session, "play_id")
			if prevPlay != "" && newPlay != "" && prevPlay != newPlay {
				resetPlayScopedServerCounters(merged)
				playRotated = true
			}
			updated[i] = merged
			found = true
			break
		}
		return updated, found
	})
	// Hoisted out of the CAS closure (non-idempotent in-memory reset).
	if playRotated {
		a.resetServerLoopState(sessionID)
	}
	if !found {
		return nil, false
	}
	return cloneSession(merged), true
}

// fillReservedSession replaces the bootstrap placeholder (see the reserve CAS
// in handleProxy, #740) with the fully-built session, matched by session_id.
// A clone of `full` is published — the caller keeps mutating its own
// sessionData for the kernel apply / recordSessionStart that follow, exactly
// as the pre-#740 saveSessionList (which cloned at publish) allowed. If the
// reservation is somehow gone (e.g. reaped mid-bootstrap), the full session is
// appended so the bootstrap still completes.
func (a *App) fillReservedSession(sessionID string, full SessionData) {
	a.mutateSessions(func(sessions []SessionData) ([]SessionData, bool) {
		clone := cloneSession(full)
		for i, s := range sessions {
			if getString(s, "session_id") == sessionID {
				sessions[i] = clone
				return sessions, true
			}
		}
		return append(sessions, clone), true
	})
}

// removeReservedSession CAS-removes the bootstrap placeholder when config
// materialization fails, so a rejected config leaks no session slot (#740).
func (a *App) removeReservedSession(sessionID string) {
	a.mutateSessions(func(sessions []SessionData) ([]SessionData, bool) {
		filtered := make([]SessionData, 0, len(sessions))
		removed := false
		for _, s := range sessions {
			if getString(s, "session_id") == sessionID {
				removed = true
				continue
			}
			filtered = append(filtered, s)
		}
		return filtered, removed
	})
}

// resetFailureWindowState clears the persisted per-surface failure
// window cursor (`<prefix>_failure_at` / `<prefix>_failure_recover_at`)
// for every surface whose fault CONFIG the incoming settings payload
// touches (#643). The cursor is the engine's "where in the
// fault/recover cycle am I" state, written back by the per-request
// handlers; without this reset a re-arm RESUMES the previous arm's
// half-consumed window — e.g. arm `--consecutive 10`, consume 4, re-arm
// ×10 → only 6 more faults fire before the OLD recover point is hit and
// the rule silently goes quiet. The next matching request after a
// config change must always open a fresh window.
//
// Covers the `all` surface too — the normalization loops above it
// historically listed only segment/manifest/master_manifest.
func resetFailureWindowState(payload map[string]interface{}, target SessionData) {
	for _, prefix := range []string{"segment", "manifest", "master_manifest", "all"} {
		touched := false
		for _, suffix := range []string{"_failure_type", "_failure_frequency", "_consecutive_failures", "_failure_mode"} {
			if _, ok := payload[prefix+suffix]; ok {
				touched = true
				break
			}
		}
		if !touched {
			continue
		}
		delete(target, prefix+"_failure_at")
		delete(target, prefix+"_failure_recover_at")
	}
}

// resetPlayScopedServerCounters zeroes the proxy-ACCUMULATED counters that
// should restart at a fresh play (#587). Called from saveSessionByIDReturning
// when the player rotates play_id. Deliberately preserves fault/shaping
// CONFIG, session identity/timing (session_start_time, origination_*), and
// control state. The server-side loop-detection in-memory state is reset by
// the caller via resetServerLoopState.
//
// IMPORTANT — what is NOT reset, and why:
//   - The *_requests_count counters (manifest/master/segments/all) are the
//     fault-pattern CLOCK: FailureHandler.handleFailureCount compares the
//     running request count against the count-based *_failure_at /
//     *_failure_recover_at thresholds to decide when to fire. Zeroing the
//     count without rewinding those thresholds would suppress faults until
//     the count climbed back, desyncing operator fault patterns. Left
//     running so injection behaviour is unchanged across a play rotation.
//   - transport_fault_*_packets can be the "packets"-units cycle counter for
//     transport faults (and self-reset each on/off cycle), so they're left
//     alone too.
//
// The fault_count_* family below is purely a write-only reporting tally
// (bumpFaultCounter only writes; every read is reporting/init/projection),
// so resetting it does NOT affect fault firing.
func resetPlayScopedServerCounters(m SessionData) {
	// Server-side loop counter (in-memory seq state reset separately).
	m["loop_count_server"] = 0
	delete(m, "loop_count_server_last_at")
	// Cumulative byte totals + the rolling-window state behind the Mbps
	// derivations, so the new play measures throughput from zero. These feed
	// dashboard display only — no control decision reads them.
	m["bytes_in_total"] = int64(0)
	m["bytes_out_total"] = int64(0)
	m["bytes_in_last"] = int64(0)
	m["bytes_out_last"] = int64(0)
	delete(m, "bytes_last_ts")
	delete(m, "io_samples")
	delete(m, "active_io_samples")
	// Fault-injection REPORTING tally — one key per category
	// (fault_count_total, fault_count_socket_*, fault_count_request_*,
	// fault_count_transfer_*). Write-only; resetting does not change firing.
	for k := range m {
		if strings.HasPrefix(k, "fault_count_") {
			m[k] = 0
		}
	}
}

// hasDeviceFamilyToken reports whether the given User-Agent string
// carries a token the harness device resolver and the dashboard's
// device chips can identify. Used by saveSessionByID's user_agent
// merge guard so a thinner per-request UA can't overwrite a richer
// one already stored on the session. Issue #471.
//
// Match is case-insensitive on a small closed set. New device
// families (Android, Tizen, etc.) get added here when they ship.
func hasDeviceFamilyToken(ua string) bool {
	if ua == "" {
		return false
	}
	lower := strings.ToLower(ua)
	for _, tok := range []string{"ipad", "iphone", "appletv", "apple tv", "roku", "android"} {
		if strings.Contains(lower, tok) {
			return true
		}
	}
	return false
}

// isStalePlayerMetricsUpdate returns true when `incoming` carries a
// `player_metrics_event_time` strictly older than what `existing`
// already has. Non-metrics callers (no event_time on incoming) are
// never stale by this check, since their merges aren't bound to the
// player's clock at all.
func isStalePlayerMetricsUpdate(existing, incoming SessionData) bool {
	incomingTS, ok := parseEventTime(getString(incoming, "player_metrics_event_time"))
	if !ok {
		return false
	}
	existingTS, ok := parseEventTime(getString(existing, "player_metrics_event_time"))
	if !ok {
		return false
	}
	return incomingTS.Before(existingTS)
}

func parseEventTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
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
		// nftables_bandwidth_mbps holds the operator's raw intent —
		// 0 means "no override" (slider at min). Effective enforcement
		// resolves to the baseline at the kernel-apply call sites via
		// a.effectiveRate. The derived effective_rate_limit_mbps field
		// below makes the actual cap visible to charts and the dashboard
		// throttle line. Issue #480.
		setDefault("nftables_bandwidth_mbps", float64(0))
		// effective_rate_limit_mbps — kernel-enforced cap at this
		// instant. Resolves in priority order:
		//   1. Pattern step runtime (nftables_pattern_rate_runtime_mbps)
		//      when a pattern is enabled and running.
		//   2. Operator slider (nftables_bandwidth_mbps) when set (>0).
		//   3. Deployment baseline (INFINITE_STREAM_DEFAULT_RATE_MBPS).
		// 0 means truly uncapped (all three sources at 0). Stamped on
		// every snapshot so dashboards have a stable, always-present
		// field to draw the throttle line from. Issue #480.
		session["effective_rate_limit_mbps"] = a.effectiveRateForSession(session)
		setDefault("nftables_delay_ms", 0)
		setDefault("nftables_packet_loss", 0)
		setDefault("nftables_jitter_ms", 0)
		setDefault("nftables_loss_correlation_pct", 0)
		setDefault("nftables_jitter_correlation_pct", 0)
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
		setDefault("player_metrics_loop_count_delta", 0)
		bestMbps := bestVariantMbps(session)
		videoMbps := getFloat(session, "player_metrics_video_bitrate_mbps")
		if bestMbps > 0 && videoMbps > 0 {
			quality := (videoMbps / bestMbps) * 100
			session["player_metrics_video_quality_pct"] = math.Round(quality*100) / 100
		} else {
			delete(session, "player_metrics_video_quality_pct")
		}
		// Drain the 1 s RTT window onto the snapshot so this
		// broadcast carries fresh client_rtt_* fields. Issue #401.
		// NB: this also runs on `/api/sessions` GET requests (which
		// re-use this normalizer), so a poll racing the SSE flush
		// can leave one of them with empty `client_rtt_ms` — the
		// other path gets the previous-window replay via
		// drainAndReset's stale fallback. Acceptable for a
		// monitoring tool; flagging it here for future readers.
		drainSessionRTT(session)
		// Out-of-band ICMP path-ping (issue #404). Latest 1 Hz
		// sample stamped here as `client_path_ping_rtt_ms`.
		stampSessionPathPing(session)
		// Strip internal session keys that aren't meant for the
		// SSE / JSON projection — runtime handles, not metrics.
		// The authoritative copies live on the snapshot map and
		// are preserved across cloneSession (cloneInterface's
		// default arm passes unknown pointer types through).
		delete(session, "_lastTCPConn")
		delete(session, "_rttWindow")
		delete(session, "_pingRTTUs")
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

// emitSessionEvent broadcasts ONE session frame to /api/sessions/stream
// clients immediately, no debounce. Issue #470: replaces the
// debounced full-state broadcast as the way the forwarder learns
// about playback events. One iOS metrics POST → one emit → one
// session_snapshots row → N classified rows in session_events (one
// per eventclass rule that matched).
//
// The session payload is the post-merge view of the row that was
// just written (saveSessionByIDReturning's return value). It carries
// the player's just-received `player_metrics_last_event` exactly
// once: subsequent broadcasts (other than another metrics POST)
// don't read this stream anymore, so the leak that drove the
// duplicate-event bug in #469 is structurally impossible.
//
// Wire shape stays the existing `event: sessions\ndata: {...}` with
// a single-element Sessions array so the forwarder consumes it with
// no schema change.
func (a *App) emitSessionEvent(session SessionData) {
	if a.sessionsHub == nil || session == nil {
		return
	}
	normalized := a.normalizeSessionsForResponse([]SessionData{session})
	rev := atomic.AddUint64(&a.sessionsBroadcastSeq, 1)
	preMarshaled := a.buildSessionsEvent(normalized, rev, 0, nil)
	a.sessionsHub.Broadcast(normalized, rev, preMarshaled)
}

func (a *App) removeInactiveSessions() {
	// A player that makes no request for this long is treated as gone and
	// its session synthesized to a terminal inactive_timeout. Kept generous
	// (5m) so a *legitimately rebuffering* play isn't evicted mid-stream: a
	// characterization sweep that slams the cap from a 4K rung back to the
	// ~2 Mbps floor between cycles can stall ~50s+ while AVPlayer drains a
	// stranded 4K segment — well under any real abandonment but over the old
	// 60s window, which killed the play (and orphaned later cycles).
	const inactiveWindow = 5 * time.Minute
	// Pin the cut point once so a CAS retry can't flip a borderline session
	// active/inactive on micro-timing.
	now := time.Now()
	// removed/active captured inside the (re-runnable) CAS closure;
	// recordSessionEnd + kernel teardown run once on the committed result
	// (mutateSessions side-effect rule). active is reused for auto-ungroup.
	var removed []SessionData
	var active []SessionData
	a.mutateSessions(func(sessions []SessionData) ([]SessionData, bool) {
		removed = removed[:0]
		active = make([]SessionData, 0, len(sessions))
		for _, session := range sessions {
			lastRequest := getString(session, "last_request")
			if lastRequest == "" {
				continue
			}
			lastTime, err := time.Parse("2006-01-02T15:04:05.000", lastRequest)
			if err != nil {
				continue
			}
			if now.Sub(lastTime) < inactiveWindow {
				active = append(active, session)
			} else {
				removed = append(removed, session)
				// session removed from active list — no separate cleanup needed
			}
		}
		// Republish whenever the list is non-empty, matching the pre-#740
		// unconditional saveSessionList(active) (which also dropped sessions
		// with empty/unparseable last_request). Empty input → no-op.
		return active, len(sessions) > 0
	})
	removedPorts := map[int]struct{}{}
	for _, session := range removed {
		a.recordSessionEnd(session, "inactive_timeout")
		if port, err := strconv.Atoi(getString(session, "x_forwarded_port")); err == nil {
			removedPorts[port] = struct{}{}
		}
	}
	for port := range removedPorts {
		a.disablePatternForPort(port)
		a.armTransportFaultLoop(port, "none", 1, transportUnitsSeconds, 0)
	}
	// Auto-ungroup groups that have DECAYED to a single member — but never a
	// group that is still ASSEMBLING. A fleet (token `_G<num>` suffix or
	// CreateGroup) can pass through a transient 1-member state as its sims
	// connect sequentially; clearing the group then would PERMANENTLY break it,
	// because group_id is only derived at session creation and never re-derived.
	// So we collapse a singleton ONLY if we've previously seen its group with ≥2
	// concurrent members — tracked per-session via group_ever_multi. This is
	// mechanism-agnostic: it protects suffix groups and operator/API groups
	// alike while still cleaning up a real group that lost all but one member.
	type groupMember struct {
		sessionID string
		everMulti bool
	}
	groupMembers := map[string][]groupMember{}
	for _, session := range active {
		gid := getString(session, "group_id")
		if gid != "" {
			groupMembers[gid] = append(groupMembers[gid], groupMember{
				sessionID: getString(session, "session_id"),
				everMulti: getString(session, "group_ever_multi") == "1",
			})
		}
	}
	for _, members := range groupMembers {
		if len(members) >= 2 {
			// A real multi-member group right now — stamp every member so a
			// later decay-to-1 is distinguishable from a still-assembling group.
			for _, m := range members {
				if !m.everMulti {
					a.saveSessionByID(m.sessionID, SessionData{
						"session_id":       m.sessionID,
						"group_ever_multi": "1",
					})
				}
			}
			continue
		}
		// Exactly one member: ungroup only if this group was previously ≥2
		// (decayed). A never-multi singleton is still assembling — keep it.
		if members[0].everMulti {
			a.saveSessionByID(members[0].sessionID, SessionData{
				"session_id": members[0].sessionID,
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
		// Emit a control_event so the dashboard's per-play history
		// surfaces server-side loops alongside fault toggles + pattern
		// steps. Replaces the snapshot_failures TypeLoopServer marker.
		// Issue #474 Milestone B.
		info := fmt.Sprintf(`{"loop":%d,"segment_seq":%d}`, count, seq)
		a.emitControlEventForSession(sessionID, "proxy", "loop_server", info)
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

// requestScheme returns "https" or "http" for the incoming request.
// Used by redirect handlers so the per-session port URL in the
// Location header matches the dashboard's origin and the browser
// doesn't block the hop as mixed content. r.TLS is non-nil when the
// request arrived over TLS directly; falling back to
// X-Forwarded-Proto handles a future TLS-terminating reverse proxy
// upstream of go-proxy.
func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return proto
	}
	return "http"
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

// effectiveRate resolves an operator-requested rate (Mbps) against the
// deployment baseline. Requested 0 means "no override" — on prod
// (defaultRateMbps=0) it stays 0 (unlimited); on test-dev
// (defaultRateMbps>0) it becomes the baseline so the kernel always
// enforces the floor. Positive requests pass through unchanged. Used
// at the tc / nftables apply sites only — storage holds the raw
// operator intent. Issue #480.
func (a *App) effectiveRate(requested float64) float64 {
	if requested > 0 {
		return requested
	}
	if a != nil && a.defaultRateMbps > 0 {
		return float64(a.defaultRateMbps)
	}
	return 0
}

// effectiveRateForSession reports the cap the kernel is actually
// enforcing at this instant — pattern step rate first, then the
// operator slider, then the deployment baseline. Used for the
// snapshot-stamped `effective_rate_limit_mbps` so the dashboard's
// "Effective Limit" line tracks both slider AND pattern overrides
// without ambiguity. Issue #480 follow-up.
//
// Kernel-apply call sites do NOT use this — they already skip the
// pattern branch via the `nftables_pattern_enabled` guard upstream
// and call `effectiveRate(operatorRate)` directly.
func (a *App) effectiveRateForSession(session SessionData) float64 {
	if getBool(session, "nftables_pattern_enabled") {
		if pr := getFloat(session, "nftables_pattern_rate_runtime_mbps"); pr > 0 {
			return pr
		}
	}
	return a.effectiveRate(getFloat(session, "nftables_bandwidth_mbps"))
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
	latest := a.sessionsView() // #740 read-only: reads matched session into dst, never writes the snapshot
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
	return a.sessionsView() // #740 read-only: callers (getGroupIdByPort/getPortsForGroup) only read
}

// updateSessionGroup updates all sessions in a group with the given updates
func (a *App) updateSessionGroup(groupID string, updates map[string]interface{}) {
	if groupID == "" {
		return
	}
	a.mutateSessions(func(sessions []SessionData) ([]SessionData, bool) {
		changed := false
		for _, session := range sessions {
			if getString(session, "group_id") == groupID {
				for key, value := range updates {
					session[key] = value
				}
				changed = true
			}
		}
		return sessions, changed
	})
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
