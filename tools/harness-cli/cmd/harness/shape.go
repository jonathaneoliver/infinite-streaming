package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jonathaneoliver/infinite-streaming/go-proxy/pkg/ladder"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/format"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/sweep"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/proxy"
)

const shapeUsage = `harness shape <target> [flags]

Slider mode (any subset; omitted fields are not modified):
  --rate FLOAT       rate cap in Mbps (e.g. 1.5)
  --delay FLOAT      one-way delay in ms (e.g. 200; observed RTT ≈ delay)
  --loss FLOAT       packet loss %% (e.g. 0.5, range 0–100)
  --jitter FLOAT     delay variation in ms (stddev, normal distribution)
  --loss-corr FLOAT  loss burst correlation %% (0 = uniform; higher = burstier)
  --jitter-corr FLOAT delay-distribution correlation %% (~25 ≈ real link)

Named link profiles (#826) — applies a whole impairment recipe at once:
  --profile NAME     clean | home | mobile-good | mobile-poor |
                     nlc-wifi | nlc-wifi-ac | nlc-lte | nlc-dsl | nlc-3g |
                     nlc-edge | nlc-very-bad | nlc-100-loss
                     (individual --delay/--loss/… flags override the profile)

Pattern mode (generates a step list from the player's current variants):
  --pattern NAME     pyramid | valley | ramp_up | ramp_down | square_wave | transient_shock | sliders
  --step-seconds N   per-step duration: 6 | 12 | 18 | 24 | 60 | 120 (default 12;
                     60/120 give buffer-draining holds for transient_shock)
  --margin PCT       flat headroom above each variant rate: 0|5|10|25|50
                     (default 5; covers TCP/IP+TLS+HTTP framing; 0 is a
                     deliberate-stall footgun)
  --max-step RATIO   fill density: max ratio between consecutive caps before
                     a geometric fill is inserted (default 1.15). The ladder
                     carries BOTH a peak (BANDWIDTH) and an average
                     (AVERAGE-BANDWIDTH) rung per variant, then fills the
                     gaps — raise --max-step to coarsen + shorten the pattern
                     (a pyramid over a dense ladder can run ~13 min/cycle)
  --top-headroom PCT start the ladder this %% over the top variant's peak
                     (default 50; adds a headroom start rung above the
                     top anchor so playback settles before constraining;
                     0 disables it)
  --broadcast BOOL   group fan-out for the pattern arm (only valid with
                     --pattern): false = arm the pattern on THIS player only
                     (e.g. the group master) so other members don't each start
                     their own pattern engine; true = force broadcast; unset =
                     server default (a normal group broadcasts shape to all)
  --clear-pattern    stop any running pattern (back to slider rate)
  --show-pattern     print current pattern + active step

Wipe:
  --clear            send {"shape": null} — drops rate/delay/loss/pattern/transport
  --show             print current shape without modifying

Examples:
  harness shape ipad --rate 1.5 --delay 100
  harness shape ipad --pattern pyramid
  harness shape ipad --pattern ramp_up --step-seconds 18 --margin 10
  harness shape ipad --clear-pattern
  harness shape ipad --clear

Pattern semantics:
  pyramid          ascending variant rates, then descending (without apex dupe)
  valley           descending then ascending (high->low->high) — inverse of pyramid;
                   starts at the top so the player cold-starts cleanly (no startup cap)
  ramp_up          ascending rates, single sweep
  ramp_down        descending rates, single sweep
  square_wave      alternate lowest + highest variant
  transient_shock  hold top, dip to each lower rung in turn (deepening),
                   recovering to top between dips — the deepening-drop staircase
  sliders          empty step list (kernel falls back to --rate)

Every mutation is checkpointed to ~/.claude/state/harness/<repo>/.
'harness undo' replays the prior shape verbatim.
`

