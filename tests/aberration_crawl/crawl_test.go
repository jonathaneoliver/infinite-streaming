package aberration_crawl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"
)

// TestAberrationCensus runs the full invariants catalogue against the
// archive. Census mode (default) reports and never fails on violations;
// ABERRATION_MODE=assert fails on error-severity rules whose mode is
// 'assert' in the catalogue (none yet — promotion happens in Phase 3).
//
// Requires ClickHouse at ABERRATION_CH_URL (default
// http://127.0.0.1:21123; tunnel: ssh -L 21123:127.0.0.1:21123 $TEST_SSH).
// Skips when CH is unreachable.
func TestAberrationCensus(t *testing.T) {
	ch := NewCHClientFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	if _, err := ch.Query(ctx, "SELECT 1"); err != nil {
		t.Skipf("ClickHouse unreachable at %s (%v) — tunnel with: ssh -L 21123:127.0.0.1:21123 $TEST_SSH", ch.URL, err)
	}

	cat, err := LoadCatalogue("invariants.yaml")
	if err != nil {
		t.Fatalf("load catalogue: %v", err)
	}
	t.Logf("catalogue: %d rules", len(cat.Rules))

	results := RunAll(ctx, ch, cat)

	assertMode := os.Getenv("ABERRATION_MODE") == "assert"
	var lines []string
	for _, rr := range results {
		switch {
		case rr.Err != "":
			lines = append(lines, fmt.Sprintf("ERROR  %-32s %-18s %s", rr.RuleID, rr.Table, rr.Err))
			t.Errorf("rule %s/%s: query error: %s", rr.RuleID, rr.Table, rr.Err)
			continue
		case rr.Skipped != "":
			lines = append(lines, fmt.Sprintf("SKIP   %-32s %-18s %s", rr.RuleID, rr.Table, rr.Skipped))
			continue
		}
		total := rr.TotalViolations()
		var checked int64
		for _, g := range rr.Groups {
			checked += g.Checked
		}
		status := "ok"
		if total > 0 {
			status = fmt.Sprintf("%d viol", total)
		}
		pend := ""
		if rr.Pending {
			pend = " [pending]"
		}
		lines = append(lines, fmt.Sprintf("%-6s %-32s %-18s checked=%-9d since=%s%s",
			status, rr.RuleID, rr.Table, checked, rr.Since, pend))

		// Per-group detail only where violations exist.
		for _, g := range rr.Groups {
			if g.Violations > 0 {
				lines = append(lines, fmt.Sprintf("         %-40s %d/%d (excl %d)",
					g.Group, g.Violations, g.Checked, g.Excluded))
			}
		}
		for _, v := range rr.UnknownValues {
			lines = append(lines, fmt.Sprintf("         unknown %-32s ×%d", v.Value, v.Count))
		}
		for _, e := range rr.Exemplars {
			lines = append(lines, fmt.Sprintf("         exemplar player=%s play=%s ts=%s", e.PlayerID, e.PlayID, e.TS))
		}

		if assertMode && rr.Mode == "assert" && !rr.Pending && rr.Severity == "error" && total > 0 {
			t.Errorf("ASSERT rule %s/%s: %d violations", rr.RuleID, rr.Table, total)
		}
	}

	sort.SliceStable(lines, func(i, j int) bool { return false }) // keep catalogue order
	for _, l := range lines {
		t.Log(l)
	}

	if path := os.Getenv("ABERRATION_REPORT"); path != "" {
		blob, err := json.MarshalIndent(results, "", "  ")
		if err == nil {
			err = os.WriteFile(path, blob, 0o644)
		}
		if err != nil {
			t.Errorf("write report %s: %v", path, err)
		} else {
			t.Logf("report written: %s", path)
		}
	}
}

// TestCatalogueValid is the cheap CI-side check: the YAML parses and
// every rule passes structural validation, no CH needed.
func TestCatalogueValid(t *testing.T) {
	cat, err := LoadCatalogue("invariants.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Rules) < 30 {
		t.Errorf("catalogue has %d rules; expected >= 30 (issue #607 acceptance)", len(cat.Rules))
	}
}
