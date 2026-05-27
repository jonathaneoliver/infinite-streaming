package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/format"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/proxy"
)

const shapeUsage = `harness shape <target> [flags]

Slider mode (any subset; omitted fields are not modified):
  --rate FLOAT       rate cap in Mbps (e.g. 1.5)
  --delay FLOAT      one-way delay in ms (e.g. 200)
  --loss FLOAT       packet loss %% (e.g. 0.5, range 0–100)

Pattern mode (generates a step list from the player's current variants):
  --pattern NAME     pyramid | ramp_up | ramp_down | square_wave | sliders
  --step-seconds N   per-step duration: 6 | 12 | 18 | 24 (default 12)
  --margin PCT       headroom above top variant: 0 | 10 | 25 | 50 (default 0)
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
  pyramid       ascending variant rates, then descending (without apex dupe)
  ramp_up       ascending rates, single sweep
  ramp_down     descending rates, single sweep
  square_wave   alternate lowest + highest variant
  sliders       empty step list (kernel falls back to --rate)

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
	pattern := fs.String("pattern", "", "pattern template (pyramid|ramp_up|ramp_down|square_wave|sliders)")
	stepSeconds := fs.Int("step-seconds", 12, "per-step duration: 6|12|18|24")
	margin := fs.Int("margin", 0, "headroom %% above top variant: 0|10|25|50")
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
		return doPattern(client, ctx, pid, asJSON, *pattern, *stepSeconds, *margin)
	}

	if *rate < 0 && *delay < 0 && *loss < 0 {
		return errors.New("nothing to do — pass --rate/--delay/--loss, --pattern NAME, --clear-pattern, --clear, or --show")
	}
	return doSliderShape(client, ctx, pid, asJSON, *rate, *delay, *loss)
}

func showShape(client *api.Client, ctx context.Context, pid string, asJSON bool) error {
	rec, etag, err := client.Player(ctx, pid)
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{
			"player_id": pid, "shape": rec.Shape, "etag": etag,
		})
	}
	if rec.Shape == nil {
		fmt.Printf("%s: no shaping\n", pid)
		return nil
	}
	return format.JSON(os.Stdout, rec.Shape)
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

func doSliderShape(client *api.Client, ctx context.Context, pid string, asJSON bool, rate, delay, loss float64) error {
	shape := proxy.Shape{}
	if rate >= 0 {
		v := float32(rate)
		shape.RateMbps = &v
	}
	if delay >= 0 {
		v := float32(delay)
		shape.DelayMs = &v
	}
	if loss >= 0 {
		v := float32(loss)
		shape.LossPct = &v
	}
	action := fmt.Sprintf("shape rate=%v delay=%v loss=%v", rate, delay, loss)
	newETag, err := client.PatchShape(ctx, pid, action, &shape)
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
// variants using the same algorithm the dashboard's NetworkShapingPattern
// panel runs (see buildSteps in that .vue). Snapshots pre-state for
// `harness undo` and PATCHes.
func doPattern(client *api.Client, ctx context.Context, pid string, asJSON bool,
	tplStr string, stepSecs, marginPct int) error {

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
	rates, err := variantRatesMbps(rec, marginPct)
	if err != nil {
		// Typed v2 path empty — fall back to legacy /api/sessions.
		fallback, ferr := variantRatesFromLegacySessions(client, ctx, pid, marginPct)
		if ferr != nil {
			return fmt.Errorf("%v; legacy fallback also failed: %v", err, ferr)
		}
		rates = fallback
	}

	steps := buildPatternSteps(tpl, rates, stepSecs)
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
	newETag, err := client.PatchShape(ctx, pid, action, &shape)
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

func parseTemplate(s string) (proxy.PatternTemplate, error) {
	switch s {
	case "pyramid":
		return proxy.Pyramid, nil
	case "ramp_up":
		return proxy.RampUp, nil
	case "ramp_down":
		return proxy.RampDown, nil
	case "square_wave":
		return proxy.SquareWave, nil
	case "sliders":
		return proxy.Sliders, nil
	}
	return "", fmt.Errorf("invalid --pattern %q: pyramid|ramp_up|ramp_down|square_wave|sliders", s)
}

func parseStepSeconds(n int) (proxy.PatternDefaultStepSeconds, error) {
	switch n {
	case 6, 12, 18, 24:
		return proxy.PatternDefaultStepSeconds(n), nil
	}
	return 0, fmt.Errorf("invalid --step-seconds %d: must be 6|12|18|24", n)
}

func parseMarginPct(n int) (proxy.PatternMarginPct, error) {
	switch n {
	case 0, 10, 25, 50:
		return proxy.PatternMarginPct(n), nil
	}
	return 0, fmt.Errorf("invalid --margin %d: must be 0|10|25|50", n)
}

// variantRatesMbps pulls the player's manifest variants, applies the
// margin %, and returns the sorted-ascending Mbps list the buildSteps
// algorithm consumes. Returns an error when the player has no variants
// yet (master playlist not fetched).
func variantRatesMbps(rec *proxy.PlayerRecord, marginPct int) ([]float32, error) {
	if rec == nil || rec.CurrentPlay == nil || rec.CurrentPlay.Manifest == nil || rec.CurrentPlay.Manifest.Variants == nil {
		return nil, errors.New("player has no manifest variants yet — has it fetched the master playlist?")
	}
	variants := *rec.CurrentPlay.Manifest.Variants
	if len(variants) == 0 {
		return nil, errors.New("player has no manifest variants yet — has it fetched the master playlist?")
	}
	rates := make([]float32, 0, len(variants))
	for _, v := range variants {
		// Same shape as dashboard's buildSteps: bps × (1 + margin) / 1000
		// rounded to 3 dp.
		mbps := float32(v.Bandwidth) * (1 + float32(marginPct)/100) / 1_000_000
		if mbps > 0 {
			rates = append(rates, roundFloat32(mbps, 3))
		}
	}
	if len(rates) == 0 {
		return nil, errors.New("manifest_variants present but all bandwidths zero")
	}
	// Sort ascending for the build algorithm.
	for i := 1; i < len(rates); i++ {
		for j := i; j > 0 && rates[j-1] > rates[j]; j-- {
			rates[j-1], rates[j] = rates[j], rates[j-1]
		}
	}
	return rates, nil
}

// variantRatesFromLegacySessions queries the v1 `/api/sessions` array
// directly and pulls `manifest_variants` for the matching player.
// Bypasses the v2 projection's gap — the legacy endpoint always
// carries the raw session map, which has `manifest_variants` whenever
// the master playlist has been fetched.
func variantRatesFromLegacySessions(client *api.Client, ctx context.Context, pid string, marginPct int) ([]float32, error) {
	body, err := client.Raw(ctx, "GET", "/api/sessions", nil)
	if err != nil {
		return nil, fmt.Errorf("legacy /api/sessions fetch failed: %w", err)
	}
	// /api/sessions returns a JSON array of session maps. Find the
	// entry whose `headers_player-ID` (case-sensitive, iOS-uppercase)
	// or `player_id` matches our canonical lowercase pid.
	var sessions []map[string]any
	if err := jsonDecodeSlice(body, &sessions); err != nil {
		return nil, fmt.Errorf("parse /api/sessions: %w", err)
	}
	want := strings.ToLower(pid)
	for _, s := range sessions {
		for _, key := range []string{"player_id", "headers_player-ID", "headers_player_id", "headers_x_playback_session_id"} {
			v, _ := s[key].(string)
			if v != "" && strings.ToLower(v) == want {
				return manifestVariantsFromMap(s, marginPct)
			}
		}
	}
	return nil, fmt.Errorf("no /api/sessions entry matches player %s", pid)
}

// manifestVariantsFromMap pulls `manifest_variants` out of a session
// map and applies the margin %, returning the sorted rate list. Same
// shape as variantRatesMbps but operating on a v1-style raw map.
func manifestVariantsFromMap(s map[string]any, marginPct int) ([]float32, error) {
	raw, ok := s["manifest_variants"]
	if !ok || raw == nil {
		return nil, errors.New("manifest_variants missing on session")
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("manifest_variants is %T, want []any", raw)
	}
	rates := make([]float32, 0, len(arr))
	for _, e := range arr {
		obj, ok := e.(map[string]any)
		if !ok {
			continue
		}
		bw, _ := obj["bandwidth"].(float64)
		if bw <= 0 {
			continue
		}
		mbps := float32(bw) * (1 + float32(marginPct)/100) / 1_000_000
		rates = append(rates, roundFloat32(mbps, 3))
	}
	if len(rates) == 0 {
		return nil, errors.New("manifest_variants present but all bandwidths zero or non-numeric")
	}
	for i := 1; i < len(rates); i++ {
		for j := i; j > 0 && rates[j-1] > rates[j]; j-- {
			rates[j-1], rates[j] = rates[j], rates[j-1]
		}
	}
	return rates, nil
}

// buildPatternSteps mirrors the dashboard's NetworkShapingPattern.vue
// `buildSteps` function. Keep in sync — operator workflows expect a
// CLI-applied pattern to look identical to a UI-applied one.
func buildPatternSteps(t proxy.PatternTemplate, rates []float32, stepSecs int) []proxy.PatternStep {
	var seq []float32
	switch t {
	case proxy.SquareWave:
		seq = []float32{rates[0], rates[len(rates)-1]}
	case proxy.RampUp:
		seq = append([]float32(nil), rates...)
	case proxy.RampDown:
		seq = append([]float32(nil), rates...)
		reverseFloat32(seq)
	case proxy.Pyramid:
		asc := append([]float32(nil), rates...)
		desc := append([]float32(nil), rates[:len(rates)-1]...)
		reverseFloat32(desc)
		seq = append(asc, desc...)
	default:
		// sliders / square / unknown — empty step list
		return nil
	}

	enabled := true
	out := make([]proxy.PatternStep, 0, len(seq))
	for _, r := range seq {
		e := enabled
		out = append(out, proxy.PatternStep{
			RateMbps:        r,
			DurationSeconds: stepSecs,
			Enabled:         &e,
		})
	}
	return out
}

func reverseFloat32(a []float32) {
	for i, j := 0, len(a)-1; i < j; i, j = i+1, j-1 {
		a[i], a[j] = a[j], a[i]
	}
}

func roundFloat32(v float32, dp int) float32 {
	m := float32(1)
	for i := 0; i < dp; i++ {
		m *= 10
	}
	return float32(int(v*m+0.5)) / m
}