func cmdShape(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 {
		return errors.New(shapeUsage)
	}
	fs := flag.NewFlagSet("shape", flag.ContinueOnError)
	rate := fs.Float64("rate", -1, "rate cap Mbps")
	delay := fs.Float64("delay", -1, "delay ms")
	loss := fs.Float64("loss", -1, "loss %")
	jitter := fs.Float64("jitter", -1, "delay variation (stddev) ms")
	lossCorr := fs.Float64("loss-corr", -1, "loss burst correlation %")
	jitterCorr := fs.Float64("jitter-corr", -1, "delay-distribution correlation %")
	profile := fs.String("profile", "", "named link profile (clean|home|mobile-good|mobile-poor|nlc-*)")
	pattern := fs.String("pattern", "", "pattern template (pyramid|valley|ramp_up|ramp_down|square_wave|transient_shock|sliders)")
	stepSeconds := fs.Int("step-seconds", 12, "per-step duration: 6|12|18|24|60|120")
	margin := fs.Int("margin", 5, "headroom %% above variant rate: 0|5|10|25|50 (5 covers protocol overhead)")
	maxStep := fs.Float64("max-step", ladder.DefaultMaxStep, "max ratio between consecutive caps before a geometric fill is inserted (default 1.15; raise to coarsen + shorten the pattern)")
	topHeadroom := fs.Float64("top-headroom", ladder.DefaultTopHeadroomPct, "start the ladder this %% over the top variant's peak (default 50; 0 disables the headroom start rung)")
	broadcast := fs.String("broadcast", "", "group broadcast for the --pattern arm: false|true (unset = server default)")
	clearPattern := fs.Bool("clear-pattern", false, "stop any running pattern")
	showPattern := fs.Bool("show-pattern", false, "print current pattern, don't modify")
	clear := fs.Bool("clear", false, "send {shape:null}")
	show := fs.Bool("show", false, "print current shape, don't modify")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	target := args[0]
	ctx := context.Background()
	pid, err := client.Resolve(ctx, target)
	if err != nil {
		return err
	}
	broadcastPtr, berr := parseBroadcast(*broadcast)
	if berr != nil {
		return berr
	}
	if broadcastPtr != nil && *pattern == "" {
		return errors.New("--broadcast is only valid with --pattern (it controls whether the pattern arm broadcasts to the group)")
	}

	switch {
	case *show:
		return showShape(client, ctx, pid, asJSON)
	case *showPattern:
		return showPattern_(client, ctx, pid, asJSON)
	case *clear:
		return doClear(client, ctx, pid, asJSON, rate, delay, loss, pattern, clearPattern)
	case *clearPattern:
		return doClearPattern(client, ctx, pid, asJSON)
	case *pattern != "":
		return doPattern(client, ctx, pid, asJSON, *pattern, *stepSeconds, *margin, *maxStep, *topHeadroom, broadcastPtr)
	}

	imp := sliderImpairment{
		rate: *rate, delay: *delay, loss: *loss,
		jitter: *jitter, lossCorr: *lossCorr, jitterCorr: *jitterCorr,
		profile: *profile,
	}
	if imp.profile == "" && imp.rate < 0 && imp.delay < 0 && imp.loss < 0 &&
		imp.jitter < 0 && imp.lossCorr < 0 && imp.jitterCorr < 0 {
		return errors.New("nothing to do — pass --rate/--delay/--loss/--jitter/--loss-corr/--jitter-corr, --profile NAME, --pattern NAME, --clear-pattern, --clear, or --show")
	}
	return doSliderShape(client, ctx, pid, asJSON, imp)
}

// sliderImpairment bundles the static-shape flags. A flag left at -1 means
// "unset — don't touch". `profile` (if set) seeds the shape from a named link
// profile; explicitly-set axis flags then override on top.
type sliderImpairment struct {
	rate, delay, loss            float64
	jitter, lossCorr, jitterCorr float64
	profile                      string
}

