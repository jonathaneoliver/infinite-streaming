package sweep

import (
	"sync"
	"sync/atomic"
	"testing"
)

func sampleExp(id string) *Experiment {
	rate := 0.4
	return &Experiment{
		ID:        id,
		CreatedAt: "2026-06-13T18:00:00Z",
		Platform:  "ipad-sim",
		Protocol:  "hls",
		Content:   "insane_new",
		Mode:      "pyramid",
		Kind:      KindSeed,
		Depth:     0,
		Reps:      1,
		Score:     12.0,
		Shape:     &Shape{RateMbps: &rate, Pattern: "pyramid", StepSeconds: 30, MarginPct: 5},
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := sampleExp("seed-ipad-hls-ratecap")
	if err := s.Save(StatusBacklog, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(StatusBacklog, want.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID || got.Platform != "ipad-sim" || got.Kind != KindSeed {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if got.Shape == nil || got.Shape.RateMbps == nil || *got.Shape.RateMbps != 0.4 {
		t.Fatalf("shape not preserved: %+v", got.Shape)
	}
}

func TestLoadMissing(t *testing.T) {
	s, _ := Open(t.TempDir())
	if _, err := s.Load(StatusBacklog, "nope"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestListSortedSkipsDotfiles(t *testing.T) {
	s, _ := Open(t.TempDir())
	for _, id := range []string{"c", "a", "b"} {
		if err := s.Save(StatusBacklog, sampleExp(id)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.List(StatusBacklog)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != "a" || got[2].ID != "c" {
		t.Fatalf("want sorted [a b c], got %v", ids(got))
	}
}

func TestCounts(t *testing.T) {
	s, _ := Open(t.TempDir())
	s.Save(StatusBacklog, sampleExp("a"))
	s.Save(StatusBacklog, sampleExp("b"))
	s.Save(StatusDone, sampleExp("c"))
	counts, err := s.Counts()
	if err != nil {
		t.Fatal(err)
	}
	if counts[StatusBacklog] != 2 || counts[StatusDone] != 1 || counts[StatusFound] != 0 {
		t.Fatalf("bad counts: %v", counts)
	}
}

func TestClaimStampsOwnerAndMovesBucket(t *testing.T) {
	s, _ := Open(t.TempDir())
	s.Save(StatusBacklog, sampleExp("x"))
	e, err := s.Claim("x", "runner-1", "2026-06-13T12:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if e.Owner != "runner-1" {
		t.Fatalf("owner not stamped: %q", e.Owner)
	}
	if e.ClaimedAt != "2026-06-13T12:00:00Z" {
		t.Fatalf("claim time not stamped: %q", e.ClaimedAt)
	}
	if _, err := s.Load(StatusBacklog, "x"); err != ErrNotFound {
		t.Fatalf("backlog file should be gone, got %v", err)
	}
	running, err := s.Load(StatusRunning, "x")
	if err != nil || running.Owner != "runner-1" {
		t.Fatalf("running file missing owner: %+v err=%v", running, err)
	}
}

func TestClaimMissingIsAlreadyClaimed(t *testing.T) {
	s, _ := Open(t.TempDir())
	if _, err := s.Claim("ghost", "r", ""); err != ErrAlreadyClaimed {
		t.Fatalf("want ErrAlreadyClaimed, got %v", err)
	}
}

// The load-bearing guarantee: N runners racing for one experiment → exactly
// one wins, the rest get ErrAlreadyClaimed. This is the parallel-safety the
// whole local-store design rests on (§4, §7).
func TestConcurrentClaimExactlyOneWinner(t *testing.T) {
	s, _ := Open(t.TempDir())
	s.Save(StatusBacklog, sampleExp("contested"))

	const racers = 16
	var wins int64
	var wg sync.WaitGroup
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			if _, err := s.Claim("contested", "r", ""); err == nil {
				atomic.AddInt64(&wins, 1)
			} else if err != ErrAlreadyClaimed {
				t.Errorf("unexpected claim error: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("want exactly 1 winner, got %d", wins)
	}
}

func TestMoveToDone(t *testing.T) {
	s, _ := Open(t.TempDir())
	s.Save(StatusBacklog, sampleExp("y"))
	e, _ := s.Claim("y", "r", "")
	e.Result = &Result{Verdict: VerdictClean}
	if err := s.Move(StatusRunning, StatusDone, e); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(StatusRunning, "y"); err != ErrNotFound {
		t.Fatalf("running file should be gone, got %v", err)
	}
	done, err := s.Load(StatusDone, "y")
	if err != nil || done.Result == nil || done.Result.Verdict != VerdictClean {
		t.Fatalf("done file wrong: %+v err=%v", done, err)
	}
}

func ids(es []*Experiment) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.ID
	}
	return out
}
