package sweep

import (
	"testing"
	"time"
)

func TestNextStepBranches(t *testing.T) {
	now := "2026-06-14T12:00:00Z"
	maxAge := time.Hour

	cases := []struct {
		name   string
		e      *Experiment
		all    []*Experiment
		action string
	}{
		{"backlog → claim", &Experiment{ID: "a", Status: StatusBacklog}, nil, "claim & run"},
		{"running no play, fresh → wait", &Experiment{ID: "b", Status: StatusRunning, Owner: "r", ClaimedAt: "2026-06-14T11:59:00Z"}, nil, "wait"},
		{"running no play, stale → reap", &Experiment{ID: "c", Status: StatusRunning, Owner: "r", ClaimedAt: "2026-06-14T09:00:00Z"}, nil, "reap & re-claim"},
		{"running with play, no result → analyze", &Experiment{ID: "d", Status: StatusRunning, PlayID: "p123"}, nil, "analyze"},
		{"found, no children → isolate", &Experiment{ID: "e", Status: StatusFound, Result: &Result{Verdict: VerdictAberration}}, nil, "isolate → promote"},
		{"found, has children → promote", &Experiment{ID: "f", Status: StatusFound}, []*Experiment{{ID: "f-iso", Parent: "f"}}, "promote"},
		{"found, promoted → done", &Experiment{ID: "g", Status: StatusFound, IssueURL: "https://gh/123"}, nil, "done"},
		{"done → done", &Experiment{ID: "h", Status: StatusDone}, nil, "done"},
	}
	for _, tc := range cases {
		all := tc.all
		if all == nil {
			all = []*Experiment{tc.e}
		}
		got := NextStep(tc.e, all, now, maxAge)
		if got.Action != tc.action {
			t.Errorf("%s: got action %q, want %q (reason %q)", tc.name, got.Action, tc.action, got.Reason)
		}
	}
}

func TestAgendaSkipsTerminal(t *testing.T) {
	now := "2026-06-14T12:00:00Z"
	all := []*Experiment{
		{ID: "back", Status: StatusBacklog},
		{ID: "done", Status: StatusDone},
		{ID: "promoted", Status: StatusFound, IssueURL: "https://gh/1"},
		{ID: "open-hit", Status: StatusFound},
	}
	steps := Agenda(all, now, time.Hour)
	if len(steps) != 2 {
		t.Fatalf("want 2 actionable (back + open-hit), got %d: %+v", len(steps), steps)
	}
	// found sorts before backlog
	if steps[0].ID != "open-hit" || steps[1].ID != "back" {
		t.Fatalf("order wrong: %s, %s", steps[0].ID, steps[1].ID)
	}
}