// cmdReset clears a player's shape + fault rules + content to a clean baseline
// (the comprehensive ResetSession merge-patch). The harness calls this to give a
// reused player_id a known-clean proxy state before a test; manual sessions never
// invoke it, so their carry-over is untouched (see feedback_manual_proxy_carryover).
func cmdReset(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 {
		return errors.New("usage: reset <player_id|target> — clear shape + fault_rules + content to a clean baseline")
	}
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	action := fs.String("action", "harness reset session", "control-event action label")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	ctx := context.Background()
	pid, err := client.Resolve(ctx, args[0])
	if err != nil {
		return err
	}
	if _, err := client.ResetSession(ctx, pid, *action); err != nil {
		return err
	}
	if asJSON {
		fmt.Printf("{\"player_id\":%q,\"reset\":true}\n", pid)
	} else {
		fmt.Printf("reset %s → clean baseline (shape + fault_rules + content cleared)\n", pid)
	}
	return nil
}

func showShape(client *api.Client, ctx context.Context, pid string, asJSON bool) error {
	rec, etag, err := client.Player(ctx, pid)
	if err != nil {
		return err
	}
	// Read the deployment baseline so we can distinguish "operator
	// hasn't set anything" (rate==null/0; effective = baseline) from
	// "operator explicitly set N Mbps" (rate==N>0; effective = N).
	// Failure to fetch info is non-fatal — we just lose the baseline
	// annotation and fall back to printing whatever Shape carries.
	info, infoErr := client.Info(ctx)
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{
			"player_id":         pid,
			"shape":             rec.Shape,
			"etag":              etag,
			"default_rate_mbps": info.DefaultRateMbps,
		})
	}
	rateOverride := operatorRateMbps(rec.Shape)
	switch {
	case rateOverride > 0:
		// Explicit override. Note where it sits relative to baseline so
		// the operator can tell at a glance whether they're tighter
		// than the deployment floor.
		var hint string
		if info.DefaultRateMbps > 0 {
			switch {
			case rateOverride < float64(info.DefaultRateMbps):
				hint = fmt.Sprintf(" (tighter than baseline %d Mbps)", info.DefaultRateMbps)
			case rateOverride > float64(info.DefaultRateMbps):
				hint = fmt.Sprintf(" (above baseline %d Mbps)", info.DefaultRateMbps)
			default:
				hint = " (matches baseline)"
			}
		}
		fmt.Printf("%s: rate override %g Mbps%s\n", pid, rateOverride, hint)
	case info.DefaultRateMbps > 0:
		// "No override" on a deployment with a baseline → baseline applies.
		fmt.Printf("%s: at baseline (no override) — kernel cap %d Mbps\n", pid, info.DefaultRateMbps)
	default:
		// "No override" on a deployment with no baseline → truly unlimited.
		if infoErr != nil {
			fmt.Printf("%s: no shaping (couldn't read /api/v2/info: %v)\n", pid, infoErr)
		} else {
			fmt.Printf("%s: no shaping (deployment baseline is 0 — unlimited)\n", pid)
		}
	}
	if rec.Shape != nil {
		// Emit the full Shape too — the human-readable summary above
		// covers the rate dimension; this surfaces delay/loss/pattern
		// so the operator sees the full picture in one command.
		return format.JSON(os.Stdout, rec.Shape)
	}
	return nil
}

// operatorRateMbps extracts the operator's rate override from a Shape.
// Returns 0 when the shape is nil, the field is unset, or the field
// is 0 — all three mean "no override" under the issue #480 framing.
func operatorRateMbps(sh *proxy.Shape) float64 {
	if sh == nil || sh.RateMbps == nil {
		return 0
	}
	return float64(*sh.RateMbps)
}

func showPattern_(client *api.Client, ctx context.Context, pid string, asJSON bool) error {
	rec, etag, err := client.Player(ctx, pid)
	if err != nil {
		return err
	}
	if rec.Shape == nil || rec.Shape.Pattern == nil {
		if asJSON {
			return format.JSON(os.Stdout, map[string]any{"player_id": pid, "pattern": nil, "etag": etag})
		}
		fmt.Printf("%s: no pattern active\n", pid)
		return nil
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{
			"player_id": pid, "pattern": rec.Shape.Pattern, "etag": etag,
		})
	}
	p := rec.Shape.Pattern
	tname := "(custom)"
	if p.Template != nil {
		tname = string(*p.Template)
	}
	fmt.Printf("%s: pattern=%s steps=%d", pid, tname, len(p.Steps))
	if p.DefaultStepSeconds != nil {
		fmt.Printf(" step_seconds=%d", int(*p.DefaultStepSeconds))
	}
	if p.MarginPct != nil {
		fmt.Printf(" margin_pct=%d", int(*p.MarginPct))
	}
	fmt.Println()
	for i, s := range p.Steps {
		enabled := "·"
		if s.Enabled != nil && !*s.Enabled {
			enabled = "✗"
		}
		fmt.Printf("  %s step %2d  %6.3f Mbps  %ds\n", enabled, i+1, s.RateMbps, s.DurationSeconds)
	}
	return nil
}

