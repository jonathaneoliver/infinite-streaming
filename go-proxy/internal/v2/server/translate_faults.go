package server

import "fmt"

// Translation between v2's `fault_rules` array model and v1's
// 5-surface storage model.
//
// v2 model: an ordered array of FaultRule entries, each with its own
// filter + behaviour. Evaluation is first-match-wins.
//
// v1 model: five hardcoded surfaces (segment, partial, manifest,
// master_manifest, audio_segment, audio_manifest, init) — each with its
// own (failure_type, frequency, consecutive, units, mode) tuple — plus
// an `all_*` family that overrides every surface when set.
//
// Translation strategy:
//  1. Walk the v2 fault_rules array.
//  2. For each rule, derive the target v1 surface from
//     `filter.request_kind`. A rule with no filter maps to the v1
//     `all_*` family.
//  3. Map the v2 type / frequency / consecutive / mode straight onto
//     the v1 surface fields.
//  4. Stash the original v2 array on `_v2_fault_rules` for round-trip.
//
// Subset support: rules with `filter.variant`, `filter.codec`, or
// `filter.url_match` need v1-side variant tracking that doesn't exist
// yet — those return an unsupportedFaultRuleError so the handler can
// surface a 501 with detail.

// supportedFaultTypes lists the v2 FaultRule.type values that map to
// v1 surface failure_type. Other v2 types (corrupted, request_*_hang,
// etc.) need v1 plumbing changes that don't exist yet.
var supportedFaultTypes = map[string]bool{
	"none":      true,
	"404":       true,
	"500":       true,
	"503":       true,
	"timeout":   true,
	"corrupted": true,
}

// v1SurfaceForRequestKind returns the v1 field-name prefix for a v2
// request_kind value. Returns ("", false) for v2-only kinds (init,
// partial, audio_*) that v1 doesn't model as a distinct surface.
func v1SurfaceForRequestKind(kind string) (string, bool) {
	switch kind {
	case "segment":
		return "segment", true
	case "partial":
		// v1's segment surface covers HLS partials too; the proxy
		// doesn't distinguish during request classification.
		return "segment", true
	case "manifest":
		return "manifest", true
	case "master_manifest":
		return "master_manifest", true
	case "audio_segment", "audio_manifest", "init":
		// v1 has no dedicated surface — Phase I+ work.
		return "", false
	}
	return "", false
}

// unsupportedFaultRuleError describes why a v2 fault_rule can't be
// translated to v1. Surfaced as 501 with detail by the handler.
type unsupportedFaultRuleError struct {
	RuleID string
	Reason string
}

func (e *unsupportedFaultRuleError) Error() string {
	if e.RuleID == "" {
		return "unsupported fault_rule: " + e.Reason
	}
	return fmt.Sprintf("unsupported fault_rule %q: %s", e.RuleID, e.Reason)
}

// faultSurfaces names every v1 surface translateFaultRules can write
// to. Used to clear stale v1 state before re-applying the v2 array.
var faultSurfaces = []string{"segment", "manifest", "master_manifest", "all"}

// clearV1FaultSurfaces resets every v1 fault surface to "none" / 0.
// Called before applying a fresh v2 fault_rules array so abandoned
// rules don't linger in v1's surface fields.
func clearV1FaultSurfaces(s map[string]any) {
	for _, surface := range faultSurfaces {
		s[surface+"_failure_type"] = "none"
		s[surface+"_failure_frequency"] = 0
		s[surface+"_consecutive_failures"] = 0
		s[surface+"_failure_mode"] = "failures_per_seconds"
		s[surface+"_failure_units"] = "requests"
		s[surface+"_consecutive_units"] = "requests"
		s[surface+"_frequency_units"] = "seconds"
	}
}

// translateFaultRules applies a v2 fault_rules array to v1 surface
// fields on the supplied session map. Stashes the original array on
// `_v2_fault_rules`. Returns an unsupportedFaultRuleError if any rule
// uses a filter the v1 model can't represent.
//
// Rules are first-match-wins: only the first rule targeting each v1
// surface produces side-effects; subsequent rules that hit the same
// surface are stored on `_v2_fault_rules` but don't drive v1 state.
func translateFaultRules(s map[string]any, rules []any) error {
	clearV1FaultSurfaces(s)
	if rules == nil {
		delete(s, "_v2_fault_rules")
		return nil
	}
	s["_v2_fault_rules"] = rules

	used := map[string]bool{}
	for _, raw := range rules {
		rule, ok := raw.(map[string]any)
		if !ok {
			return &unsupportedFaultRuleError{Reason: "fault_rules entries must be objects"}
		}
		if err := applyOneFaultRule(s, rule, used); err != nil {
			return err
		}
	}
	return nil
}

