// Package eventclass classifies session_snapshots / network_requests
// rows into typed Event values at ingest time (forwarder write path),
// replacing the read-time multi-CTE UNION-ALL SQL in events_query.go.
//
// Why write-time: events emitted at ingest carry the same identity
// provenance as the source row — player_id, play_id, session_id, AND
// attempt_id (the UInt32 sticky counter that the read-time SQL had
// no way to plumb without a temporal join). They also drop the
// per-query CH cost of the 22-branch UNION ALL.
//
// How to add a new classifier:
//
//  1. Drop a file in this package — e.g. snapshot_state.go for a new
//     state-machine derived event, network_status.go for a new HTTP
//     status code bucket.
//  2. Implement either SnapshotClassifier or NetworkClassifier (or
//     both — most cause/effect pairs span both surfaces).
//  3. In that file's init(), call Register(NAME, classifier{}).
//
// Each registered classifier returns []Event for a single source row
// (zero events when no condition triggers). The forwarder's ingest
// path calls every classifier on every row, batches the resulting
// Events, and inserts them into infinite_streaming.session_events.
//
// Maintenance contract with the legacy SQL:
//   - The set of `type` strings produced here is the same set the
//     events_query.go template emitted (stall, restart, downshift,
//     http_5xx, etc.). Dashboards / harness consumers don't see a
//     shape change at cutover.
//   - The `info` string format is type-specific and matches the
//     legacy SQL ("2.34s", "503 GET /seg.m4s", "29.86→15.36 Mbps").
//   - kind ∈ {"cause", "effect"} and priority ∈ {1,2,3,4} follow the
//     same multiIf tables used in the read-time SQL (see Kind and
//     Priority helpers below).
package eventclass

import (
	"hash/fnv"
	"strconv"
	"strings"
	"sync"
)

// Event is one classified row destined for the session_events table.
// Field shapes mirror the legacy events_query.go SELECT projection so
// the table's column types (and any downstream consumer) stay stable
// across the cutover.
type Event struct {
	// Ts is the source row's timestamp in the same string form CH
	// returns from session_snapshots / network_requests
	// ("YYYY-MM-DD HH:MM:SS.fff"). Kept as a string so the insert
	// path can pass it straight through without a parse/format
	// round-trip.
	Ts string

	// Identity fields — copied verbatim from the source row so an
	// event carries the same (player, play, attempt, session)
	// provenance as the snapshot / network row that produced it.
	PlayerID  string
	PlayID    string
	AttemptID uint32
	SessionID string

	// Type is the closed-set event identifier (see TypeStall et al.
	// below). LowCardinality(String) on the CH side.
	Type string

	// Info is a short, type-specific display string. Free-form
	// otherwise.
	Info string

	// Classification mirrors the source row's retention tier — the
	// event ages out on the same schedule as the snapshot/network
	// row that produced it.
	Classification string
}

// Kind returns "cause" for fault-class types and "effect" otherwise.
// Same multiIf table the legacy events_query.go used so dashboard
// "sort cause first" behaviour stays stable.
func (e Event) Kind() string {
	switch e.Type {
	case TypeMasterManifestFailure, TypeAllFailure,
		TypeManifestFailure, TypeSegmentFailure,
		TypeTransportFailure,
		TypeTransferActiveTimeout, TypeTransferIdleTimeout,
		TypeFaultOn, TypeFaultOff,
		TypeHTTP5xx, TypeHTTP4xx,
		TypeRequestTimeout, TypeRequestIncomplete, TypeRequestFaulted,
		TypeSlowRequest, TypeSlowSegment, TypeRequestRetry,
		TypeLoopServer:
		return "cause"
	default:
		return "effect"
	}
}

// Priority returns 1..4 (1 = critical, 4 = low). Identical to the
// legacy SQL's multiIf priority table — see eventsSQLTemplate's
// `multiIf` block.
//
// `stallDurationS` is the parsed duration for stall events, used to
// promote long stalls to priority 1; callers pass 0 for non-stalls.
func (e Event) Priority(stallDurationS float64) uint8 {
	switch e.Type {
	case TypeUserMarked, TypeError, TypeMasterManifestFailure, TypeAllFailure:
		return 1
	case TypeStall:
		if stallDurationS >= 3 {
			return 1
		}
		return 2
	case TypeRestart:
		return 2
	case TypeManifestFailure, TypeSegmentFailure,
		TypeTransportFailure,
		TypeTransferActiveTimeout, TypeTransferIdleTimeout,
		TypeFaultOn, TypeFaultOff,
		TypeDownshift, TypeTimejump, TypeBuffering:
		return 3
	case TypeHTTP5xx, TypeRequestTimeout:
		return 2
	case TypeHTTP4xx, TypeRequestIncomplete, TypeRequestFaulted,
		TypeSlowRequest, TypeSlowSegment:
		return 3
	case TypeRequestRetry, TypeUpshift, TypePlaybackStart, TypeLoopServer:
		return 4
	default:
		return 3
	}
}

