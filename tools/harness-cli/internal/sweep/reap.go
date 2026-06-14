package sweep

import "time"

// A runner that dies mid-run orphans its file in running/ — without recovery
// the queue silently shrinks (§11). ReapStale finds the running experiments
// whose claim is older than maxAge so the caller can return them to backlog.
//
// `now` and each ClaimedAt are RFC3339 UTC. An experiment with no ClaimedAt
// (claimed by an older binary, or a hand-moved file) is treated as stale so it
// can't wedge the queue forever. A future-dated ClaimedAt (clock skew) is not
// stale. The threshold should be generous — the design suggests ~2× the
// longest expected run — so a slow-but-alive run is never yanked out from
// under itself.
func ReapStale(running []*Experiment, now string, maxAge time.Duration) []*Experiment {
	nowT, err := time.Parse(time.RFC3339, now)
	if err != nil {
		return nil // can't reason about time → reap nothing, fail safe
	}
	var stale []*Experiment
	for _, e := range running {
		if e.ClaimedAt == "" {
			stale = append(stale, e)
			continue
		}
		claimedT, err := time.Parse(time.RFC3339, e.ClaimedAt)
		if err != nil {
			stale = append(stale, e) // unparseable → treat as stale
			continue
		}
		if nowT.Sub(claimedT) > maxAge {
			stale = append(stale, e)
		}
	}
	return stale
}

// Requeue resets the runtime state a reaped experiment accumulated under its
// dead owner, so the next claimer starts clean. The recipe + provenance are
// untouched; only owner/claim/play/result are cleared.
func Requeue(e *Experiment) {
	e.Owner = ""
	e.ClaimedAt = ""
	e.PlayID = ""
	e.Result = nil
}