// applyOneFaultRule writes one v2 rule to the v1 surface(s) it
// targets. Updates `used` so first-match-wins ordering holds.
func applyOneFaultRule(s map[string]any, rule map[string]any, used map[string]bool) error {
	id, _ := rule["id"].(string)

	// Reject filter shapes the v1 model can't express.
	filterAny, hasFilter := rule["filter"]
	var filter map[string]any
	if hasFilter && filterAny != nil {
		f, ok := filterAny.(map[string]any)
		if !ok {
			return &unsupportedFaultRuleError{RuleID: id, Reason: "filter must be an object"}
		}
		if _, has := f["variant"]; has {
			return &unsupportedFaultRuleError{RuleID: id, Reason: "filter.variant requires v1 variant tracking (not yet wired)"}
		}
		if _, has := f["url_match"]; has {
			return &unsupportedFaultRuleError{RuleID: id, Reason: "filter.url_match requires v1 substring matching on a per-rule scope (not yet wired)"}
		}
		if _, has := f["codec"]; has {
			return &unsupportedFaultRuleError{RuleID: id, Reason: "filter.codec requires v1 variant tracking (not yet wired)"}
		}
		filter = f
	}

	faultType, _ := rule["type"].(string)
	if !supportedFaultTypes[faultType] {
		return &unsupportedFaultRuleError{
			RuleID: id,
			Reason: "unsupported fault type — v1 surface model accepts none / 404 / 500 / 503 / timeout / corrupted",
		}
	}

	frequency := 0
	if f, ok := numericFloat(rule["frequency"]); ok {
		frequency = int(f)
	}
	consecutive := 1
	if f, ok := numericFloat(rule["consecutive"]); ok && int(f) >= 1 {
		consecutive = int(f)
	}
	mode, _ := rule["mode"].(string)
	if mode == "" {
		mode = "failures_per_seconds"
	}

	// Resolve targets.
	var targets []string
	switch {
	case filter == nil:
		targets = []string{"all"}
	default:
		kinds, ok := filter["request_kind"].([]any)
		if !ok || len(kinds) == 0 {
			// filter present but no request_kind → effectively no
			// surface scoping, treat as all.
			targets = []string{"all"}
		} else {
			seen := map[string]bool{}
			for _, k := range kinds {
				kindStr, ok := k.(string)
				if !ok {
					continue
				}
				surface, ok := v1SurfaceForRequestKind(kindStr)
				if !ok {
					return &unsupportedFaultRuleError{
						RuleID: id,
						Reason: fmt.Sprintf("request_kind %q has no v1 surface (init / audio_* not yet wired)", kindStr),
					}
				}
				if !seen[surface] {
					seen[surface] = true
					targets = append(targets, surface)
				}
			}
			if len(targets) == 0 {
				targets = []string{"all"}
			}
		}
	}

	for _, surface := range targets {
		if used[surface] {
			// First-match-wins: a previous rule already claimed this
			// v1 surface. Subsequent rules that hit it are stored on
			// `_v2_fault_rules` (already done above) but don't drive
			// v1 state.
			continue
		}
		used[surface] = true
		s[surface+"_failure_type"] = faultType
		s[surface+"_failure_frequency"] = frequency
		s[surface+"_consecutive_failures"] = consecutive
		s[surface+"_failure_mode"] = mode
		switch mode {
		case "failures_per_seconds":
			s[surface+"_consecutive_units"] = "requests"
			s[surface+"_frequency_units"] = "seconds"
		case "requests":
			s[surface+"_consecutive_units"] = "requests"
			s[surface+"_frequency_units"] = "requests"
		case "seconds":
			s[surface+"_consecutive_units"] = "seconds"
			s[surface+"_frequency_units"] = "seconds"
		}
	}
	return nil
}

// faultRulesFromSession returns the v2 fault_rules array stashed on the
// session map (or nil if the player has never had a v2-shaped rules
// patch applied). Used by the per-rule sub-resource handlers to
// fetch / mutate / delete one rule by id.
func faultRulesFromSession(s map[string]any) []any {
	if s == nil {
		return nil
	}
	if v, ok := s["_v2_fault_rules"].([]any); ok {
		return v
	}
	return nil
}

// findFaultRuleIndex returns the array index of the first rule whose
// id matches the supplied ruleID, or -1.
func findFaultRuleIndex(rules []any, ruleID string) int {
	for i, raw := range rules {
		rule, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := rule["id"].(string); id == ruleID {
			return i
		}
	}
	return -1
}
