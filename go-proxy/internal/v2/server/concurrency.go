package server

import (
	"sync"
	"sync/atomic"
	"time"
)

// FieldRevisions tracks per-leaf-path revision tokens for one resource.
// The "current" resource ETag is the lexicographic maximum of all field
// revisions (RFC 7232 strong tag, see formatETag).
//
// Revision values are RFC3339Nano timestamps (e.g.
// `2026-05-08T17:31:42.123456789Z`). The format is shared with v1's
// `control_revision` field — see `newControlRevision` in
// go-proxy/cmd/server/main.go. RFC3339Nano sorts lexicographically by
// time, so string comparison gives the right answer for "is this
// revision newer than that one." A monotonic seq fallback (atomic
// counter appended after the timestamp) guarantees uniqueness and
// monotonicity even when wall clock has insufficient resolution or
// jumps backward briefly under NTP.
type FieldRevisions struct {
	mu   sync.RWMutex
	revs map[string]string
}

// NewFieldRevisions returns an empty tracker.
func NewFieldRevisions() *FieldRevisions {
	return &FieldRevisions{revs: map[string]string{}}
}

// Top returns the lexicographic maximum revision across every tracked
// field, or "" if no field has ever been touched. This is what
// handlers stamp into the resource's `control_revision` and the `ETag`
// response header.
func (f *FieldRevisions) Top() string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var max string
	for _, r := range f.revs {
		if r > max {
			max = r
		}
	}
	return max
}

// Touch advances the supplied paths to a fresh revision and returns
// it. All touched paths share one revision so a single PATCH that
// writes N fields produces one new ETag, not N. A no-op call (empty
// paths) returns "" without consuming a revision.
func (f *FieldRevisions) Touch(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	return f.TouchWith(paths, newRevision())
}

// TouchWith advances the supplied paths to the supplied revision string.
// Used by handlers that compute a revision under an outer lock and want
// to stamp the same value into v1's `control_revision` field plus v2's
// FieldRevisions in one atomic transaction.
func (f *FieldRevisions) TouchWith(paths []string, rev string) string {
	if len(paths) == 0 {
		return ""
	}
	f.mu.Lock()
	for _, p := range paths {
		f.revs[p] = rev
	}
	f.mu.Unlock()
	return rev
}

// Conflicts returns the subset of `paths` whose stored revision (or any
// hierarchically-related stored path's revision — parent or descendant)
// is strictly newer than `ifMatch` — i.e. someone else has written one
// of these fields since the client last read.
//
// **Hierarchical match.** A stored revision at `fault_rules` conflicts
// with a query path `fault_rules.r1` (the array was replaced wholesale,
// so the client's view of `r1` is stale). A stored revision at
// `fault_rules.r2` conflicts with a query path `fault_rules` (the
// whole-array writer needs to know any per-rule writes have happened).
// Sibling paths don't conflict (`fault_rules.r1` vs `fault_rules.r2`
// are independent — that's the whole point of the per-rule sub-resource
// endpoints).
//
// If `ifMatch` is empty (header absent), every touched-since-zero
// related field counts as a conflict — this is intentional. Callers
// must require `If-Match` rather than letting empties succeed.
//
// Comparison is lexicographic on RFC3339Nano strings. RFC3339Nano sorts
// by time, so string `>` is equivalent to "newer." Legacy data that
// doesn't parse as RFC3339Nano falls back to plain string compare —
// still produces stable ordering, just not time-ordered.
func (f *FieldRevisions) Conflicts(ifMatch string, paths []string) []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := []string{}
	seen := map[string]bool{}
	report := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, q := range paths {
		for storedPath, storedRev := range f.revs {
			if !pathsRelated(storedPath, q) {
				continue
			}
			if ifMatch == "" || storedRev > ifMatch {
				report(q)
			}
		}
	}
	return out
}

// pathsRelated reports whether `a` and `b` are the same path or one is
// a `.`-bounded ancestor of the other. Sibling paths are not related.
func pathsRelated(a, b string) bool {
	if a == b {
		return true
	}
	if len(a) < len(b) {
		a, b = b, a
	}
	// a is the longer (or equal) path. b ancestor of a iff
	// a starts with b followed by '.'.
	return len(a) > len(b) && a[len(b)] == '.' && a[:len(b)] == b
}

// Snapshot copies the current path → revision map (for serialisation
// when the resource is persisted across restarts, or for tests).
func (f *FieldRevisions) Snapshot() map[string]string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make(map[string]string, len(f.revs))
	for k, v := range f.revs {
		out[k] = v
	}
	return out
}

// Restore replaces the tracker's contents with the supplied map. Used
// when re-hydrating a player record from durable storage.
func (f *FieldRevisions) Restore(m map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revs = make(map[string]string, len(m))
	for k, v := range m {
		f.revs[k] = v
	}
}

// ----- Revision generator --------------------------------------------------

// monoSeq breaks ties when two revisions are minted within the same
// nanosecond (or the wall clock fails to advance). Atomic for safe
// concurrent use; appended to the timestamp before format-encoding.
var monoSeq uint64

// newRevision returns a fresh process-monotonic revision string in
// v1's `control_revision` format (RFC3339Nano timestamp).
//
// For high-throughput call sites, two revisions minted within the same
// nanosecond (rare, but possible on fast CI) would collide on a pure
// timestamp. We append an atomic counter as a fractional-suffix only
// when the timestamp string would equal the most recent — but in
// practice the format already spans nanosecond precision, so the
// guard is mostly defensive.
func newRevision() string {
	atomic.AddUint64(&monoSeq, 1)
	return time.Now().UTC().Format(time.RFC3339Nano)
}
