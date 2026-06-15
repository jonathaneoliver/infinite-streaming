package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/sweep"
)

// cmdSweepSeedFromTriage closes the loop (#783): it reads the observational
// triage signal (label divergence — skew + the dominant dimension per label)
// and generates sweep experiments that reproduce the dimension a severe,
// dimension-driven label is concentrated on. Only CONTROLLABLE dimensions
// become experiments (test/pattern, platform, content); observational ones
// (app_version, device_model, device_kind) and not-yet-runnable targets are
// skipped with a logged reason. Dedups by deterministic id.
//
//	harness sweep seed-from-triage [--days N] [--top N] [--min-skew X] [--dry-run]

// runnablePlatforms are the platforms the probe can actually drive today.
// androidtv via adb/cli (a physical Android TV on the runner host); the rest
// via appium against a booted sim / attached iPhone.
var runnablePlatforms = map[string]bool{"ipad-sim": true, "iphone-sim": true, "iphone": true, "androidtv": true}

// testRecipe maps a characterization `test` value to a sweep mode + (for the
// pattern tests) the shape pattern the probe arms.
func testRecipe(test string) (mode, pattern string) {
	switch test {
	case "pyramid":
		return "pyramid", "pyramid"
	case "transient_shock":
		return "transient_shock", "transient_shock"
	case "rampup":
		return "rampup", "ramp_up"
	case "rampdown":
		return "rampdown", "ramp_down"
	default:
		return test, "" // non-pattern test: probe plays plain (limited fidelity)
	}
}

// labelEvent strips the "<severity>=" prefix and any leading * marker.
func labelEvent(label string) string {
	ev := label
	if i := strings.IndexByte(ev, '='); i >= 0 {
		ev = ev[i+1:]
	}
	return strings.TrimPrefix(ev, "*")
}

func labelSeverity(label string) string {
	if i := strings.IndexByte(label, '='); i >= 0 {
		return label[:i]
	}
	return ""
}

