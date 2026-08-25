package sweep

import (
	"fmt"
	"strings"
)

// Promotion turns a confirmed hit into the durable record: a deduped GitHub
// Issue (§4). The dedup key is the Signature
// (sig:<class>-<proto>-<recipe>-<kind>[-axis]) — the CLI checks `gh issue list
// --label <sig>` and either comments the new repro or opens a fresh Issue. This
// file builds the human-facing title/body; the gh calls live in the cmd layer
// so this stays pure + testable.

// IssueLabels are the GitHub labels a promoted finding carries. `sweep` scopes
// the search; the signature is the dedup key; the verdict picks bug-vs-notable
// priority (a notable is a lower-priority Issue, not a `bug`, per §7.4).
func IssueLabels(sig string, v Verdict) []string {
	labels := []string{"sweep", sig}
	if v == VerdictAberration {
		labels = append(labels, "bug")
	} else {
		labels = append(labels, "notable")
	}
	return labels
}

// IssueTitle is a one-line human summary: the aberration kind + the recipe that
// triggered it. Stable enough to read in a list, specific enough to tell two
// signatures apart.
func IssueTitle(e *Experiment, kind string) string {
	if kind == "" {
		kind = "anomaly"
	}
	return fmt.Sprintf("[sweep/%s] %s on %s/%s/%s (%s)",
		e.ClassOrDefault(), kind, e.Platform, e.Protocol, e.Mode, recipeSlug(e))
}

// IssueBody renders the markdown body for a finding Issue (or a follow-up
// comment when the signature already has an open Issue). `attributedAxis` is
// the isolation-confirmed cause if known (else ""). Designed to be written to
// a --body-file (per the repo's gh heredoc/body-file rule), never inlined.
func IssueBody(e *Experiment, sig, attributedAxis string) string {
	var b strings.Builder
	v, labels := VerdictClean, []string(nil)
	if e.Result != nil {
		v, labels = e.Result.Verdict, e.Result.Labels
	}
	kind := PrimaryKind(labels)

	fmt.Fprintf(&b, "## %s\n\n", IssueTitle(e, kind))
	fmt.Fprintf(&b, "Auto-detected by the fault-injection sweep (issue #772, `docs/sweep-design.md`).\n\n")

	fmt.Fprintf(&b, "**Verdict:** `%s`", v)
	if attributedAxis != "" {
		fmt.Fprintf(&b, " — attributed to axis `%s`", attributedAxis)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "**Signature:** `%s`\n\n", sig)

	b.WriteString("### Recipe\n\n")
	b.WriteString("| field | value |\n|---|---|\n")
	fmt.Fprintf(&b, "| class | %s |\n", e.ClassOrDefault())
	fmt.Fprintf(&b, "| platform | %s |\n", e.Platform)
	fmt.Fprintf(&b, "| protocol | %s |\n", e.Protocol)
	fmt.Fprintf(&b, "| content | %s |\n", e.Content)
	fmt.Fprintf(&b, "| mode | %s |\n", e.Mode)
	if e.Fault != nil {
		fmt.Fprintf(&b, "| fault | %s |\n", faultSlug(e.Fault))
	}
	if e.Shape != nil {
		fmt.Fprintf(&b, "| shape | %s |\n", shapeSummary(e.Shape))
	}
	if e.ContentManipulation != nil {
		fmt.Fprintf(&b, "| content_manipulation | %s |\n", cmSummary(e.ContentManipulation))
	}
	if e.TransferTimeouts != nil {
		fmt.Fprintf(&b, "| transfer_timeouts | %s |\n", transferSummary(e.TransferTimeouts))
	}
	fmt.Fprintf(&b, "| experiment | `%s` (kind=%s, depth=%d) |\n", e.ID, e.Kind, e.Depth)

	if len(labels) > 0 {
		b.WriteString("\n### QoE labels seen\n\n")
		for _, l := range labels {
			fmt.Fprintf(&b, "- `%s`\n", l)
		}
	}

	if e.PlayID != "" {
		b.WriteString("\n### Evidence\n\n")
		fmt.Fprintf(&b, "- play_id `%s` — `harness query play %s`\n", e.PlayID, e.PlayID)
		fmt.Fprintf(&b, "- dashboard: filter sessions by `sweep=1` / `exp_id=%s`\n", Slug(e.ID))
	}
	if e.WhyText != "" {
		fmt.Fprintf(&b, "\n### Why this ran\n\n%s\n", e.WhyText)
	}
	return b.String()
}

func shapeSummary(s *Shape) string {
	var parts []string
	if s.Pattern != "" {
		parts = append(parts, fmt.Sprintf("pattern=%s", s.Pattern))
	}
	if s.RateMbps != nil {
		parts = append(parts, fmt.Sprintf("rate=%gMbps", *s.RateMbps))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, " ")
}

func transferSummary(t *TransferTimeouts) string {
	var parts []string
	if t.ActiveSeconds > 0 {
		parts = append(parts, fmt.Sprintf("active=%ds", t.ActiveSeconds))
	}
	if t.IdleSeconds > 0 {
		parts = append(parts, fmt.Sprintf("idle=%ds", t.IdleSeconds))
	}
	if t.AppliesSegments {
		parts = append(parts, "segments")
	}
	if t.AppliesManifests {
		parts = append(parts, "manifests")
	}
	if t.AppliesMaster {
		parts = append(parts, "master")
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, " ")
}

func cmSummary(c *ContentManipulation) string {
	var parts []string
	if c.LiveOffset != nil {
		parts = append(parts, fmt.Sprintf("live_offset=%g", *c.LiveOffset))
	}
	if c.AllowedVariants != "" {
		parts = append(parts, "allowed_variants="+c.AllowedVariants)
	}
	if c.VariantOrder != "" {
		parts = append(parts, "variant_order="+c.VariantOrder)
	}
	if c.StripCodecs {
		parts = append(parts, "strip_codecs")
	}
	if c.StripAvgBandwidth {
		parts = append(parts, "strip_avg_bandwidth")
	}
	if c.StripResolution {
		parts = append(parts, "strip_resolution")
	}
	if c.OverstateBandwidth != nil {
		parts = append(parts, fmt.Sprintf("overstate=%g", *c.OverstateBandwidth))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, " ")
}
