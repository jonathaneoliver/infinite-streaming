package sweep

import (
	"fmt"
	"strings"
)

// Slug makes a value safe to use as a forwarder label value. The forwarder
// silently DROPS any label value containing `,` or `=` (the LabelPlay
// encoding), so we must never pass raw text — we encode those (and spaces) to
// `_` instead of losing the value with no error (§4.1). Idempotent.
func Slug(v string) string {
	r := strings.NewReplacer(",", "_", "=", "_", " ", "_")
	return r.Replace(v)
}

// RunLabels builds the `testing=`-tier metadata for one run — the WHAT (recipe
// axes + provenance) plus the WHY slug (§4.1). The harness stamps these via
// `harness labels set <target> k=v …`; the forwarder prefixes them `testing=`
// so they're metadata, not a good/bad signal. All values are Slug-encoded.
func RunLabels(e *Experiment) map[string]string {
	m := map[string]string{
		"sweep":    "1",
		"exp_id":   Slug(e.ID),
		"class":    string(e.ClassOrDefault()),
		"kind":     string(e.Kind),
		"platform": e.Platform,
		"protocol": e.Protocol,
		"mode":     e.Mode,
		"recipe":   recipeSlug(e),
	}
	if e.Fault != nil {
		m["fault"] = faultSlug(e.Fault)
	}
	if e.Arm != "" {
		m["arm"] = string(e.Arm)
	}
	if e.Group != "" {
		m["group"] = Slug(e.Group)
	}
	if e.Parent != "" {
		m["parent"] = Slug(e.Parent)
	}
	if e.Depth > 0 {
		m["depth"] = fmt.Sprintf("%d", e.Depth)
	}
	if e.RepGroup != "" {
		m["rep_group"] = Slug(e.RepGroup)
	}
	if e.Why != "" {
		m["why"] = Slug(e.Why)
	}
	if e.Result != nil && e.Result.Verdict != "" {
		m["verdict"] = string(e.Result.Verdict)
	}
	return m
}

// faultSlug is the fault descriptor for the `fault=` label: "none" when there
// is no HTTP fault, else "<type>_<request_kind>" (e.g. "500_segment").
func faultSlug(f *Fault) string {
	if f == nil {
		return "none"
	}
	if f.RequestKind != "" {
		return Slug(f.Type + "_" + f.RequestKind)
	}
	return Slug(f.Type)
}

// RecipeSlug is the exported form of recipeSlug for callers outside the
// package (e.g. `harness sweep publish` building the dashboard row).
func RecipeSlug(e *Experiment) string { return recipeSlug(e) }

// recipeSlug is the single most-distinguishing knob of an experiment — the
// dedup-relevant "what was applied". For a fault-class experiment that's the
// fault family; for a config-class experiment it's the dominant config knob
// (content manipulation > pattern > transfer-timeout > baseline). Used in the
// `recipe=` label and the finding signature.
func recipeSlug(e *Experiment) string {
	if e.Fault != nil {
		return faultSlug(e.Fault)
	}
	if cm := e.ContentManipulation; cm != nil {
		switch {
		case cm.StripAvgBandwidth:
			return "strip_avgbw"
		case cm.StripCodecs:
			return "strip_codecs"
		case cm.StripResolution:
			return "strip_resolution"
		case cm.AllowedVariants != "":
			return Slug("ladder_" + cm.AllowedVariants)
		case cm.VariantOrder != "":
			return Slug("variant_order_" + cm.VariantOrder)
		case cm.LiveOffset != nil:
			return Slug(fmt.Sprintf("live_offset_%g", *cm.LiveOffset))
		case cm.OverstateBandwidth != nil:
			return "overstate_bw"
		}
	}
	if e.Shape != nil && e.Shape.Pattern != "" {
		return Slug("pattern_" + e.Shape.Pattern)
	}
	if e.TransferTimeouts != nil {
		return "xfer_timeout"
	}
	return "baseline"
}

// Signature is the dedup key for a finding's GitHub Issue (§4):
// `sig:<class>-<protocol>-<recipe>-<aberrationKind>[-<attributedAxis>]`. The
// class namespaces config findings from fault findings so they never collide;
// recipe is the dominant knob; axis is appended once isolation attributes a
// cause.
func Signature(e *Experiment, aberrationKind, attributedAxis string) string {
	parts := []string{"sig:" + string(e.ClassOrDefault()), e.Protocol, recipeSlug(e), Slug(aberrationKind)}
	if attributedAxis != "" {
		parts = append(parts, Slug(attributedAxis))
	}
	return strings.Join(parts, "-")
}
