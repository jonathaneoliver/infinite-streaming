package sweep

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
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

// mockForwarder is a minimal in-memory stand-in for the forwarder's CH-master
// sweep endpoints, so the CH-backed Store can be unit-tested without ClickHouse.
// (The concurrency-safe claim arbitration itself is a forwarder/CH concern,
// verified live; here we exercise the Store's request/response round-trips.)
func mockForwarder(t *testing.T) (*Store, func()) {
	t.Helper()
	rows := map[string]expRow{}
	mux := http.NewServeMux()
	mux.HandleFunc("/analytics/api/v2/sweep/experiments", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var post struct {
				Experiments []expRow `json:"experiments"`
			}
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &post)
			for _, row := range post.Experiments {
				rows[row.ExpID] = row // ReplacingMergeTree upsert by exp_id
			}
			writeJSONTest(w, map[string]any{"stored": len(post.Experiments)})
		case http.MethodGet:
			status := r.URL.Query().Get("status")
			var items []expRow
			for _, row := range rows {
				if row.Status == "deleted" {
					continue
				}
				if status == "" || row.Status == status {
					items = append(items, row)
				}
			}
			sort.Slice(items, func(i, j int) bool { return items[i].ExpID < items[j].ExpID })
			raw := make([]json.RawMessage, len(items))
			for i, it := range items {
				raw[i], _ = json.Marshal(it)
			}
			writeJSONTest(w, map[string]any{"items": raw})
		}
	})
	mux.HandleFunc("/analytics/api/v2/sweep/claim", func(w http.ResponseWriter, r *http.Request) {
		var post struct {
			Owner string `json:"owner"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &post)
		// pick the top-scored backlog row
		var pick *expRow
		for id := range rows {
			row := rows[id]
			if row.Status != "backlog" {
				continue
			}
			if pick == nil || row.Score > pick.Score || (row.Score == pick.Score && row.ExpID < pick.ExpID) {
				r := row
				pick = &r
			}
		}
		if pick == nil {
			writeJSONTest(w, map[string]any{"experiment": nil})
			return
		}
		pick.Status = "running"
		pick.Owner = post.Owner
		rows[pick.ExpID] = *pick
		writeJSONTest(w, map[string]any{"experiment": json.RawMessage(pick.RawJSON), "owner": post.Owner, "claimed_at": "2026-06-13T12:00:00Z"})
	})
	mux.HandleFunc("/analytics/api/v2/sweep/delete", func(w http.ResponseWriter, r *http.Request) {
		var post struct {
			ExpID string `json:"exp_id"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &post)
		if row, ok := rows[post.ExpID]; ok {
			row.Status = "deleted"
			rows[post.ExpID] = row
		}
		writeJSONTest(w, map[string]any{"deleted": post.ExpID})
	})
	srv := httptest.NewServer(mux)
	return OpenCH(srv.URL, srv.Client(), ""), srv.Close
}

func writeJSONTest(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestSaveListRoundTrip(t *testing.T) {
	s, done := mockForwarder(t)
	defer done()
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
		t.Fatalf("shape (from raw_json) not preserved: %+v", got.Shape)
	}
}

func TestLoadMissing(t *testing.T) {
	s, done := mockForwarder(t)
	defer done()
	if _, err := s.Load(StatusBacklog, "nope"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestListSorted(t *testing.T) {
	s, done := mockForwarder(t)
	defer done()
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
	s, done := mockForwarder(t)
	defer done()
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

func TestClaimNextStampsOwner(t *testing.T) {
	s, done := mockForwarder(t)
	defer done()
	s.Save(StatusBacklog, sampleExp("x"))
	e, err := s.ClaimNext("runner-1")
	if err != nil {
		t.Fatal(err)
	}
	if e == nil || e.ID != "x" {
		t.Fatalf("claim returned %+v", e)
	}
	if e.Owner != "runner-1" {
		t.Fatalf("owner not stamped: %q", e.Owner)
	}
	// now the experiment is running, so a second claim finds nothing
	again, err := s.ClaimNext("runner-2")
	if err != nil {
		t.Fatal(err)
	}
	if again != nil {
		t.Fatalf("backlog should be empty, got %+v", again)
	}
}

func TestMoveToDone(t *testing.T) {
	s, done := mockForwarder(t)
	defer done()
	s.Save(StatusBacklog, sampleExp("y"))
	e, _ := s.ClaimNext("r")
	e.Result = &Result{Verdict: VerdictClean}
	if err := s.Move(StatusRunning, StatusDone, e); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(StatusRunning, "y"); err != ErrNotFound {
		t.Fatalf("should no longer be running, got %v", err)
	}
	doneExp, err := s.Load(StatusDone, "y")
	if err != nil || doneExp.Result == nil || doneExp.Result.Verdict != VerdictClean {
		t.Fatalf("done experiment wrong: %+v err=%v", doneExp, err)
	}
}

func TestDeleteTombstones(t *testing.T) {
	s, done := mockForwarder(t)
	defer done()
	s.Save(StatusBacklog, sampleExp("z"))
	if err := s.Delete("z"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(StatusBacklog, "z"); err != ErrNotFound {
		t.Fatalf("deleted experiment should be gone, got %v", err)
	}
}

func ids(es []*Experiment) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.ID
	}
	return out
}

var _ = strings.TrimSpace
