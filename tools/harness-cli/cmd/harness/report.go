package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/forwarder"
)

// cmdCharReport renders a STUDY comparison from the archive. Given a group_id
// (the ≤4 concurrent arms of one run) it pulls each arm's per-play summary and
// tabulates the config that VARIED (the IV — segment/pattern/… surfaced via the
// play `scenario`) against the continuous QoE metrics (the DV — TTFF/stalls/
// shifts…) plus a label-derived verdict. It ALWAYS shows the metric value, not
// just a label: labels threshold the value and hide the gradient among the
// "good" arms — the continuous numbers are what let you rank them.
//
// This is the first cut of epic #880 Gap 1 (docs/characterization-results-design.md).
// group_id is the minimal study key; Gap 0's `scenario` facet-join / `study_id`
// (to stitch N reps / >4 variations across runs) is a follow-up.
func cmdCharReport(client *api.Client, args []string, asJSON bool) error {
	fs := flag.NewFlagSet("char report", flag.ContinueOnError)
	group := fs.String("group", "", "group_id (or prefix) to compare — the run's arms")
	from := fs.String("from", "", "ISO 8601 lower bound (default: 48h ago)")
	to := fs.String("to", "", "ISO 8601 upper bound (exclusive)")
	limit := fs.Int("limit", 500, "max plays to scan for the group")
	reps := fs.Bool("reps", false, "aggregate plays by config into per-config medians (a study across reps/runs)")
	curve := fs.String("curve", "", "append a response curve (ASCII bars of a metric's median per config): ttff|frames|dropped|stalls|shifts")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *group == "" {
		return fmt.Errorf("usage: harness char report --group <group_id> [--from ISO] [--to ISO] [--limit N]")
	}

	params := &forwarder.GetApiV2PlaysParams{}
	lim := *limit
	params.Limit = &lim
	lower := time.Now().Add(-48 * time.Hour)
	if *from != "" {
		t, err := time.Parse(time.RFC3339, *from)
		if err != nil {
			return fmt.Errorf("invalid --from %q (need RFC3339): %w", *from, err)
		}
		lower = t
	}
	params.From = &lower
	if *to != "" {
		t, err := time.Parse(time.RFC3339, *to)
		if err != nil {
			return fmt.Errorf("invalid --to %q (need RFC3339): %w", *to, err)
		}
		params.To = &t
	}

	body, err := client.ArchivePlays(context.Background(), params)
	if err != nil {
		return err
	}
	var env struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("decode plays: %w", err)
	}

	// The /api/v2/plays query has no group filter yet (Gap 0) — filter client-side
	// on the group_id each row already carries.
	var rows []map[string]any
	for _, p := range env.Items {
		if g := rptStr(p["group_id"]); g != "" && (g == *group || strings.HasPrefix(g, *group)) {
			rows = append(rows, p)
		}
	}
	if len(rows) == 0 {
		return fmt.Errorf("no plays for group %q in the window — widen --from or raise --limit", *group)
	}

	if asJSON {
		out, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(out)
		return err
	}
	if *reps {
		renderStudyReps(os.Stdout, *group, rows)
	} else {
		renderStudyReport(os.Stdout, *group, rows)
	}
	if *curve != "" {
		rptRenderCurve(os.Stdout, rows, *curve)
	}
	return nil
}

// rptCurveMetric maps a --curve metric name to its per-play field.
var rptCurveMetric = map[string]string{
	"ttff":    "first_frame_s",
	"frames":  "frames_displayed",
	"dropped": "frames_dropped",
	"stalls":  "stalls",
	"shifts":  "bitrate_shifts",
}