// Fingerprint is a stable hash over (player, play, ts, type, info)
// used by the session_events table's ORDER BY tuple to dedupe
// re-ingests of the same source row (forwarder restart, SSE reconnect).
func (e Event) Fingerprint() uint64 {
	h := fnv.New64a()
	h.Write([]byte(e.PlayerID))
	h.Write([]byte{0})
	h.Write([]byte(e.PlayID))
	h.Write([]byte{0})
	h.Write([]byte(e.Ts))
	h.Write([]byte{0})
	h.Write([]byte(e.Type))
	h.Write([]byte{0})
	h.Write([]byte(e.Info))
	return h.Sum64()
}

// Closed event-type set. Same strings the legacy events_query.go SQL
// emitted — preserved verbatim so dashboards keep filtering on the
// same identifiers after the cutover.
const (
	TypeStall                 = "stall"
	TypeBuffering             = "buffering"
	TypeRestart               = "restart"
	TypePlaybackStart         = "playback_start"
	TypeDownshift             = "downshift"
	TypeUpshift               = "upshift"
	TypeTimejump              = "timejump"
	TypeError                 = "error"
	TypeUserMarked            = "user_marked"
	TypeMasterManifestFailure = "master_manifest_failure"
	TypeAllFailure            = "all_failure"
	TypeManifestFailure       = "manifest_failure"
	TypeSegmentFailure        = "segment_failure"
	TypeTransportFailure      = "transport_failure"
	TypeTransferActiveTimeout = "transfer_active_timeout"
	TypeTransferIdleTimeout   = "transfer_idle_timeout"
	TypeFaultOn               = "fault_on"
	TypeFaultOff              = "fault_off"
	TypeLoopServer            = "loop_server"
	TypeHTTP5xx               = "http_5xx"
	TypeHTTP4xx               = "http_4xx"
	TypeRequestTimeout        = "request_timeout"
	TypeRequestIncomplete     = "request_incomplete"
	TypeRequestFaulted        = "request_faulted"
	TypeSlowRequest           = "slow_request"
	TypeSlowSegment           = "slow_segment"
	TypeRequestRetry          = "request_retry"
)

// Snapshot is the slice of a session_snapshots row that classifiers
// need. The forwarder projects its internal `row` into Snapshot
// before invoking classifiers — keeping eventclass independent of the
// forwarder package means new classifier files don't pull in the
// whole ingest path. Field names mirror the source struct so the
// projection is mechanical.
//
// Only fields actually read by classifiers are listed; add more as
// new classifiers need them.
type Snapshot struct {
	Ts             string
	PlayerID       string
	PlayID         string
	AttemptID      uint32
	SessionID      string
	Classification string

	LastEvent     string
	PlayerError   string
	VideoBitrate  float32

	// Counter cohort — the read-time SQL detected events on the
	// monotonic-increase boundary of each one. Same semantics here.
	ManifestConsecutiveFailures        uint32
	SegmentConsecutiveFailures         uint32
	MasterManifestConsecutiveFailures  uint32
	AllConsecutiveFailures             uint32
	TransportConsecutiveFailures       uint32
	FaultCountTransferActiveTimeout    uint32
	FaultCountTransferIdleTimeout      uint32
	LoopCountServer                    uint32

	// Transport fault toggle — fault_on/fault_off fire on the
	// 0→1 / 1→0 edge of this field.
	TransportFaultActive uint8
}

// NetworkRequest is the slice of a network_requests row classifiers
// need. Same projection rationale as Snapshot.
type NetworkRequest struct {
	Ts             string
	PlayerID       string
	PlayID         string
	AttemptID      uint32
	SessionID      string
	Classification string

	Method        string
	Path          string
	URL           string
	Status        uint16
	Faulted       uint8
	FaultType     string
	ClientWaitMs  float32
	TransferMs    float32
}

// SnapshotClassifier inspects a (prev, cur) snapshot pair and emits
// zero or more Events. prev may be nil — the first snapshot of a play
// has no predecessor. Classifiers that need a predecessor (e.g. the
// counter-bump detectors) MUST handle the nil-prev case (typically by
// returning no events).
type SnapshotClassifier interface {
	Classify(prev, cur *Snapshot) []Event
}

