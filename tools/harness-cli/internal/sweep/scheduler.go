package sweep

import "sort"

// Weights drive the discovery-first score (§5 of the design). Defaults make
// the system depth-first by construction: a confirmed hit's isolation/bisect
// follow-ups always outrank any untouched seed cell, and cost is only a
// tiebreaker. Human severity feedback (§8) later overrides these.
type Weights struct {
	Kind  float64 // weight on kind rank (isolation > bisect > hypothesis > seed)
	Depth float64 // weight on bisection depth (deeper narrowing rises)
	Cost  float64 // penalty per estimated runtime-minute (tiebreaker only)
}

// DefaultWeights: Kind dominates (100) so non-seed work always jumps the
// queue; Depth (10) lifts deeper bisects; Cost (1) only breaks ties among
// equals — a 6-minute run loses to a 3-minute one, never to a seed.
func DefaultWeights() Weights { return Weights{Kind: 100, Depth: 10, Cost: 1} }

// kindRank orders the four kinds for the scheduler. Higher = run sooner.
func kindRank(k Kind) float64 {
	switch k {
	case KindIsolation:
		return 4
	case KindBisect:
		return 3
	case KindHypothesis:
		return 2
	case KindManual:
		return 1.5 // an operator-authored ad-hoc probe jumps the seed backlog but yields to any hit-derived follow-up
	case KindSeed:
		return 1
	default:
		return 0
	}
}

// modeMinutes is the per-mode wall-clock estimate (§A.3) — minutes, the
// dominant cost term. Unknown modes fall back to 3.
var modeMinutes = map[string]float64{
	"pyramid":             6,
	"hysteresis_gap":      6,
	"startup_caps":        5,
	"steps":               3,
	"rampup":              3,
	"rampdown":            3,
	"downshift_severity":  3,
	"transient_shock":     2,
	"emergency_downshift": 3,
	"startup":             2,
	"abort":               3,
}

// platformCostMul scales runtime by platform (appium Apple TV is the slow end).
var platformCostMul = map[string]float64{
	"appletv":   1.5,
	"androidtv": 1.2,
	"iphone":    1.1,
	"ipad-sim":  1.0,
	"web":       1.0,
}

// EstRuntimeMinutes is the scheduler's cost estimate for one experiment.
func EstRuntimeMinutes(e *Experiment) float64 {
	m, ok := modeMinutes[e.Mode]
	if !ok {
		m = 3
	}
	if mul, ok := platformCostMul[e.Platform]; ok {
		m *= mul
	}
	return m
}

// Score computes the scheduler sort key for one experiment (§5). Higher runs
// first. Adjacency + redundancy terms (which need cross-experiment / findings
// context) are layered on by the orchestrator; this is the per-experiment core.
func (w Weights) Score(e *Experiment) float64 {
	return w.Kind*kindRank(e.Kind) + w.Depth*float64(e.Depth) - w.Cost*EstRuntimeMinutes(e)
}

// Rank sorts a backlog by descending Score (id breaks ties for determinism),
// recomputing and stamping each experiment's Score in place.
func (w Weights) Rank(backlog []*Experiment) {
	for _, e := range backlog {
		e.Score = w.Score(e)
	}
	sort.SliceStable(backlog, func(i, j int) bool {
		if backlog[i].Score != backlog[j].Score {
			return backlog[i].Score > backlog[j].Score
		}
		return backlog[i].ID < backlog[j].ID
	})
}

// SelectNext returns the highest-priority backlog experiment to run next, or
// nil if the backlog is empty. depthFirst is the bootstrap guard (§5): while
// on, if any non-seed (isolation/bisect/hypothesis) work exists it is always
// chosen over a seed, so the loop drives the LLM investigate→insert→re-run
// chain deep before widening — even if a seed somehow scored higher.
func (w Weights) SelectNext(backlog []*Experiment, depthFirst bool) *Experiment {
	if len(backlog) == 0 {
		return nil
	}
	ranked := make([]*Experiment, len(backlog))
	copy(ranked, backlog)
	w.Rank(ranked)
	if depthFirst {
		for _, e := range ranked {
			if e.Kind != KindSeed {
				return e
			}
		}
	}
	return ranked[0]
}
