package sweep

import "fmt"

// A seedRecipe is one starter experiment shape: a mode (the playback motion)
// plus the class-appropriate overlay. config-class recipes set content/shape/
// transfer knobs; fault-class recipes set a Fault. Content is fixed to
// insane_new for now (§9). The characterization mode drives the bandwidth
// motion itself (pyramid shapes a pyramid, etc.).
type seedRecipe struct {
	family    string // recipe-family slug, for the id
	mode      string
	segment   string // master variant the probe requests (s2|s6|ll); "" = app default (s6)
	groupSlug string // non-empty → arms sharing it form a per-platform comparison group
	whyText   string // optional rationale override (else a generic starter-seed line)
	cm        *ContentManipulation
	shape     *Shape
	transfer  *TransferTimeouts
	fault     *Fault
}

func floatPtr(v float64) *float64 { return &v }

// configRecipes — the realistic stream-config + benign-network set (§5, §10).
// No injected errors, no delay/loss, no sub-floor caps: patterns are
// ladder-derived (sustainable by construction) and the content knobs are
// legitimate manifest variations. This is the default depth-first set, built to
// surface ABR decision-quality issues (the over-downshift class) and
// manifest-config robustness.
var configRecipes = []seedRecipe{
	{family: "abr-pyramid", mode: "pyramid", shape: &Shape{Pattern: "pyramid", StepSeconds: 30, MarginPct: 5}},
	{family: "downshift", mode: "downshift_severity"}, // where over-downshift `notable` surfaces
	{family: "strip-avgbw", mode: "steps", cm: &ContentManipulation{StripAvgBandwidth: true}},
	{family: "sparse-ladder", mode: "steps", cm: &ContentManipulation{AllowedVariants: "drop-top-rung"}},
	{family: "xfer-timeout", mode: "steps", transfer: &TransferTimeouts{ActiveSeconds: 10, AppliesSegments: true}},
}

// liveOffsetRecipes — the segment × live-offset matrix (#793). live_offset is a
// load-time/session property (read at manifest join), so each value is its OWN
// little job (one clean IV per run, which is what the manipulation-check gate
// needs); the arms share a per-platform group so the dashboard + an LLM
// synthesis compare them as a set. The hold-back FLOOR is 3× the MAX segment
// duration, and "6s" segments can round up to 7s → floor ~21s (not 18s); "2s"
// → ~6-9s. So the same offset is sub-spec on s6 but legal on s2 — the headline
// comparison. The gate marks a run inconclusive when the achieved offset
// doesn't reach the intended value (segment-slack-aware), so arms expected NOT
// to land are real "is this even testable here" probes, not false findings.
var liveOffsetRecipes = []seedRecipe{
	{family: "live-offset-s6-cross12", mode: "startup", segment: "s6", groupSlug: "live-offset",
		cm:      &ContentManipulation{LiveOffset: floatPtr(12)},
		whyText: "live-offset 12s on 6s segments — below the ~21s floor (3×7) → sub-spec; expect inconclusive (IV clamped to the floor, won't land). The out-of-spec half of the segment×offset comparison (pairs with s2-cross12)."},
	{family: "live-offset-s2-cross12", mode: "startup", segment: "s2", groupSlug: "live-offset",
		cm:      &ContentManipulation{LiveOffset: floatPtr(12)},
		whyText: "live-offset 12s on 2s segments — above the ~6-9s floor → legal; expect it to land. The in-spec half of the segment×offset comparison (pairs with s6-cross12): same offset, opposite outcome by segment size."},
	{family: "live-offset-s6-deep36", mode: "startup", segment: "s6", groupSlug: "live-offset",
		cm:      &ContentManipulation{LiveOffset: floatPtr(36)},
		whyText: "live-offset 36s on 6s segments — well above the ~21s floor and far from the ~21s default → legal; expect it to land (shows s6 CAN honour a deliberately-moved offset, so a non-landing elsewhere is meaningful)."},
	{family: "live-offset-s2-sub2", mode: "startup", segment: "s2", groupSlug: "live-offset",
		cm:      &ContentManipulation{LiveOffset: floatPtr(2)},
		whyText: "live-offset 2s on 2s segments — below the ~6-9s floor → sub-spec; expect inconclusive (clamped to the floor)."},
}

// faultRecipes — the explicit-error recovery set (separate class). Each injects
// one HTTP/connection fault the player is expected to recover from; the oracle
// judges against the recovery-expected envelope (a fault within envelope is not
// an aberration). Seeded + run only when `--class fault` is selected.
var faultRecipes = []seedRecipe{
	{family: "seg5xx", mode: "steps", fault: &Fault{Type: "500", RequestKind: "segment", Frequency: 1, Mode: "requests"}},
	{family: "manifest-timeout", mode: "startup", fault: &Fault{Type: "timeout", RequestKind: "manifest", Frequency: 1, Mode: "requests"}},
	{family: "seg-corrupt", mode: "steps", fault: &Fault{Type: "corrupted", RequestKind: "segment", Frequency: 1, Mode: "requests"}},
}

