// server_pattern_test.go — issue #521.
//
// Verifies the proxy's pattern step engine using the BUILT-IN templates
// (square_wave / ramp_up / ramp_down / pyramid), swept across step
// durations, with the step caps OFFSET by the measured delivery factor so
// the *delivered* throughput should land on the encoded variant rate.
//
// Method:
//  1. Measure the delivery factor (server_limit-style): set a known cap,
//     Range-pull, factor = observed/configured (~0.95 = wire overhead).
//  2. For each template, mirror the dashboard's buildSteps on the raw
//     variant rates, then set each step cap = encoded/factor so the
//     delivered rate ≈ the encoded variant rate (replaces the hardwired
//     ~5% pattern margin with the measured one).
//  3. Pull with the Range-budget technique, attributing each fetch to the
//     engine's OWN live step (nftables_pattern_step / _rate_runtime_mbps)
//     rather than our clock, and report delivered-vs-encoded-target % per
//     step.
//
// Run:
//
//	cd tests/server_behavior && go test -v -run TestServerPattern -timeout 30m
//
// Env:
//
//	THROUGHPUT_HOST, THROUGHPUT_API_PORT, THROUGHPUT_INSECURE, THROUGHPUT_CONTENT
//	PATTERN_TEMPLATES=pyramid          built-in templates (CSV); add ramp_up,ramp_down,square_wave
//	PATTERN_STEP_DURATIONS=6,12,24     per-step durations (s) to sweep
//	PATTERN_CAL_MBPS=50  PATTERN_CAL_S=8   calibration cap + window for the factor
package server_behavior

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

// patternStep mirrors the proxy's PatternStep (cmd/server/openapi.go).
type patternStep struct {
	RateMbps        float64 `json:"rate_mbps"`
	DurationSeconds int     `json:"duration_seconds"`
	Enabled         bool    `json:"enabled"`
}

