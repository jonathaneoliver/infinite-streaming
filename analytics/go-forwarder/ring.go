// Per-(session_id, play_id) in-memory ring buffer that mirrors every
// row the forwarder ingests until ClickHouse has confirmed the insert.
// Two purposes:
//
//   1. The read side of the /api/v2/timeseries endpoint can answer
//      "what's the latest sample/network entry?" from RAM without
//      waiting on the 100–250 ms ingest-to-visible latency that the
//      JSONEachRow batch path imposes. The ring covers the freshness
//      gap and, sized larger, can also serve the full default focus
//      window from RAM so most live queries never touch ClickHouse.
//
//   2. Correctness when the read API straddles the visibility seam.
//      The read path ALWAYS reads ring + ClickHouse and dedupes by
//      fingerprint. Entries flip ring → ClickHouse-visible via three
//      states (pending → inserting → confirmed); confirmed entries
//      are eligible for age-based eviction, pending/inserting are
//      sticky regardless of age (they exist nowhere else yet, losing
//      them would be a read-side data loss).
//
// Sharded by session_id to bound mutex contention as live-player
// count grows. Subscribers (the SSE handler) get a fan-out channel
// per (session, play) so they receive each new entry the moment it's
// Add()'d without polling.
package main

import (
	"hash/fnv"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ringKind discriminates the two stream payloads carried by the ring.
// session_events are derived at query time (today via ClickHouse SQL
// multiIf), so they don't pass through the ring.
type ringKind uint8

const (
	kindSample ringKind = iota
	kindNetwork
)

// ringStatus tracks where a row is in the write pipeline. Eviction is
// gated on this; only `confirmed` entries are eligible to be dropped
// from the ring by age, since they live in ClickHouse too.
type ringStatus uint8

const (
	statusPending ringStatus = iota
	statusInserting
	statusConfirmed
)

// ringEntry is one item in the ring. Status is read/written via
// atomic so subscribers and the eviction goroutine don't need the
// bucket lock to observe state transitions.
type ringEntry struct {
	Kind        ringKind
	TsMs        int64
	Fingerprint string
	// PlayID stamped at ingest. The read path can filter by this when
	// the caller passes an explicit play_id. Empty string is allowed
	// for rows the proxy hadn't yet stamped.
	PlayID  string
	status  atomic.Uint32 // ringStatus
	Payload interface{}   // *row for samples, *netRow for network
}

func (e *ringEntry) Status() ringStatus    { return ringStatus(e.status.Load()) }
func (e *ringEntry) setStatus(s ringStatus) { e.status.Store(uint32(s)) }

// ringKey identifies a per-player_id bucket. play_id is NOT part of
// the key — it's stored on each ringEntry as a payload field, and the
// read path filters by it if the caller asked. Keying on player_id
// only matches the dashboard's "show me this device, follow it across
// plays" model: a single subscription receives every row for the
// player, and the play rotates underneath without re-subscription.
type ringKey struct {
	PlayerID string
}

// ringBucket holds the ordered entries for one (session, play). The
// slice is append-only on Add(); eviction rewrites it in place. We
// keep it sorted by arrival (which is mostly ts-ascending; out-of-
// order arrivals are rare enough that the read API resorts).
type ringBucket struct {
	mu          sync.RWMutex
	entries     []*ringEntry
	subscribers []chan<- *ringEntry
	// lastActivityMs is the wall-clock ms of the most recent Add or
	// subscriber registration on this bucket. Buckets idle longer than
	// the ring's retention window are candidates for full eviction.
	lastActivityMs int64
}

// Ring is the global per-forwarder cache. Sharded by session_id so
// reads/writes of different sessions don't fight a single mutex.
type Ring struct {
	windowMs   int64
	shards     []ringShard
	subBufSize int
}

type ringShard struct {
	mu      sync.RWMutex
	buckets map[ringKey]*ringBucket
}

const ringShardCount = 32

// NewRing constructs a Ring with the given retention window.
//
// windowMs governs how long confirmed entries linger after CH ingest.
// Common values:
//
//	   30s — closes only the ingest-batch latency gap.
//	  600s (10m) — covers the dashboard's default focus window so
//	               most live queries skip ClickHouse entirely.
//	 1800s (30m) — generous live-tail; CH consulted only on pan-back.
//
// subBufSize controls how many entries each subscriber channel
// buffers before pushes drop. The SSE handler reads in a tight loop
// so 64 is plenty in practice; sized small to keep slow consumers
// from holding rows in RAM.
func NewRing(windowMs int64, subBufSize int) *Ring {
	if subBufSize <= 0 {
		subBufSize = 64
	}
	r := &Ring{
		windowMs:   windowMs,
		shards:     make([]ringShard, ringShardCount),
		subBufSize: subBufSize,
	}
	for i := range r.shards {
		r.shards[i].buckets = make(map[ringKey]*ringBucket)
	}
	return r
}

func (r *Ring) shardFor(k ringKey) *ringShard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(k.PlayerID))
	return &r.shards[h.Sum32()%ringShardCount]
}