func doClear(client *api.Client, ctx context.Context, pid string, asJSON bool,
	rate, delay, loss *float64, pattern *string, clearPattern *bool) error {
	if *rate >= 0 || *delay >= 0 || *loss >= 0 || *pattern != "" || *clearPattern {
		return errors.New("--clear is mutually exclusive with other shape flags")
	}
	newETag, err := client.ClearShape(ctx, pid, "shape clear")
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{
			"player_id": pid, "cleared": true, "etag": newETag,
		})
	}
	fmt.Printf("cleared shape on %s (etag %s)\n", pid, shortRev(newETag))
	return nil
}

func doSliderShape(client *api.Client, ctx context.Context, pid string, asJSON bool, imp sliderImpairment) error {
	action := fmt.Sprintf("shape rate=%v delay=%v loss=%v jitter=%v loss_corr=%v jitter_corr=%v profile=%q",
		imp.rate, imp.delay, imp.loss, imp.jitter, imp.lossCorr, imp.jitterCorr, imp.profile)

	// Build the merge-patch as a map: it's the only way to express the
	// rate-clears-pattern sentinel (pattern:null), which the typed
	// proxy.Shape can't carry because Pattern is `omitempty`. A profile
	// seeds the block; explicitly-set axis flags then override on top.
	shape := map[string]any{}
	setsRate := false

	if imp.profile != "" {
		prof, ok := sweep.ResolveLinkProfile(imp.profile)
		if !ok {
			return fmt.Errorf("unknown link profile %q (known: %s)", imp.profile, strings.Join(sweep.LinkProfileNames(), ", "))
		}
		// Deterministic IMPAIRMENT: always set all five impairment axes (omitted
		// ones → 0, so `clean` truly clears and no stale jitter/loss leaks).
		// Throughput is the OVERLAY axis: only the NLC presets (which model a
		// full link's bandwidth) pin rate_mbps; the four recipes leave the
		// operator's throughput cap alone so impairment can be stamped on top.
		shape["delay_ms"] = derefOr(prof.DelayMs, 0)
		shape["loss_pct"] = derefOr(prof.LossPct, 0)
		shape["jitter_ms"] = derefOr(prof.JitterMs, 0)
		shape["loss_correlation_pct"] = derefOr(prof.LossCorrelationPct, 0)
		shape["jitter_correlation_pct"] = derefOr(prof.JitterCorrelationPct, 0)
		if prof.RateMbps != nil {
			shape["rate_mbps"] = *prof.RateMbps
			setsRate = true
		}
	}

	// Explicit flags override the profile seed (and are the only source
	// in the no-profile case). -1 means "unset — leave as-is".
	if imp.rate >= 0 {
		shape["rate_mbps"] = imp.rate
		setsRate = true
	}
	if imp.delay >= 0 {
		shape["delay_ms"] = imp.delay
	}
	if imp.loss >= 0 {
		shape["loss_pct"] = imp.loss
	}
	if imp.jitter >= 0 {
		shape["jitter_ms"] = imp.jitter
	}
	if imp.lossCorr >= 0 {
		shape["loss_correlation_pct"] = imp.lossCorr
	}
	if imp.jitterCorr >= 0 {
		shape["jitter_correlation_pct"] = imp.jitterCorr
	}

	// Setting a static rate disarms any active throughput pattern —
	// they're mutually exclusive sources-of-truth for the kernel cap.
	// Delay/loss/jitter are orthogonal axes that coexist with a running
	// pattern, so a delay-only edit leaves the pattern armed.
	if setsRate {
		shape["pattern"] = nil
	}

	newETag, err := client.PatchShapeMap(ctx, pid, action, shape)
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{
			"player_id": pid, "shape": shape, "etag": newETag,
		})
	}
	fmt.Printf("patched shape on %s (etag %s)\n", pid, shortRev(newETag))
	return nil
}