// applyPattern installs a shaping pattern on the session's internal port.
// steps=nil clears any active pattern.
func applyPattern(c *http.Client, apiBase string, internalPort int, steps []patternStep, templateMode string) error {
	u := fmt.Sprintf("https://%s/api/nftables/pattern/%d", apiBase, internalPort)
	payload := map[string]any{"steps": steps, "template_mode": templateMode, "delay_ms": 0, "loss_pct": 0}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, u, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("applyPattern %s: %d: %s", u, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// variantRatesAsc returns the variant bandwidths in Mbps, ascending + deduped.
func variantRatesAsc(vs []variant) []float64 {
	out := make([]float64, 0, len(vs))
	for _, v := range vs {
		if v.BandwidthBps > 0 {
			out = append(out, float64(v.BandwidthBps)/1e6)
		}
	}
	sort.Float64s(out)
	uniq := out[:0]
	for i, r := range out {
		if i == 0 || r != out[i-1] {
			uniq = append(uniq, r)
		}
	}
	return uniq
}

// patternTemplateSeq mirrors the dashboard's buildSteps (NetworkShapingPattern.vue):
// the per-step rate sequence each built-in template produces from the
// variant ladder (margin applied separately as the offset).
func patternTemplateSeq(ratesAsc []float64, template string) []float64 {
	switch template {
	case "square_wave":
		if len(ratesAsc) < 2 {
			return append([]float64{}, ratesAsc...)
		}
		return []float64{ratesAsc[0], ratesAsc[len(ratesAsc)-1]}
	case "ramp_up":
		return append([]float64{}, ratesAsc...)
	case "ramp_down":
		out := append([]float64{}, ratesAsc...)
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
		return out
	case "pyramid":
		out := append([]float64{}, ratesAsc...)
		for i := len(ratesAsc) - 2; i >= 0; i-- {
			out = append(out, ratesAsc[i])
		}
		return out
	case "transient_shock":
		// Deepening-dip staircase: top, r[n-2], top, …, top, r[0], top.
		if len(ratesAsc) == 0 {
			return nil
		}
		top := ratesAsc[len(ratesAsc)-1]
		out := []float64{top}
		for i := len(ratesAsc) - 2; i >= 0; i-- {
			out = append(out, ratesAsc[i], top)
		}
		return out
	default:
		return nil
	}
}

func (p *probe) firstSeg(t *testing.T) string {
	if segs := p.pullOnce(t); len(segs) > 0 {
		return segs[0]
	}
	return ""
}

type patStepResult struct {
	template    string
	stepSecs    int
	idx         int
	encodedMbps float64 // intended delivered rate (the variant rate)
	capMbps     float64 // configured cap (= encoded / factor)
	obsMbps     float64
	secs        float64
}

func TestServerPattern(t *testing.T) {
	if testing.Short() {
		t.Skip("pattern fidelity skipped in short mode")
	}
	templates := strings.Split(env("PATTERN_TEMPLATES", "pyramid"), ",")
	durs, err := parseRates(env("PATTERN_STEP_DURATIONS", "6,12,24"))
	if err != nil {
		t.Fatalf("parse PATTERN_STEP_DURATIONS: %v", err)
	}

	p := newProbe(t)
	startedAt := time.Now()
	rates := variantRatesAsc(p.variants)
	if len(rates) == 0 {
		t.Fatalf("no variant rates discovered")
	}
	t.Logf("variant ladder (Mbps): %v", rates)

	// --- 1. Measure the delivery factor at a known cap (server_limit-style).
	calCap := envInt("PATTERN_CAL_MBPS", 50)
	if err := setRateLimit(p.c, p.apiBase, p.sess.InternalPort, calCap); err != nil {
		t.Fatalf("set calibration cap: %v", err)
	}
	time.Sleep(settleKernel)
	calRes := runRateWindow(t, p, p.firstSeg(t), calCap, time.Duration(envInt("PATTERN_CAL_S", 8))*time.Second)
	factor := calRes.avgMbps / float64(calCap)
	if factor <= 0 || factor > 1.2 {
		t.Logf("calibration factor %.3f implausible (cal %d→%.2f) — falling back to 0.95", factor, calCap, calRes.avgMbps)
		factor = 0.95
	}
	_ = setRateLimit(p.c, p.apiBase, p.sess.InternalPort, 0)
	t.Logf("measured delivery factor = %.3f (cal %d Mbps → %.2f Mbps observed)", factor, calCap, calRes.avgMbps)

	var results []patStepResult

	for _, tmpl := range templates {
		tmpl = strings.TrimSpace(tmpl)
		for _, d := range durs {
			d := d
			tmpl := tmpl
			seq := patternTemplateSeq(rates, tmpl)
			if len(seq) == 0 {
				t.Logf("template %q produced no steps; skipping", tmpl)
				continue
			}
			// Offset each cap so the DELIVERED rate ≈ the encoded variant rate.
			steps := make([]patternStep, len(seq))
			for i, enc := range seq {
				steps[i] = patternStep{
					RateMbps:        math.Round(enc/factor*100) / 100,
					DurationSeconds: d,
					Enabled:         true,
				}
			}
			t.Run(fmt.Sprintf("%s_%ds", tmpl, d), func(t *testing.T) {
				if err := applyPattern(p.c, p.apiBase, p.sess.InternalPort, steps, tmpl); err != nil {
					t.Fatalf("apply %s pattern: %v", tmpl, err)
				}
				defer func() {
					applyPattern(p.c, p.apiBase, p.sess.InternalPort, nil, tmpl)
					setShapeFull(p.c, p.apiBase, p.sess.InternalPort, 0, 0, 0)
				}()
				time.Sleep(settleKernel)

				start := time.Now()
				// Run a bit past the nominal schedule so the engine reaches
				// the last step even with arm/settle lag.
				deadline := start.Add(time.Duration(d*len(steps))*time.Second + 3*time.Second)
				type bucket struct {
					bytes int64
					secs  float64
				}
				buckets := make([]bucket, len(steps))
				stepFirstSeen := make([]time.Time, len(steps))
				seg := p.firstSeg(t)
				var ewma float64
				maxStepSeen := 0
				// Skip a short settle after each step first appears so the
				// htb cap-change burst/starve doesn't bias the steady-state.
				settle := 2.5
				if float64(d)/3 < settle {
					settle = float64(d) / 3
				}
				for time.Now().Before(deadline) {
					if seg == "" {
						seg = p.firstSeg(t)
						if seg == "" {
							time.Sleep(150 * time.Millisecond)
							continue
						}
					}
					// Attribute this fetch to the engine's OWN live step, not
					// our clock (the engine's pattern start lags `start` by an
					// unknown settle, so elapsed/d mis-attributes fetches).
					m, merr := getSessionMap(p.c, p.apiBase, p.playerID)
					if merr != nil {
						continue
					}
					liveStep := 0
					if v, ok := mapFloat(m, "nftables_pattern_step"); ok {
						liveStep = int(v)
					}
					liveCap, _ := mapFloat(m, "nftables_pattern_rate_runtime_mbps")
					if liveStep > maxStepSeen {
						maxStepSeen = liveStep
					}
					capForFetch := liveCap
					if capForFetch <= 0 {
						capForFetch = steps[0].RateMbps
					}
					perFetch := int64(capForFetch * 1e6 / 8 * 1.5)
					if perFetch < 64*1024 {
						perFetch = 64 * 1024
					}
					t0 := time.Now()
					n, ferr := rangeGet(p.c, seg, perFetch, 30*time.Second)
					if ferr != nil {
						seg = ""
						continue
					}
					dt := time.Since(t0).Seconds()
					if liveStep >= 1 && liveStep <= len(steps) {
						bi := liveStep - 1
						if stepFirstSeen[bi].IsZero() {
							stepFirstSeen[bi] = time.Now()
						}
						// Only count once the cap-change transition has settled.
						if time.Since(stepFirstSeen[bi]).Seconds() >= settle {
							buckets[bi].bytes += n
							buckets[bi].secs += dt
						}
					}
					if dt > 0 {
						inst := float64(n) * 8 / 1e6 / dt
						if ewma == 0 {
							ewma = inst
						} else {
							ewma = ewma*0.7 + inst*0.3
						}
					}
					p.heartbeat(round2(ewma), round2(ewma), time.Since(start).Seconds(), "playing")
				}
				if maxStepSeen < len(steps) {
					t.Errorf("%s %ds: engine only reached step %d of %d — schedule didn't complete",
						tmpl, d, maxStepSeen, len(steps))
				}

				for i := range steps {
					obs := 0.0
					if buckets[i].secs > 0 {
						obs = float64(buckets[i].bytes) * 8 / 1e6 / buckets[i].secs
					}
					results = append(results, patStepResult{
						template: tmpl, stepSecs: d, idx: i + 1,
						encodedMbps: seq[i], capMbps: steps[i].RateMbps,
						obsMbps: obs, secs: buckets[i].secs,
					})
					// Step 1 is the unshaped→first-cap entry transition (the
					// throttle can lag the engine's step report), so report but
					// don't fail on it. For the rest, with the cap offset the
					// delivered rate should land on the encoded target — allow
					// ±25% (short steps have few steady-state samples; the
					// duration sweep shows this tighten at 12/24s).
					switch {
					case i == 0:
						t.Logf("%s %ds step 1 (entry, target %.2f): delivered %.2f — entry transition, not asserted", tmpl, d, seq[i], obs)
					case buckets[i].secs < 2 || seq[i] <= 0:
						t.Logf("%s %ds step %d (%.2f Mbps): %.1fs steady-state — too few samples to assert", tmpl, d, i+1, seq[i], buckets[i].secs)
					default:
						// Duration-aware band: short steps are transition-
						// dominated (that's the finding, not a regression), so
						// tolerate more; long steps must be tight. Empirically
						// ~±40% at 6s, ±20% at 12s, ±10% at 24s.
						band := 240.0 / float64(d)
						if band < 10 {
							band = 10
						}
						diff := (obs - seq[i]) / seq[i] * 100
						if math.Abs(diff) > band {
							t.Errorf("%s %ds step %d: delivered %.2f vs encoded target %.2f (%+.0f%%) — outside ±%.0f%%",
								tmpl, d, i+1, obs, seq[i], diff, band)
						}
					}
				}
			})
		}
	}

	sm := serverMatrix{
		Title:   fmt.Sprintf("Pattern fidelity — caps offset by measured factor %.3f; delivered vs encoded target", factor),
		Columns: []string{"template", "step_s", "step", "encoded_target", "set_cap", "obs_mbps", "diff_vs_target"},
	}
	for _, r := range results {
		diff := "—"
		if r.secs > 0 && r.encodedMbps > 0 {
			diff = fmt.Sprintf("%+.1f%%", (r.obsMbps-r.encodedMbps)/r.encodedMbps*100)
		}
		sm.Rows = append(sm.Rows, []string{
			r.template,
			fmt.Sprintf("%d", r.stepSecs),
			fmt.Sprintf("%d", r.idx),
			fmt.Sprintf("%.2f", r.encodedMbps),
			fmt.Sprintf("%.2f", r.capMbps),
			fmt.Sprintf("%.2f", r.obsMbps),
			diff,
		})
	}
	p.postServerReport(t, "server_pattern",
		fmt.Sprintf("%s × %v s (factor %.3f)", strings.Join(templates, ","), durs, factor),
		startedAt, !t.Failed(), sm)
}