// rptRenderCurve draws the response function: an ASCII bar chart of a metric's
// median across the IV values — the IV→DV relationship at a glance (the knee),
// not a grid. Aggregates by config regardless of --reps.
func rptRenderCurve(w io.Writer, rows []map[string]any, metric string) {
	field, ok := rptCurveMetric[metric]
	if !ok {
		fmt.Fprintf(w, "\n(unknown --curve metric %q — have: ttff frames dropped stalls shifts)\n", metric)
		return
	}
	ivCols, _ := rptDetectIV(rows)

	order := []string{}
	vals := map[string][]float64{}
	for _, p := range rows {
		key := strings.Join(rptIVKey(p, ivCols), "/")
		if _, seen := vals[key]; !seen {
			order = append(order, key)
		}
		if f, ok := rptFloat(p[field]); ok {
			vals[key] = append(vals[key], f)
		}
	}
	sort.Strings(order)

	type pt struct {
		label string
		med   float64
		n     int
	}
	var pts []pt
	maxv := 0.0
	for _, k := range order {
		xs := vals[k]
		if len(xs) == 0 {
			continue
		}
		s := append([]float64(nil), xs...)
		sort.Float64s(s)
		med := s[len(s)/2]
		if len(s)%2 == 0 {
			med = (s[len(s)/2-1] + s[len(s)/2]) / 2
		}
		pts = append(pts, pt{k, med, len(xs)})
		if med > maxv {
			maxv = med
		}
	}
	if len(pts) == 0 {
		fmt.Fprintf(w, "\n(no %s values to plot)\n", metric)
		return
	}

	fmt.Fprintf(w, "\nresponse curve — %s (median) by %s:\n", metric, strings.Join(ivCols, "/"))
	const width = 40
	for _, p := range pts {
		bars := 0
		if maxv > 0 {
			bars = int(p.med/maxv*width + 0.5)
		}
		fmt.Fprintf(w, "  %-16s %9.2f  n=%d  %s\n", p.label, p.med, p.n, strings.Repeat("█", bars))
	}
}

// rptDetectIV finds which scenario facets VARY across the plays (the IV columns)
// vs which are shared by every play (held-constant context).
func rptDetectIV(rows []map[string]any) (ivCols, constants []string) {
	facetVals := map[string]map[string]bool{}
	for _, p := range rows {
		sc, _ := p["scenario"].(map[string]any)
		for _, f := range rptScenarioFacets {
			if v := rptStr(sc[f]); v != "" {
				if facetVals[f] == nil {
					facetVals[f] = map[string]bool{}
				}
				facetVals[f][v] = true
			}
		}
	}
	for _, f := range rptScenarioFacets {
		switch len(facetVals[f]) {
		case 0: // absent on every arm — skip
		case 1:
			for v := range facetVals[f] {
				constants = append(constants, f+"="+v)
			}
		default:
			ivCols = append(ivCols, f)
		}
	}
	sort.Strings(constants)
	return ivCols, constants
}

func rptIVKey(p map[string]any, ivCols []string) []string {
	sc, _ := p["scenario"].(map[string]any)
	vals := make([]string, len(ivCols))
	for i, f := range ivCols {
		vals[i] = rptStr(sc[f])
	}
	return vals
}

