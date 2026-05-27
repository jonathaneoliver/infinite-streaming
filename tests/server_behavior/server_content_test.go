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
	"net/url"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// assertParseableMaster fails if a manipulated master playlist no longer
// parses as m3u8, or if the manipulation changed the variant count — the
// content controls edit attributes/values, they must never drop a variant or
// produce a playlist a player can't read. (parseMaster errors when it finds
// zero #EXT-X-STREAM-INF variants, so a garbled re-encode is caught here.)
func assertParseableMaster(t *testing.T, label string, body []byte, baseURL string, wantVariants int) {
	t.Helper()
	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("%s: parse base URL %q: %v", label, baseURL, err)
	}
	vs, err := parseMaster(body, u)
	if err != nil {
		t.Errorf("%s: manipulated master no longer parses as m3u8: %v", label, err)
		return
	}
	if len(vs) != wantVariants {
		t.Errorf("%s: variant count changed after manipulation: got %d, want %d", label, len(vs), wantVariants)
	}
}

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// bandwidthAttr matches the standalone BANDWIDTH attribute (preceded by ":"
// as the first STREAM-INF attr, or "," mid-line) — NOT "AVERAGE-BANDWIDTH",
// whose "-BANDWIDTH" is preceded by a letter.
var bandwidthAttr = regexp.MustCompile(`[,:]BANDWIDTH=(\d+)`)

// maxBandwidth returns the largest peak BANDWIDTH advertised in a master
// playlist, for verifying the overstate_bandwidth manipulation inflated it.
func maxBandwidth(master []byte) int {
	max := 0
	for _, m := range bandwidthAttr.FindAllSubmatch(master, -1) {
		if v, err := strconv.Atoi(string(m[1])); err == nil && v > max {
			max = v
		}
	}
	return max
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
			assertParseableMaster(t, "master/"+m.name, got, p.masterURL, len(p.variants))
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

	// --- combined manipulations: prove the controls compose ---
	// Enable two master manipulations at once and assert BOTH effects hold
	// in the same served playlist — a regression here would mean one control
	// clobbers another's edit (e.g. a re-encode dropping a prior change).
	baseMaxBW := maxBandwidth(base)
	combos := []struct {
		name      string
		keys      []string
		gone      []string // substrings that must all disappear
		overstate bool     // also assert peak BANDWIDTH inflated vs base
	}{
		{"codecs+avg_bandwidth",
			[]string{"content_strip_codecs", "content_strip_average_bandwidth"},
			[]string{"CODECS", "AVERAGE-BANDWIDTH"}, false},
		{"codecs+overstate_bandwidth",
			[]string{"content_strip_codecs", "content_overstate_bandwidth"},
			[]string{"CODECS"}, true},
	}
	for _, cb := range combos {
		cb := cb
		t.Run("master_combo_"+cb.name, func(t *testing.T) {
			set := map[string]any{}
			for _, k := range cb.keys {
				set[k] = true
			}
			if err := patchSession(p.c, p.apiBase, p.sess.SessionID, set); err != nil {
				t.Fatalf("enable combo %s: %v", cb.name, err)
			}
			defer func() {
				clr := map[string]any{}
				for _, k := range cb.keys {
					clr[k] = false
				}
				patchSession(p.c, p.apiBase, p.sess.SessionID, clr)
			}()
			time.Sleep(500 * time.Millisecond)

			got, _, err := getBytes(p.c, p.masterURL)
			if err != nil {
				t.Fatalf("combo master fetch: %v", err)
			}
			differ := !bytes.Equal(base, got)
			if !differ {
				t.Errorf("combo %s: master unchanged — controls didn't compose", cb.name)
			}
			assertParseableMaster(t, "master/combo:"+cb.name, got, p.masterURL, len(p.variants))
			note := fmt.Sprintf("%d→%d bytes", len(base), len(got))
			for _, g := range cb.gone {
				if bytes.Contains(base, []byte(g)) && bytes.Contains(got, []byte(g)) {
					t.Errorf("combo %s: %s still present after enabling %v", cb.name, g, cb.keys)
				}
				note += "; " + g + " gone"
			}
			if cb.overstate {
				gotMaxBW := maxBandwidth(got)
				note += fmt.Sprintf("; peakBW %d→%d", baseMaxBW, gotMaxBW)
				if gotMaxBW <= baseMaxBW {
					t.Errorf("combo %s: peak BANDWIDTH did not inflate (%d→%d) with overstate enabled",
						cb.name, baseMaxBW, gotMaxBW)
				}
			}
			rows = append(rows, []string{"master/combo:" + cb.name, note, boolStr(differ)})
		})
	}

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