// derefOr returns *p, or fallback when p is nil.
func derefOr(p *float64, fallback float64) float64 {
	if p == nil {
		return fallback
	}
	return *p
}

// doClearPattern disables any running pattern by sending an empty step list.
// Server-side that's the kernel signal to fall back to the slider rate
// (kept intact, distinct from `--clear` which wipes everything).
func doClearPattern(client *api.Client, ctx context.Context, pid string, asJSON bool) error {
	t := proxy.Sliders
	shape := proxy.Shape{
		Pattern: &proxy.Pattern{
			Template: &t,
			Steps:    []proxy.PatternStep{},
		},
	}
	newETag, err := client.PatchShape(ctx, pid, "shape clear-pattern", &shape)
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{
			"player_id": pid, "pattern_cleared": true, "etag": newETag,
		})
	}
	fmt.Printf("cleared pattern on %s (slider rate preserved; etag %s)\n", pid, shortRev(newETag))
	return nil
}

// doPattern generates step rates from the player's current manifest
// variants via the shared go-proxy/pkg/ladder builder — the same one the
// characterization harness uses and the dashboard's NetworkShapingPattern
// panel mirrors in JS. The ladder carries both a peak and an average
// anchor per variant, +marginPct flat, with geometric fills to maxStep
// (#551). Snapshots pre-state for `harness undo` and PATCHes.
func doPattern(client *api.Client, ctx context.Context, pid string, asJSON bool,
	tplStr string, stepSecs, marginPct int, maxStep, topHeadroomPct float64, broadcast *bool) error {

	tpl, err := parseTemplate(tplStr)
	if err != nil {
		return err
	}
	stepSecsEnum, err := parseStepSeconds(stepSecs)
	if err != nil {
		return err
	}
	marginEnum, err := parseMarginPct(marginPct)
	if err != nil {
		return err
	}

	// Pull the player's variant list to size the step rates. The v2
	// projection on /api/v2/players doesn't always carry
	// current_play.manifest.variants (pre-existing v2-translate gap).
	// Mirror the dashboard's useManifestVariants composable: try
	// the typed v2 path first, fall back to /api/sessions which
	// always carries the manifest_variants slice.
	rec, _, err := client.Player(ctx, pid)
	if err != nil {
		return err
	}
	rungs, err := variantLadder(rec, float64(marginPct), maxStep, topHeadroomPct)
	if err != nil {
		return err
	}

	steps := buildPatternSteps(tpl, rungs, stepSecs)
	if len(steps) == 0 && tpl != proxy.Sliders {
		return fmt.Errorf("template %q produced an empty step list — does the player have a manifest yet?", tplStr)
	}

	shape := proxy.Shape{
		Pattern: &proxy.Pattern{
			Template:           &tpl,
			Steps:              steps,
			DefaultStepSeconds: &stepSecsEnum,
			MarginPct:          &marginEnum,
		},
	}
	action := fmt.Sprintf("shape pattern=%s steps=%d step_s=%d margin=%d%%",
		tplStr, len(steps), stepSecs, marginPct)
	newETag, err := client.PatchShapeBroadcast(ctx, pid, action, &shape, broadcast)
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{
			"player_id": pid, "pattern": shape.Pattern, "etag": newETag,
		})
	}
	fmt.Printf("applied %s pattern to %s — %d steps × %ds = %ds total cycle (etag %s)\n",
		tplStr, pid, len(steps), stepSecs, len(steps)*stepSecs, shortRev(newETag))
	return nil
}

