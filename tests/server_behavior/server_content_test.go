// server_content_test.go — content-manipulation coverage (epic #518 P2).
//
// "Just fetch with and without things": for each manipulation control, fetch
// the affected resource with the control OFF (baseline) and ON, and assert
// the served bytes differ in the expected way.
//
//   - Master playlist manipulations (applied by the proxy on the master):
//     content_strip_codecs        → CODECS attr removed
//     content_strip_average_bandwidth → AVERAGE-BANDWIDTH removed
//     content_overstate_bandwidth → BANDWIDTH values inflated
//   - Segment corruption: segment_failure_type="corrupted" → body zero-filled.
//
// Run:
//
//	cd tests/server_behavior && go test -v -run TestServerContent -timeout 5m
package server_behavior

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func allZero(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

func TestServerContent(t *testing.T) {
	if testing.Short() {
		t.Skip("content manipulation skipped in short mode")
	}
	p := newProbe(t)
	startedAt := time.Now()
	var rows [][]string

	// --- master playlist manipulations: fetch without, then with ---
	base, _, err := getBytes(p.c, p.masterURL)
	if err != nil {
		t.Fatalf("baseline master fetch: %v", err)
	}
	manips := []struct {
		name string // sub-test + label
		key  string // session-map key (bool)
		gone string // substring expected to disappear ("" = no substring check)
	}{
		{"strip_codecs", "content_strip_codecs", "CODECS"},
		{"strip_average_bandwidth", "content_strip_average_bandwidth", "AVERAGE-BANDWIDTH"},
		{"overstate_bandwidth", "content_overstate_bandwidth", ""},
	}
	for _, m := range manips {
		m := m
		t.Run("master_"+m.name, func(t *testing.T) {
			if err := patchSession(p.c, p.apiBase, p.sess.SessionID, map[string]any{m.key: true}); err != nil {
				t.Fatalf("enable %s: %v", m.name, err)
			}
			defer patchSession(p.c, p.apiBase, p.sess.SessionID, map[string]any{m.key: false})
			time.Sleep(500 * time.Millisecond)

			got, _, err := getBytes(p.c, p.masterURL)
			if err != nil {
				t.Fatalf("manipulated master fetch: %v", err)
			}
			differ := !bytes.Equal(base, got)
			if !differ {
				t.Errorf("master unchanged with %s enabled — manipulation not applied", m.name)
			}
			note := fmt.Sprintf("%d→%d bytes", len(base), len(got))
			if m.gone != "" {
				baseHas := bytes.Contains(base, []byte(m.gone))
				gotHas := bytes.Contains(got, []byte(m.gone))
				note += fmt.Sprintf("; %s base=%v after=%v", m.gone, baseHas, gotHas)
				if baseHas && gotHas {
					t.Errorf("%s still present in master after %s enabled", m.gone, m.name)
				}
			}
			rows = append(rows, []string{"master/" + m.name, note, boolStr(differ)})
		})
	}

	// --- segment corruption: fetch real, then corrupted (zero-filled) ---
	t.Run("segment_corrupted", func(t *testing.T) {
		segs := p.pullOnce(t)
		if len(segs) == 0 {
			t.Fatalf("no segments to probe")
		}
		segURL := segs[0]
		real, st1, err := getBytes(p.c, segURL)
		if err != nil || st1 >= 400 || len(real) == 0 {
			t.Fatalf("baseline segment fetch: status=%d len=%d err=%v", st1, len(real), err)
		}
		// corrupted is a segment fault; freq=1/consec=1 → every fetch corrupted.
		if err := patchSession(p.c, p.apiBase, p.sess.SessionID, faultSet("segment", "corrupted", 1, 1)); err != nil {
			t.Fatalf("enable corruption: %v", err)
		}
		defer patchSession(p.c, p.apiBase, p.sess.SessionID, faultClear("segment"))
		time.Sleep(settleKernel)

		corrupt, _, err := getBytes(p.c, segURL)
		if err != nil {
			t.Fatalf("corrupted segment fetch: %v", err)
		}
		differ := !bytes.Equal(real, corrupt)
		zero := allZero(corrupt)
		if !differ {
			t.Errorf("segment bytes unchanged with corruption enabled")
		}
		rows = append(rows, []string{"segment/corrupted",
			fmt.Sprintf("%d→%d bytes, zero-filled=%v", len(real), len(corrupt), zero), boolStr(differ)})
	})

	t.Logf("\n=== content manipulation matrix ===")
	for _, r := range rows {
		t.Logf("%-26s %-44s differ=%s", r[0], r[1], r[2])
	}
	p.postServerReport(t, "server_content", fmt.Sprintf("%d manipulations", len(rows)), startedAt, !t.Failed(),
		serverMatrix{
			Title:   "Content manipulation (served bytes with vs without each control)",
			Columns: []string{"control", "observed", "bytes_differ"},
			Rows:    rows,
		})
}
