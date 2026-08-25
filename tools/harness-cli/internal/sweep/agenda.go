package sweep

import (
	"fmt"
	"time"
)

// The agenda (#772 CH-master): the next action for an experiment, derived purely
// from ClickHouse state — status + play/result + claim age + lineage + issue —
// so a runner (or a fresh session with only the database) knows what to do and
// can resume the loop without any local memory. Side effects are recorded back
// to CH (verdict, issue_url) so the next action stays a pure function and the
// loop never redoes work.

// Step is one actionable instruction.
type Step struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Action string `json:"action"` // short imperative
	Reason string `json:"reason"` // why / what it's waiting on
}

// NextStep returns the next action for e given the whole queue `all` (for lineage
// checks) and the reap window (now / maxAge for the stale-claim test).
func NextStep(e *Experiment, all []*Experiment, now string, maxAge time.Duration) Step {
	s := Step{ID: e.ID, Status: string(e.Status)}
	switch e.Status {
	case StatusBacklog:
		s.Action, s.Reason = "claim & run", "pending — claimed by score order"
	case StatusRunning:
		switch {
		case e.PlayID == "" && claimStale(e.ClaimedAt, now, maxAge):
			s.Action, s.Reason = "reap & re-claim", "claim is stale (owner "+orDash(e.Owner)+")"
		case e.PlayID == "":
			s.Action, s.Reason = "wait", "probe in flight (owner "+orDash(e.Owner)+")"
		case e.Result == nil:
			s.Action, s.Reason = "analyze", "play "+short(e.PlayID)+" produced — needs a verdict"
		default:
			s.Action, s.Reason = "move", "analyzed ("+string(e.Result.Verdict)+") — awaiting bucket move"
		}
	case StatusFound:
		switch {
		case e.IssueURL != "":
			s.Action, s.Reason = "done", "promoted → "+e.IssueURL
		case hasChildren(e, all):
			s.Action, s.Reason = "promote", "isolation fan / reps enqueued — promote once attributed"
		default:
			s.Action, s.Reason = "isolate → promote", "confirmed hit — enqueue the OFAT fan, then promote"
		}
	case StatusReview, StatusFeedback:
		s.Action, s.Reason = "needs human", "manual review / severity rating"
	case StatusDone:
		s.Action, s.Reason = "done", "clean"
	default:
		s.Action = "—"
	}
	return s
}

// Agenda returns the next step for every actionable experiment (skips terminal
// done/promoted), ordered backlog → running → found.
func Agenda(all []*Experiment, now string, maxAge time.Duration) []Step {
	order := map[Status]int{StatusRunning: 0, StatusFound: 1, StatusReview: 2, StatusFeedback: 3, StatusBacklog: 4}
	var steps []Step
	for _, e := range all {
		if e.Status == StatusDone {
			continue
		}
		if e.Status == StatusFound && e.IssueURL != "" {
			continue // terminal: already promoted
		}
		steps = append(steps, NextStep(e, all, now, maxAge))
	}
	stableSortSteps(steps, func(a, b Step) bool {
		oa, ob := order[Status(a.Status)], order[Status(b.Status)]
		if oa != ob {
			return oa < ob
		}
		return a.ID < b.ID
	})
	return steps
}

func hasChildren(e *Experiment, all []*Experiment) bool {
	for _, c := range all {
		if c.Parent == e.ID {
			return true
		}
	}
	return false
}

func claimStale(claimedAt, now string, maxAge time.Duration) bool {
	if claimedAt == "" {
		return true
	}
	c, err1 := time.Parse(time.RFC3339, claimedAt)
	n, err2 := time.Parse(time.RFC3339, now)
	if err1 != nil || err2 != nil {
		return true
	}
	return n.Sub(c) > maxAge
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// stableSortSteps is a tiny insertion sort (the queue is small) to avoid a
// sort import churn; keeps the order deterministic.
func stableSortSteps(s []Step, less func(a, b Step) bool) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && less(s[j], s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

var _ = fmt.Sprintf