// renderStudyReps groups the plays by config (IV tuple) and reports per-config
// medians across reps/runs — the characterization view: rank configs by median
// performance over N reps so n=1 noise doesn't masquerade as a result. TTFF also
// shows its min–max spread. NOTE: aggregating across runs can conflate config
// with time — a small-N study spanning hours may bake an environmental blip into
// a config's median (docs/characterization-results-design.md, Gap 0 temporal
// guard: a follow-up).
func renderStudyReps(w io.Writer, group string, rows []map[string]any) {
	ivCols, constants := rptDetectIV(rows)

	type bucket struct {
		iv       []string
		n        int
		ttff     []float64
		frames   []float64
		dropped  []float64
		stalls   []float64
		shifts   []float64
		verdicts []string
		worst    map[string]int
	}
	order := []string{}
	buckets := map[string]*bucket{}
	for _, p := range rows {
		iv := rptIVKey(p, ivCols)
		key := strings.Join(iv, "\x1f")
		b := buckets[key]
		if b == nil {
			b = &bucket{iv: iv, worst: map[string]int{}}
			buckets[key] = b
			order = append(order, key)
		}
		b.n++
		rptAddF(&b.ttff, p["first_frame_s"])
		rptAddF(&b.frames, p["frames_displayed"])
		rptAddF(&b.dropped, p["frames_dropped"])
		rptAddF(&b.stalls, p["stalls"])
		rptAddF(&b.shifts, p["bitrate_shifts"])
		verdict, worst := rptVerdict(p["label_histogram"])
		b.verdicts = append(b.verdicts, verdict)
		if worst != "" {
			b.worst[worst]++
		}
	}
	sort.Strings(order)

	fmt.Fprintf(w, "study report — %s  (%d configs, %d plays, aggregated by config)\n", group, len(order), len(rows))
	if len(constants) > 0 {
		fmt.Fprintf(w, "held constant: %s\n", strings.Join(constants, "  "))
	}
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	hdr := append([]string{}, ivCols...)
	hdr = append(hdr, "n", "TTFF_med", "TTFF_range", "frames_med", "dropped_med", "stalls_med", "shifts_med", "verdict", "worst_qoe")
	fmt.Fprintln(tw, strings.Join(hdr, "\t"))
	for _, k := range order {
		b := buckets[k]
		cells := make([]string, 0, len(hdr))
		for _, v := range b.iv {
			cells = append(cells, rptDash(v))
		}
		cells = append(cells,
			fmt.Sprintf("%d", b.n),
			rptMed(b.ttff, 2),
			rptRange(b.ttff, 2),
			rptMed(b.frames, 0),
			rptMed(b.dropped, 0),
			rptMed(b.stalls, 0),
			rptMed(b.shifts, 0),
			rptWorstVerdict(b.verdicts),
			rptDash(rptTopKey(b.worst)),
		)
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	tw.Flush()
	fmt.Fprintln(w, "\nper-config medians over reps — rank by TTFF_med etc.; TTFF_range flags rep spread (noise).")
	fmt.Fprintln(w, "verdict = worst qoe_tier across the config's reps; worst_qoe = its most-common worst QoE label.")
}

func rptAddF(dst *[]float64, v any) {
	if f, ok := rptFloat(v); ok {
		*dst = append(*dst, f)
	}
}

func rptMed(xs []float64, dp int) string {
	if len(xs) == 0 {
		return "-"
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	m := s[n/2]
	if n%2 == 0 {
		m = (s[n/2-1] + s[n/2]) / 2
	}
	return fmt.Sprintf("%.*f", dp, m)
}

func rptRange(xs []float64, dp int) string {
	if len(xs) < 2 {
		return "-"
	}
	lo, hi := xs[0], xs[0]
	for _, x := range xs {
		if x < lo {
			lo = x
		}
		if x > hi {
			hi = x
		}
	}
	return fmt.Sprintf("%.*f–%.*f", dp, lo, dp, hi)
}

// rptWorstVerdict returns the worst qoe_tier verdict word across a config's reps.
func rptWorstVerdict(vs []string) string {
	rank := map[string]int{"premium": 0, "ok": 1, "warn": 2, "BAD": 3}
	worst, wr := "", -1
	for _, v := range vs {
		if r, ok := rank[v]; ok && r > wr {
			wr, worst = r, v
		}
	}
	if worst == "" {
		return "-"
	}
	return worst
}

func rptTopKey(m map[string]int) string {
	best, bn := "", 0
	for k, n := range m {
		if n > bn {
			bn, best = n, k
		}
	}
	return best
}

// rptScenarioFacets are the play-identity fields treated as IV candidates, in
// display order. A facet that varies across the group becomes a column; one
// shared by every arm is printed once as held-constant context.
var rptScenarioFacets = []string{"manifest_variant", "platform", "content_id", "device_model", "os_version", "app_version", "test"}

func renderStudyReport(w io.Writer, group string, rows []map[string]any) {
	ivCols, constants := rptDetectIV(rows)

	fmt.Fprintf(w, "study report — group %s  (%d arms)\n", group, len(rows))
	if len(constants) > 0 {
		fmt.Fprintf(w, "held constant: %s\n", strings.Join(constants, "  "))
	}
	fmt.Fprintln(w)

	// Sort arms by the IV columns so the gradient reads top-to-bottom.
	sort.SliceStable(rows, func(i, j int) bool {
		si, _ := rows[i]["scenario"].(map[string]any)
		sj, _ := rows[j]["scenario"].(map[string]any)
		for _, f := range ivCols {
			a, b := rptStr(si[f]), rptStr(sj[f])
			if a != b {
				return a < b
			}
		}
		return false
	})

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	hdr := append([]string{}, ivCols...)
	hdr = append(hdr, "TTFF_s", "frames", "dropped", "stalls", "stall_s", "shifts", "res_chg", "verdict", "worst_qoe", "end", "play")
	fmt.Fprintln(tw, strings.Join(hdr, "\t"))
	for _, p := range rows {
		sc, _ := p["scenario"].(map[string]any)
		cells := make([]string, 0, len(hdr))
		for _, f := range ivCols {
			cells = append(cells, rptDash(rptStr(sc[f])))
		}
		verdict, worst := rptVerdict(p["label_histogram"])
		cells = append(cells,
			rptNum(p["first_frame_s"]),
			rptNum(p["frames_displayed"]),
			rptNum(p["frames_dropped"]),
			rptNum(p["stalls"]),
			rptMsToS(p["stalling_time_ms"]),
			rptNum(p["bitrate_shifts"]),
			rptNum(p["resolution_changes"]),
			verdict,
			rptDash(worst),
			rptDash(rptStr(p["last_state"])),
			rptShort8(rptStr(p["play_id"])),
		)
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	tw.Flush()
	fmt.Fprintln(w, "\nmetrics are shown ALWAYS (not only when a label fires) — labels threshold the value and hide the")
	fmt.Fprintln(w, "gradient among the 'good' arms; compare the numbers to rank them.")
	fmt.Fprintln(w, "verdict = qoe_tier end-state (premium/ok/BAD); worst_qoe = worst QoE label (lifecycle/teardown excluded).")
}

// rptVerdict computes a QoE verdict + the worst QoE-relevant label from a
// label_histogram ([ "<severity>=<event>", count ] pairs). The verdict prefers
// the forwarder's authoritative qoe_tier_* END-STATE rollup
// (premium/acceptable/unacceptable, qoe_labels.go); absent that it falls back to
// the worst QoE-scoped severity. Lifecycle/teardown labels (unexpected_*, etc.)
// are excluded — they're error-tier but describe how the play *ended* (the
// harness stops it at duration), not how it *performed*.
func rptVerdict(v any) (verdict, worstQoE string) {
	arr, _ := v.([]any)
	tier := ""
	best := 0
	for _, e := range arr {
		pair, _ := e.([]any)
		if len(pair) == 0 {
			continue
		}
		lbl := rptStr(pair[0])
		i := strings.IndexByte(lbl, '=')
		if i < 0 {
			continue
		}
		sev := lbl[:i]
		event := strings.TrimPrefix(lbl[i+1:], "*") // strip the synth-label mark
		if strings.HasPrefix(event, "qoe_tier_") {
			tier = event
			continue
		}
		if rptLifecycleLabel(event) {
			continue
		}
		if r := rptSeverityRank(sev); r > best {
			best, worstQoE = r, event
		}
	}
	switch tier {
	case "qoe_tier_premium":
		verdict = "premium"
	case "qoe_tier_acceptable":
		verdict = "ok"
	case "qoe_tier_unacceptable":
		verdict = "BAD"
	default:
		verdict = rptSeverityWord(best) // no tier — fall back to the worst QoE severity
	}
	return verdict, worstQoE
}

// rptLifecycleLabel reports labels that describe session lifecycle/teardown
// rather than playback QoE — excluded from the verdict + worst-label so a
// harness-stopped play doesn't read as "BAD".
func rptLifecycleLabel(event string) bool {
	switch event {
	case "unexpected_end", "unexpected_fault", "unexpected_startup",
		"first_frame", "play_start", "session_start", "server_start", "loop_server":
		return true
	}
	return false
}

func rptSeverityRank(sev string) int {
	switch sev {
	case "error":
		return 4
	case "critical":
		return 3
	case "warning":
		return 2
	case "info":
		return 1
	}
	return 0
}

func rptSeverityWord(rank int) string {
	switch {
	case rank >= 3: // error / critical
		return "BAD"
	case rank == 2: // warning
		return "warn"
	default:
		return "ok"
	}
}

// --- small any→string/number helpers (JSON numbers arrive as float64) ---

func rptStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

func rptDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func rptNum(v any) string {
	f, ok := rptFloat(v)
	if !ok {
		return "-"
	}
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%.2f", f)
}

func rptMsToS(v any) string {
	f, ok := rptFloat(v)
	if !ok {
		return "-"
	}
	return fmt.Sprintf("%.1f", f/1000)
}

func rptFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		// ClickHouse serializes UInt64 counts (frames_displayed, bitrate_shifts,
		// resolution_changes, …) as JSON strings to avoid precision loss.
		if t == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func rptShort8(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