// NetworkClassifier inspects a single network_request row and emits
// zero or more Events. Most network-side classifiers are stateless
// per-row; the request_retry detector that needs prev-state lives in
// its own file and threads its own cache.
type NetworkClassifier interface {
	ClassifyRequest(req *NetworkRequest) []Event
}

// Registry holds the classifier sets that each event type registered
// at init() time. Concurrent reads only (after init); the slices
// themselves are not mutated post-init so they can be ranged without
// locks from the ingest goroutine.
type Registry struct {
	mu       sync.RWMutex
	snapshot map[string]SnapshotClassifier
	network  map[string]NetworkClassifier
}

var defaultRegistry = &Registry{
	snapshot: make(map[string]SnapshotClassifier),
	network:  make(map[string]NetworkClassifier),
}

// RegisterSnapshot adds a SnapshotClassifier to the default registry
// under the given name (used for logging / debug; not the event type
// string itself). Call from a classifier file's init().
func RegisterSnapshot(name string, c SnapshotClassifier) {
	defaultRegistry.mu.Lock()
	defer defaultRegistry.mu.Unlock()
	if _, dup := defaultRegistry.snapshot[name]; dup {
		panic("eventclass: duplicate snapshot classifier name: " + name)
	}
	defaultRegistry.snapshot[name] = c
}

// RegisterNetwork mirrors RegisterSnapshot for per-request classifiers.
func RegisterNetwork(name string, c NetworkClassifier) {
	defaultRegistry.mu.Lock()
	defer defaultRegistry.mu.Unlock()
	if _, dup := defaultRegistry.network[name]; dup {
		panic("eventclass: duplicate network classifier name: " + name)
	}
	defaultRegistry.network[name] = c
}

// ClassifySnapshot runs every registered SnapshotClassifier against
// (prev, cur) and returns the concatenated events. Order within a
// single (prev, cur) call is unspecified; events for distinct rows
// preserve source-row order because the forwarder calls this once per
// incoming snapshot.
func ClassifySnapshot(prev, cur *Snapshot) []Event {
	if cur == nil {
		return nil
	}
	defaultRegistry.mu.RLock()
	defer defaultRegistry.mu.RUnlock()
	var out []Event
	for _, c := range defaultRegistry.snapshot {
		out = append(out, c.Classify(prev, cur)...)
	}
	return out
}

// ClassifyNetwork runs every registered NetworkClassifier against the
// given request and returns the concatenated events.
func ClassifyNetwork(req *NetworkRequest) []Event {
	if req == nil {
		return nil
	}
	defaultRegistry.mu.RLock()
	defer defaultRegistry.mu.RUnlock()
	var out []Event
	for _, c := range defaultRegistry.network {
		out = append(out, c.ClassifyRequest(req)...)
	}
	return out
}

// Snapshot of currently-registered classifier names — useful for
// startup logging so operators can confirm the binary's classifier
// set without grepping the package directory.
func RegisteredNames() (snapshot, network []string) {
	defaultRegistry.mu.RLock()
	defer defaultRegistry.mu.RUnlock()
	for k := range defaultRegistry.snapshot {
		snapshot = append(snapshot, k)
	}
	for k := range defaultRegistry.network {
		network = append(network, k)
	}
	return snapshot, network
}

// FormatBitrateShift returns the legacy SQL's "X.XX→Y.YY Mbps" info
// string for downshift / upshift events. Centralised here so the
// rate-shift classifier and any test that asserts string equality
// against the legacy SQL output share the same formatter.
func FormatBitrateShift(prev, cur float32) string {
	return formatFloat2(float64(prev)) + "→" + formatFloat2(float64(cur)) + " Mbps"
}

// FormatDurationS returns "X.XXs" — used by stall / buffering info.
func FormatDurationS(s float64) string {
	return formatFloat2(s) + "s"
}

// FormatHTTPInfo returns "STATUS METHOD PATH" — used by http_5xx /
// http_4xx info, matching the legacy SQL's concat(...).
func FormatHTTPInfo(status uint16, method, path string) string {
	var b strings.Builder
	b.WriteString(strconv.FormatUint(uint64(status), 10))
	b.WriteByte(' ')
	b.WriteString(method)
	b.WriteByte(' ')
	b.WriteString(path)
	return b.String()
}

func formatFloat2(v float64) string {
	return strconv.FormatFloat(v, 'f', 2, 64)
}