// narrowPlatforms / fullPlatforms — the active sweep is **iOS-sim only** for
// now; --full is reserved for when the other platforms come online.
var (
	narrowPlatforms = []string{"ipad-sim"}
	fullPlatforms   = []string{"ipad-sim", "iphone", "appletv", "androidtv"}
	// Protocol is **HLS only** for now — the probe plays the app's default
	// (HLS) and protocol selection isn't wired in yet. DASH returns when it is.
	seedProtocols = []string{"hls"}
)

// SeedContent is the single content item the sweep runs against for now: the
// H264 build of the "insane new" clip (the catalogue `name`).
const SeedContent = "insane_new_p200_h264"

// recipesFor returns the recipe set for a class (config is the default). The
// config set includes the live-offset matrix (#793).
func recipesFor(class Class) []seedRecipe {
	if class == ClassFault {
		return faultRecipes
	}
	out := append([]seedRecipe{}, configRecipes...)
	return append(out, liveOffsetRecipes...)
}

// Seed builds the starter backlog for one class. full=false is the narrow
// depth-first set (iPad-sim only); full=true widens across the physical-device
// platforms. `now` is the RFC3339 UTC stamp for created_at (passed in so
// callers control the clock). Scores are stamped with the default weights.
// Seed builds the starter experiment set. platformsOverride (optional) targets a
// specific platform (e.g. "androidtv") instead of the narrow/full defaults.
func Seed(class Class, full bool, now string, platformsOverride ...string) []*Experiment {
	if class == "" {
		class = ClassConfig
	}
	platforms := narrowPlatforms
	if full {
		platforms = fullPlatforms
	}
	if len(platformsOverride) > 0 {
		platforms = platformsOverride
	}
	recipes := recipesFor(class)
	w := DefaultWeights()
	var out []*Experiment
	for _, p := range platforms {
		for _, proto := range seedProtocols {
			for _, r := range recipes {
				e := &Experiment{
					ID:                  fmt.Sprintf("seed-%s-%s-%s-%s-%s", class, p, proto, r.family, r.mode),
					CreatedAt:           now,
					Class:               class,
					Platform:            p,
					LaunchMode:          LaunchModeAppium, // every item is driven by appium (the only mode the probe supports), incl. the physical Android TV
					Protocol:            proto,
					Content:             SeedContent,
					Segment:             r.segment,
					Mode:                r.mode,
					ContentManipulation: cloneCM(r.cm),
					Shape:               cloneShape(r.shape),
					TransferTimeouts:    cloneTransfer(r.transfer),
					Fault:               cloneFault(r.fault),
					Kind:                KindSeed,
					Group:               groupID(r.groupSlug, p),
					Reps:                1,
					Depth:               0,
					Why:                 "starter_seed",
					WhyText:             seedWhyText(r, class, p, proto),
				}
				e.Score = w.Score(e)
				out = append(out, e)
			}
		}
	}
	return out
}

// groupID expands a recipe's group slug into a per-platform comparison group
// id (so arms on the same device compare against each other). Empty slug → no
// group (the standalone-seed default).
func groupID(slug, platform string) string {
	if slug == "" {
		return ""
	}
	return fmt.Sprintf("grp-%s-%s", slug, platform)
}

// seedWhyText prefers a recipe's explicit rationale; otherwise a generic
// starter-seed line. Provenance is never blank.
func seedWhyText(r seedRecipe, class Class, platform, proto string) string {
	if r.whyText != "" {
		return r.whyText
	}
	return fmt.Sprintf("starter %s-class seed: %s recipe (%s) on %s/%s", class, r.family, r.mode, platform, proto)
}

func cloneFault(f *Fault) *Fault {
	if f == nil {
		return nil
	}
	c := *f
	return &c
}

func cloneShape(s *Shape) *Shape {
	if s == nil {
		return nil
	}
	c := *s
	if s.RateMbps != nil {
		c.RateMbps = floatPtr(*s.RateMbps)
	}
	return &c
}

func cloneCM(cm *ContentManipulation) *ContentManipulation {
	if cm == nil {
		return nil
	}
	c := *cm
	if cm.LiveOffset != nil {
		c.LiveOffset = floatPtr(*cm.LiveOffset)
	}
	if cm.OverstateBandwidth != nil {
		c.OverstateBandwidth = floatPtr(*cm.OverstateBandwidth)
	}
	return &c
}

func cloneTransfer(t *TransferTimeouts) *TransferTimeouts {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}
