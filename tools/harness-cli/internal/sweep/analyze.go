package sweep

import (
	"encoding/json"
	"fmt"
)

// This file is the device-INDEPENDENT half of the §7 run loop: once a probe
// has produced a play_id, everything here is pure data → verdict → next-state,
// so it unit-tests against fixtures without a live deploy or a sim.

// LabelsFromPlayHistogram extracts the distinct severity-tagged labels for one
// play from a forwarder /api/v2/plays response envelope. The authoritative
// source is each play row's `label_histogram` — `[[label, count], …]` pairs
// unioned across session_events + network_requests + control_events (the same
// vocab the oracle classifies). This is what the dashboard's chip cloud reads;
// the per-row events `labels` field is the operator k/v MAP, not these. Order
// is the histogram's (occurrence-descending), deduped. A missing/empty
// histogram returns nil so a clean play reads `clean`, not an error.
func LabelsFromPlayHistogram(body []byte) ([]string, error) {
	var env struct {
		Items []struct {
			LabelHistogram [][]any `json:"label_histogram"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("sweep: parse plays envelope: %w", err)
	}
	seen := make(map[string]bool)
	var out []string
	for _, it := range env.Items {
		for _, pair := range it.LabelHistogram {
			if len(pair) == 0 {
				continue
			}
			label, _ := pair[0].(string)
			if label == "" || seen[label] {
				continue
			}
			seen[label] = true
			out = append(out, label)
		}
	}
	return out, nil
}

// Analyze records the oracle verdict (Oracle A, §3) onto an experiment and
// reports the bucket it should move to. It does NOT touch the store — the
// caller owns the Move so a crash can't lose the file. `playID` is stamped so
// the dashboard/`found` post-mortem can jump straight to the archived play.
//
//	clean       → done
//	notable     → found   (a lower-priority finding, still promoted)
//	aberration  → found
//
// inconclusive is never produced here — it's an infra/probe verdict the runner
// sets when there's no play_id at all (§11), distinct from "the player coped".
func Analyze(e *Experiment, playID string, labels []string) Status {
	v := Classify(labels)
	e.PlayID = playID
	e.Result = &Result{
		Verdict: v,
		Labels:  labels,
		Note:    analysisNote(v, labels),
	}
	switch v {
	case VerdictNotable, VerdictAberration:
		return StatusFound
	default:
		return StatusDone
	}
}

func analysisNote(v Verdict, labels []string) string {
	switch v {
	case VerdictNotable, VerdictAberration:
		if k := PrimaryKind(labels); k != "" {
			return fmt.Sprintf("%s: %s", v, k)
		}
	}
	return string(v)
}

// NeedsConfirmation reports whether a first-pass hit should be re-run before
// it's trusted — the n=1 guard (§5, A.3). A single-rep seed/isolation that came
// back notable/aberration needs ≥reps confirmation runs; an experiment that is
// itself already part of a rep batch (RepGroup set) must NOT spawn another
// batch, or the queue never converges.
func NeedsConfirmation(e *Experiment) bool {
	if e.Result == nil {
		return false
	}
	if e.RepGroup != "" { // already a confirmation rep — don't recurse
		return false
	}
	switch e.Result.Verdict {
	case VerdictNotable, VerdictAberration:
		return e.Reps <= 1
	default:
		return false
	}
}

// ConfirmationReps materialises `n` independent re-runs of a hit's recipe,
// each a separate experiment sharing one rep_group, so the loop can promote
// only when the verdict is stable across them (§5: "repetition is the sweep's
// job, not the test's"). The reps carry RepGroup (so they won't themselves
// spawn more) and reset runtime/outcome state. `now` is RFC3339 UTC.
func ConfirmationReps(parent *Experiment, n int, now string) []*Experiment {
	if n < 1 {
		n = 1
	}
	group := "rep-" + shortHash(parent.ID)
	out := make([]*Experiment, 0, n)
	for i := 0; i < n; i++ {
		c := cloneExperimentRecipe(parent)
		c.ID = fmt.Sprintf("%s-%d", group, i+1)
		c.Kind = parent.Kind
		c.Arm = parent.Arm
		c.Group = parent.Group
		c.Parent = parent.ID
		c.Depth = parent.Depth
		c.RepGroup = group
		c.Reps = 1
		c.CreatedAt = now
		c.ClaimedAt = ""
		c.Why = "confirm_" + string(parent.Result.Verdict)
		c.WhyText = fmt.Sprintf("confirmation rep %d/%d for %s (verdict %s); n=1 guard before promotion",
			i+1, n, parent.ID, parent.Result.Verdict)
		c.Score = 0
		out = append(out, c)
	}
	return out
}