// bucket returns the bucket for k, creating it on demand. The
// returned bucket is owned by the ring; caller holds no lock.
func (r *Ring) bucket(k ringKey) *ringBucket {
	sh := r.shardFor(k)
	sh.mu.RLock()
	b, ok := sh.buckets[k]
	sh.mu.RUnlock()
	if ok {
		return b
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if b, ok := sh.buckets[k]; ok {
		return b
	}
	b = &ringBucket{}
	sh.buckets[k] = b
	return b
}

// Add stores an entry in the bucket for k. Status starts as pending.
// Returns the stored entry so the ingest pipeline can flip its status
// later via MarkInserting/MarkConfirmed.
//
// Subscribers receive the entry via a non-blocking send; if a
// subscriber's channel is full, the entry is dropped for that
// subscriber (the slow consumer is responsible for catching up via a
// fresh range query). Add never blocks on subscribers.
func (r *Ring) Add(k ringKey, kind ringKind, tsMs int64, fingerprint, playID string, payload interface{}) *ringEntry {
	e := &ringEntry{
		Kind:        kind,
		TsMs:        tsMs,
		Fingerprint: fingerprint,
		PlayID:      playID,
		Payload:     payload,
	}
	e.setStatus(statusPending)

	b := r.bucket(k)
	// Append and fan out under the bucket's WRITE lock. The fan-out
	// is bounded (select-default drops on full subscriber channels)
	// so it doesn't sit on the lock for long. Holding the lock during
	// the send is what eliminates the race with cancelSubscribe's
	// close(ch): a closed channel will never be on the subscriber
	// list when this fan-out runs.
	b.mu.Lock()
	b.entries = append(b.entries, e)
	b.lastActivityMs = nowMs()
	for _, ch := range b.subscribers {
		select {
		case ch <- e:
		default:
			// Slow subscriber — drop this push. The subscriber's
			// reconnect / range-query path will recover the row.
		}
	}
	b.mu.Unlock()
	return e
}

// MarkInserting flips a batch of pending entries to inserting just
// before the ClickHouse INSERT is sent. Idempotent w.r.t. already-
// confirmed entries (a confirmed entry stays confirmed).
func (r *Ring) MarkInserting(entries []*ringEntry) {
	for _, e := range entries {
		if e.Status() == statusPending {
			e.setStatus(statusInserting)
		}
	}
}

// MarkConfirmed flips a batch of inserting entries to confirmed after
// ClickHouse ACKs the INSERT. Confirmed entries are eligible for
// age-based eviction.
func (r *Ring) MarkConfirmed(entries []*ringEntry) {
	for _, e := range entries {
		e.setStatus(statusConfirmed)
	}
}

// RevertInserting flips a batch of inserting entries back to pending
// when ClickHouse rejects the INSERT. Caller retries the batch.
func (r *Ring) RevertInserting(entries []*ringEntry) {
	for _, e := range entries {
		if e.Status() == statusInserting {
			e.setStatus(statusPending)
		}
	}
}

// Range returns entries for k whose kind is in `kinds` and whose ts
// falls within `[fromMs, toMs]` (inclusive). Returned slice is sorted
// ascending by TsMs.
//
// The caller is expected to ALSO query ClickHouse for the same range
// and dedupe the union by (Kind, Fingerprint). The ring contains
// every row not-yet-confirmed plus a tail of recently-confirmed rows
// that haven't been evicted yet; ClickHouse contains every confirmed
// row. The two overlap; the dedupe is required.
//
// Passing `kinds=nil` returns both kinds.
func (r *Ring) Range(k ringKey, fromMs, toMs int64, kinds []ringKind) []*ringEntry {
	b := r.bucket(k)
	b.mu.RLock()
	out := make([]*ringEntry, 0, len(b.entries))
	for _, e := range b.entries {
		if e.TsMs < fromMs || e.TsMs > toMs {
			continue
		}
		if kinds != nil && !kindMatches(kinds, e.Kind) {
			continue
		}
		out = append(out, e)
	}
	b.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].TsMs < out[j].TsMs })
	return out
}

