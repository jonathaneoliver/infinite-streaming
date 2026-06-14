package sweep

import "testing"

func TestKindRankingDominates(t *testing.T) {
	w := DefaultWeights()
	seed := &Experiment{Kind: KindSeed, Mode: "steps", Platform: "ipad-sim"}      // cheap seed
	iso := &Experiment{Kind: KindIsolation, Mode: "pyramid", Platform: "appletv"} // expensive isolation
	if w.Score(iso) <= w.Score(seed) {
		t.Fatalf("isolation must outrank seed regardless of cost: iso=%.1f seed=%.1f",
			w.Score(iso), w.Score(seed))
	}
}

func TestDeeperBisectRanksHigher(t *testing.T) {
	w := DefaultWeights()
	shallow := &Experiment{Kind: KindBisect, Depth: 1, Mode: "steps", Platform: "ipad-sim"}
	deep := &Experiment{Kind: KindBisect, Depth: 3, Mode: "steps", Platform: "ipad-sim"}
	if w.Score(deep) <= w.Score(shallow) {
		t.Fatalf("deeper bisect should rank higher: deep=%.1f shallow=%.1f", w.Score(deep), w.Score(shallow))
	}
}

func TestCostBreaksTiesCheapFirst(t *testing.T) {
	w := DefaultWeights()
	cheap := &Experiment{Kind: KindSeed, Mode: "transient_shock", Platform: "ipad-sim"} // ~2 min
	dear := &Experiment{Kind: KindSeed, Mode: "pyramid", Platform: "appletv"}           // ~9 min
	if w.Score(cheap) <= w.Score(dear) {
		t.Fatalf("cheaper run should win the tie: cheap=%.1f dear=%.1f", w.Score(cheap), w.Score(dear))
	}
}

func TestSelectNextDepthFirstPrefersNonSeed(t *testing.T) {
	w := DefaultWeights()
	backlog := []*Experiment{
		{ID: "s1", Kind: KindSeed, Mode: "transient_shock", Platform: "ipad-sim"},
		{ID: "b1", Kind: KindBisect, Depth: 1, Mode: "pyramid", Platform: "appletv"}, // slower + bisect
	}
	got := w.SelectNext(backlog, true)
	if got == nil || got.ID != "b1" {
		t.Fatalf("depth-first should pick the bisect, got %+v", got)
	}
	// Without depth-first the bisect still wins here (kind dominates), but the
	// guard guarantees it even if a seed ever scored higher.
}

func TestSelectNextEmpty(t *testing.T) {
	if got := DefaultWeights().SelectNext(nil, true); got != nil {
		t.Fatalf("empty backlog must select nil, got %+v", got)
	}
}

func TestSeedNarrowConfigClass(t *testing.T) {
	es := Seed(ClassConfig, false, "2026-06-13T00:00:00Z")
	// 1 platform × len(seedProtocols) × len(configRecipes)
	want := len(seedProtocols) * len(configRecipes)
	if len(es) != want {
		t.Fatalf("narrow config seed want %d, got %d", want, len(es))
	}
	seen := map[string]bool{}
	for _, e := range es {
		if e.Platform != "ipad-sim" {
			t.Fatalf("narrow seed must be ipad-sim only, got %s", e.Platform)
		}
		if e.Class != ClassConfig {
			t.Fatalf("config seed must be config-class, got %s", e.Class)
		}
		if e.Fault != nil {
			t.Fatalf("config seed must never carry a fault: %+v", e.Fault)
		}
		if e.Kind != KindSeed || e.Content != SeedContent {
			t.Fatalf("bad seed: %+v", e)
		}
		if seen[e.ID] {
			t.Fatalf("duplicate seed id %s", e.ID)
		}
		seen[e.ID] = true
	}
}

func TestSeedFaultClass(t *testing.T) {
	es := Seed(ClassFault, false, "2026-06-13T00:00:00Z")
	want := len(seedProtocols) * len(faultRecipes)
	if len(es) != want {
		t.Fatalf("narrow fault seed want %d, got %d", want, len(es))
	}
	for _, e := range es {
		if e.Class != ClassFault || e.Fault == nil {
			t.Fatalf("fault seed must be fault-class with a fault: %+v", e)
		}
	}
}

func TestSeedFullWidens(t *testing.T) {
	es := Seed(ClassConfig, true, "2026-06-13T00:00:00Z")
	want := len(fullPlatforms) * len(seedProtocols) * len(configRecipes)
	if len(es) != want {
		t.Fatalf("full seed want %d, got %d", want, len(es))
	}
}

func TestSeedDeterministic(t *testing.T) {
	a := Seed(ClassConfig, false, "t")
	b := Seed(ClassConfig, false, "t")
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Score != b[i].Score {
			t.Fatalf("seed not deterministic at %d: %s/%s", i, a[i].ID, b[i].ID)
		}
	}
}