func cmdSweepSeedFromTriage(client *api.Client, args []string, asJSON bool) error {
	fs := flag.NewFlagSet("sweep seed-from-triage", flag.ContinueOnError)
	days := fs.Int("days", 7, "lookback window for the triage signal")
	top := fs.Int("top", 8, "consider the top-N labels by skew")
	minSkew := fs.Float64("min-skew", 2.0, "only seed labels whose skew (max lift) ≥ this")
	dryRun := fs.Bool("dry-run", false, "print the plan; don't write experiments")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := openStore(client)
	if err != nil {
		return err
	}

	// Pull the divergence signal: per label, its skew + dominant "dim=value".
	url := fmt.Sprintf("%s/analytics/api/v2/label_divergence?days=%d&exclude_faulted=1",
		strings.TrimRight(client.BaseURL, "/"), *days)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if client.BasicAuth != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(client.BasicAuth)))
	}
	resp, err := client.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("fetch divergence: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("divergence: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var div struct {
		Items []struct {
			Label string  `json:"label"`
			Skew  float64 `json:"skew"`
			Top   string  `json:"top"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &div); err != nil {
		return fmt.Errorf("parse divergence: %w", err)
	}

	// Keep severe, dimension-driven labels; rank by skew; take the top N.
	type cand struct {
		label, dim, value string
		skew              float64
	}
	var cands []cand
	for _, it := range div.Items {
		sev := labelSeverity(it.Label)
		if sev != "warning" && sev != "critical" && sev != "error" {
			continue
		}
		if it.Skew < *minSkew || it.Top == "" {
			continue
		}
		dim, value := it.Top, ""
		if i := strings.IndexByte(it.Top, '='); i >= 0 {
			dim, value = it.Top[:i], it.Top[i+1:]
		}
		cands = append(cands, cand{it.Label, dim, value, it.Skew})
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].skew > cands[j].skew })
	if len(cands) > *top {
		cands = cands[:*top]
	}

	now := nowUTC()
	type plan struct {
		Label, Action, ID, Reason string
	}
	var plans []plan

	// Several severe labels often concentrate on the SAME controllable
	// dimension value (e.g. four labels all 15–18× on iphone-sim). They want
	// ONE experiment, not four — so fold by deterministic id, recording every
	// contributing label on the one experiment's rationale.
	type folded struct {
		exp     *sweep.Experiment
		labels  []string
		topSkew float64
		topWhy  string
	}
	byID := map[string]*folded{}
	var order []string
	for _, c := range cands {
		e, skip := experimentFromTriage(c.label, c.dim, c.value, c.skew, now)
		if e == nil {
			plans = append(plans, plan{c.label, "skip", "", skip})
			continue
		}
		f, ok := byID[e.ID]
		if !ok {
			f = &folded{exp: e}
			byID[e.ID] = f
			order = append(order, e.ID)
		}
		f.labels = append(f.labels, c.label)
		if c.skew > f.topSkew {
			f.topSkew, f.topWhy = c.skew, fmt.Sprintf("%.1f× on %s=%s", c.skew, c.dim, c.value)
			f.exp.Why = "triage_" + sweep.Slug(labelEvent(c.label))
		}
	}

	seeded := 0
	for _, id := range order {
		f := byID[id]
		e := f.exp
		e.WhyText = fmt.Sprintf("seeded from triage: %s concentrate on %s (top %s)",
			strings.Join(f.labels, ", "), strings.SplitN(f.topWhy, " on ", 2)[1], f.topWhy)
		// Dedup against anything already queued for this target.
		if _, err := s.Load(sweep.StatusBacklog, e.ID); err == nil {
			plans = append(plans, plan{strings.Join(f.labels, ", "), "skip", e.ID, "already in backlog"})
			continue
		}
		e.Score = sweep.DefaultWeights().Score(e)
		if !*dryRun {
			if err := s.Save(sweep.StatusBacklog, e); err != nil {
				return err
			}
		}
		seeded++
		plans = append(plans, plan{strings.Join(f.labels, ", "), "seed", e.ID, f.topWhy})
	}

	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"seeded": seeded, "dry_run": *dryRun, "plans": plans})
	}
	verb := "seeded"
	if *dryRun {
		verb = "would seed"
	}
	fmt.Printf("%s %d experiment(s) from triage (top %d labels, skew ≥ %.1f):\n", verb, seeded, *top, *minSkew)
	for _, p := range plans {
		if p.Action == "seed" {
			fmt.Printf("  + %-40s %s  (%s)\n", p.ID, p.Label, p.Reason)
		} else {
			fmt.Printf("  – skip %-34s %s  (%s)\n", p.Label, "", p.Reason)
		}
	}
	return nil
}

// experimentFromTriage turns one triage candidate into a sweep experiment, or
// returns (nil, reason) when the dominant dimension isn't controllable/runnable.
func experimentFromTriage(label, dim, value string, skew float64, now string) (*sweep.Experiment, string) {
	whySlug := "triage_" + sweep.Slug(labelEvent(label))
	whyText := fmt.Sprintf("seeded from triage: %s is %.1f× on %s=%s", label, skew, dim, value)

	switch dim {
	case "test":
		mode, pattern := testRecipe(value)
		e := &sweep.Experiment{
			ID: "triage-test-" + sweep.Slug(value), CreatedAt: now, Class: sweep.ClassConfig,
			Platform: "ipad-sim", LaunchMode: sweep.LaunchModeAppium, Protocol: "hls", Content: sweep.SeedContent, Mode: mode,
			Kind: sweep.KindSeed, Reps: 1, Why: whySlug, WhyText: whyText,
		}
		if pattern != "" {
			e.Shape = &sweep.Shape{Pattern: pattern, StepSeconds: 12, MarginPct: 5}
		}
		return e, ""
	case "platform":
		if !runnablePlatforms[value] {
			return nil, "platform " + value + " not runnable by the probe yet (e.g. Android TV) — expand coverage first"
		}
		return &sweep.Experiment{
			ID: "triage-platform-" + sweep.Slug(value), CreatedAt: now, Class: sweep.ClassConfig,
			Platform: value, LaunchMode: sweep.LaunchModeAppium, Protocol: "hls", Content: sweep.SeedContent, Mode: "steps",
			Kind: sweep.KindSeed, Reps: 1, Why: whySlug, WhyText: whyText,
		}, ""
	case "content":
		if value != sweep.SeedContent {
			return nil, "content " + value + " out of scope (single-content sweep) — add it to expand"
		}
		return nil, "content matches the current clip — no new test"
	default:
		return nil, dim + " is observational (app/device) — can't reproduce it with an experiment, only filter"
	}
}