func kindMatches(kinds []ringKind, k ringKind) bool {
	for _, want := range kinds {
		if want == k {
			return true
		}
	}
	return false
}

// Subscribe registers ch to receive every subsequent ringEntry Add()'d
// to the bucket for k. The returned func detaches ch when the caller
// is done (e.g. SSE client disconnect). Buffered channels should be
// preferred; sends drop on full channel.
func (r *Ring) Subscribe(k ringKey) (<-chan *ringEntry, func()) {
	b := r.bucket(k)
	ch := make(chan *ringEntry, r.subBufSize)
	b.mu.Lock()
	b.subscribers = append(b.subscribers, ch)
	b.lastActivityMs = nowMs()
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		for i, sub := range b.subscribers {
			if sub == ch {
				// Order doesn't matter; swap-and-truncate.
				b.subscribers[i] = b.subscribers[len(b.subscribers)-1]
				b.subscribers = b.subscribers[:len(b.subscribers)-1]
				break
			}
		}
		b.mu.Unlock()
		// Drain any stragglers so the sender's select-default doesn't
		// see a blocked channel that's actually been cancelled.
		close(ch)
	}
	return ch, cancel
}

// EvictOlderThan trims confirmed entries with TsMs < cutoffMs across
// all buckets, and drops whole buckets whose lastActivityMs falls
// before cutoffMs (no activity for a full retention window means the
// (session, play) is done and won't be queried live again).
//
// Called periodically by a background goroutine. Pending/inserting
// entries are sticky regardless of age — they exist nowhere else.
func (r *Ring) EvictOlderThan(cutoffMs int64) (evictedEntries, evictedBuckets int) {
	for sh := range r.shards {
		shard := &r.shards[sh]
		shard.mu.Lock()
		for key, b := range shard.buckets {
			b.mu.Lock()
			// Filter entries: keep anything not-yet-confirmed, or
			// confirmed but young enough. Append-only slice, so we
			// rebuild it (typical churn keeps the slice short).
			kept := b.entries[:0]
			for _, e := range b.entries {
				if e.Status() == statusConfirmed && e.TsMs < cutoffMs {
					evictedEntries++
					continue
				}
				kept = append(kept, e)
			}
			b.entries = kept

			// If the bucket has been idle past the retention window
			// AND has no surviving entries AND no subscribers, drop
			// the whole bucket. Active subscribers keep it alive even
			// if entries have all been evicted (a fresh play that
			// just connected before any tick lands).
			if len(b.entries) == 0 && len(b.subscribers) == 0 && b.lastActivityMs < cutoffMs {
				b.mu.Unlock()
				delete(shard.buckets, key)
				evictedBuckets++
				continue
			}
			b.mu.Unlock()
		}
		shard.mu.Unlock()
	}
	return evictedEntries, evictedBuckets
}

// StartEvictor spawns a goroutine that calls EvictOlderThan once per
// `interval`. Cancel via ctx. Safe to leave running for the life of
// the forwarder process.
func (r *Ring) StartEvictor(stop <-chan struct{}, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				cutoff := nowMs() - r.windowMs
				r.EvictOlderThan(cutoff)
			}
		}
	}()
}

// nowMs returns wall-clock now as ms since epoch. Centralised so a
// test harness can swap it out via a build tag if we ever need to.
func nowMs() int64 {
	return time.Now().UnixMilli()
}
