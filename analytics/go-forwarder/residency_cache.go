// #550 Phase 1 — per-play state cache for delta computation.
//
// Wire contract is accumulated-only on the iOS side (each row carries
// monotonically-increasing *_time_ms / *_count values since play
// start). At insert time the forwarder subtracts the previous
// accumulated values for this play_id from the current snapshot's
// accumulated values, producing the *_delta companion columns.
//
// Eviction: handlePayload() collects active play_ids from each
// payload and calls Prune() — same lifecycle as sessionToPlayerID
// (net_log.go). A play that drops out of the SSE stream for a
// payload tick is evicted; the next snapshot under the same play_id
// would re-init from 0 (correct for the "since play start" semantics
// of the lag(_, _, 0) precedent).
//
// Idempotency: handlePayload's fingerprint dedupe (line ~597) gates
// the delta computation. Same snapshot in twice = same cache entry
// updated to the same value = delta would be 0 the second time — but
// the dedupe prevents the second observation from reaching toRow at
// all, so the cache never observes an unchanged accumulated value.

package main

import "sync"

// residencyState mirrors the 14 accumulated *_time_ms + *_count
// counters + the single error_count counter on `row`. Used as the
// value type in the per-play cache.
type residencyState struct {
	PlayingTimeMs       uint32
	PlayingCount        uint32
	PausingTimeMs       uint32
	PausingCount        uint32
	BufferingTimeMs     uint32
	BufferingCount      uint32
	StallingTimeMs      uint32
	StallingCount       uint32
	IdlingTimeMs        uint32
	IdlingCount         uint32
	SeekingTimeMs       uint32
	SeekingCount        uint32
	TrickplayingTimeMs  uint32
	TrickplayingCount   uint32
	ErrorCount          uint32
}

type residencyCache struct {
	mu sync.RWMutex
	m  map[string]residencyState
}

func newResidencyCache() *residencyCache {
	return &residencyCache{m: make(map[string]residencyState)}
}

// observe atomically reads the prior accumulated values for playID,
// stores the new values, and returns the prior — the caller subtracts
// to get the deltas. On first observation for a play returns a zero
// state, so first row's delta equals the row's accumulated value
// (matches `lag(x, 1, 0) OVER ...` semantics).
func (c *residencyCache) observe(playID string, current residencyState) residencyState {
	if playID == "" {
		return residencyState{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	prior := c.m[playID]
	c.m[playID] = current
	return prior
}

// prune removes any cache entries whose play_id isn't in `active`.
// Same shape as sessionPlayerMap.prune.
func (c *residencyCache) prune(active map[string]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.m {
		if _, ok := active[k]; !ok {
			delete(c.m, k)
		}
	}
}

// playResidency is the package-level cache instance used by
// handlePayload + toRow.
var playResidency = newResidencyCache()

// subDeltaU32 saturating-subtracts uint32 values. Accumulated
// counters should never decrease — but if a payload arrives with a
// value lower than the prior (clock skew, play_id reused after a
// reset that the cache didn't observe), clamp at 0 rather than
// underflowing into a huge UInt32.
func subDeltaU32(current, prior uint32) uint32 {
	if current < prior {
		return 0
	}
	return current - prior
}

// applyResidencyDeltas computes the *_delta columns on `r` by
// observing the per-play cache. Called from toRow after the
// accumulated fields are filled in.
func applyResidencyDeltas(r *row, playID string) {
	current := residencyState{
		PlayingTimeMs:      r.PlayingTimeMs,
		PlayingCount:       r.PlayingCount,
		PausingTimeMs:      r.PausingTimeMs,
		PausingCount:       r.PausingCount,
		BufferingTimeMs:    r.BufferingTimeMs,
		BufferingCount:     r.BufferingCount,
		StallingTimeMs:     r.StallingTimeMs,
		StallingCount:      r.StallingCount,
		IdlingTimeMs:       r.IdlingTimeMs,
		IdlingCount:        r.IdlingCount,
		SeekingTimeMs:      r.SeekingTimeMs,
		SeekingCount:       r.SeekingCount,
		TrickplayingTimeMs: r.TrickplayingTimeMs,
		TrickplayingCount:  r.TrickplayingCount,
		ErrorCount:         r.ErrorCount,
	}
	prior := playResidency.observe(playID, current)
	r.PlayingTimeMsDelta      = subDeltaU32(current.PlayingTimeMs,      prior.PlayingTimeMs)
	r.PlayingCountDelta       = subDeltaU32(current.PlayingCount,       prior.PlayingCount)
	r.PausingTimeMsDelta      = subDeltaU32(current.PausingTimeMs,      prior.PausingTimeMs)
	r.PausingCountDelta       = subDeltaU32(current.PausingCount,       prior.PausingCount)
	r.BufferingTimeMsDelta    = subDeltaU32(current.BufferingTimeMs,    prior.BufferingTimeMs)
	r.BufferingCountDelta     = subDeltaU32(current.BufferingCount,     prior.BufferingCount)
	r.StallingTimeMsDelta     = subDeltaU32(current.StallingTimeMs,     prior.StallingTimeMs)
	r.StallingCountDelta      = subDeltaU32(current.StallingCount,      prior.StallingCount)
	r.IdlingTimeMsDelta       = subDeltaU32(current.IdlingTimeMs,       prior.IdlingTimeMs)
	r.IdlingCountDelta        = subDeltaU32(current.IdlingCount,        prior.IdlingCount)
	r.SeekingTimeMsDelta      = subDeltaU32(current.SeekingTimeMs,      prior.SeekingTimeMs)
	r.SeekingCountDelta       = subDeltaU32(current.SeekingCount,       prior.SeekingCount)
	r.TrickplayingTimeMsDelta = subDeltaU32(current.TrickplayingTimeMs, prior.TrickplayingTimeMs)
	r.TrickplayingCountDelta  = subDeltaU32(current.TrickplayingCount,  prior.TrickplayingCount)
	r.ErrorCountDelta         = subDeltaU32(current.ErrorCount,         prior.ErrorCount)
}