// parseBroadcast turns the tri-state --broadcast flag into a *bool:
// "" → nil (server default), true|1 → &true, false|0 → &false. Threaded into
// the PATCH as the ?broadcast= query param (see client.PatchShapeBroadcast).
func parseBroadcast(s string) (*bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return nil, nil
	case "true", "1":
		v := true
		return &v, nil
	case "false", "0":
		v := false
		return &v, nil
	}
	return nil, fmt.Errorf("invalid --broadcast %q: true|false", s)
}

func parseTemplate(s string) (proxy.PatternTemplate, error) {
	switch s {
	case "pyramid":
		return proxy.Pyramid, nil
	case "valley":
		return proxy.Valley, nil
	case "ramp_up":
		return proxy.RampUp, nil
	case "ramp_down":
		return proxy.RampDown, nil
	case "square_wave":
		return proxy.SquareWave, nil
	case "transient_shock":
		return proxy.TransientShock, nil
	case "sliders":
		return proxy.Sliders, nil
	}
	return "", fmt.Errorf("invalid --pattern %q: pyramid|valley|ramp_up|ramp_down|square_wave|transient_shock|sliders", s)
}

func parseStepSeconds(n int) (proxy.PatternDefaultStepSeconds, error) {
	switch n {
	case 6, 12, 18, 24, 60, 120:
		return proxy.PatternDefaultStepSeconds(n), nil
	}
	return 0, fmt.Errorf("invalid --step-seconds %d: must be 6|12|18|24|60|120", n)
}

func parseMarginPct(n int) (proxy.PatternMarginPct, error) {
	switch n {
	case 0, 5, 10, 25, 50:
		return proxy.PatternMarginPct(n), nil
	}
	return 0, fmt.Errorf("invalid --margin %d: must be 0|5|10|25|50", n)
}

// variantLadder pulls the player's manifest variants and builds the
// shared dual-rung (avg+peak) + geometrically-filled limit ladder via
// go-proxy/pkg/ladder, descending by cap. bumpPct is the flat headroom
// (the operator --margin); maxStep is the fill density. Returns an error
// when the player has no variants yet (master playlist not fetched).
func variantLadder(rec *proxy.PlayerRecord, bumpPct, maxStep, topHeadroomPct float64) ([]ladder.Rung, error) {
	if rec == nil || rec.CurrentPlay == nil || rec.CurrentPlay.Manifest == nil || rec.CurrentPlay.Manifest.Variants == nil {
		return nil, errors.New("player has no manifest variants yet — has it fetched the master playlist?")
	}
	variants := *rec.CurrentPlay.Manifest.Variants
	if len(variants) == 0 {
		return nil, errors.New("player has no manifest variants yet — has it fetched the master playlist?")
	}
	lv := make([]ladder.Variant, 0, len(variants))
	for _, v := range variants {
		avg := 0
		if v.AverageBandwidth != nil {
			avg = *v.AverageBandwidth
		}
		lv = append(lv, ladder.Variant{AvgBps: avg, PeakBps: v.Bandwidth, Resolution: v.Resolution})
	}
	rungs := ladder.StandardLadder(lv, bumpPct, maxStep, topHeadroomPct)
	if len(rungs) == 0 {
		return nil, errors.New("manifest_variants present but all bandwidths zero")
	}
	return rungs, nil
}

// buildPatternSteps orders the limit ladder into a pattern step list via
// the shared ladder.BuildPattern (the same logic the dashboard's
// NetworkShapingPattern.vue mirrors in JS, parity-checked against
// go-proxy/pkg/ladder's golden vectors).
func buildPatternSteps(t proxy.PatternTemplate, rungs []ladder.Rung, stepSecs int) []proxy.PatternStep {
	lsteps := ladder.BuildPattern(string(t), rungs, stepSecs)
	enabled := true
	out := make([]proxy.PatternStep, 0, len(lsteps))
	for _, s := range lsteps {
		e := enabled
		out = append(out, proxy.PatternStep{
			RateMbps:        float32(s.RateMbps),
			DurationSeconds: s.DurationSeconds,
			Enabled:         &e,
		})
	}
	return out
}
