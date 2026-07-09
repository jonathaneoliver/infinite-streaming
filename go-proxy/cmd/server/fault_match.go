package main

import (
	"regexp"
	"strings"
)

// MatchedFault is the behaviour half of a fault_rule extracted after its filter
// matched a request — the tuple the cadence engine (FailureHandler) needs.
// RuleID keys the per-rule cadence state on the session (#919 step 2).
type MatchedFault struct {
	RuleID      string
	Type        string
	Frequency   int
	Mode        string
	Consecutive int
}

// matchFaultRule walks the v2 fault_rules array first-match-wins and returns
// the first ACTIVE rule whose FaultFilter matches rc. ok=false when none match.
//
// A rule with Type "none" is DISABLED ("`none` disables this rule without
// removing it") — it is skipped, never matched, so it can't shadow a later
// active rule. This is what the dashboard's per-surface model relies on: an
// "All" surface left at none must not shield a segment fault behind it, and it
// mirrors the old v1 engine where a none all-rule was inert.
func matchFaultRule(rules []any, rc RequestClass) (MatchedFault, bool) {
	for _, raw := range rules {
		rule, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if t := ruleString(rule["type"]); t == "" || t == "none" {
			continue // disabled rule — skip, don't shadow
		}
		if !faultFilterMatches(rule["filter"], rc) {
			continue
		}
		return MatchedFault{
			RuleID:      ruleString(rule["id"]),
			Type:        ruleString(rule["type"]),
			Frequency:   intFromInterface(rule["frequency"]),
			Mode:        ruleString(rule["mode"]),
			Consecutive: intFromInterface(rule["consecutive"]),
		}, true
	}
	return MatchedFault{}, false
}

// faultFilterMatches evaluates a FaultFilter against rc. All present predicates
// (request_kind, variant, url_match) AND together. A nil/absent filter matches
// every request on the surface.
func faultFilterMatches(filterAny any, rc RequestClass) bool {
	if filterAny == nil {
		return true
	}
	filter, ok := filterAny.(map[string]any)
	if !ok {
		return true // malformed filter → treat as no filter (match all), never a silent 500
	}
	if kinds, ok := filter["request_kind"].([]any); ok && len(kinds) > 0 {
		if !containsStringFold(kinds, rc.Kind) {
			return false
		}
	}
	if v, present := filter["variant"]; present && v != nil {
		if !variantPredicateMatches(v, rc) {
			return false
		}
	}
	if u, present := filter["url_match"]; present && u != nil {
		if !urlMatchMatches(u, rc) {
			return false
		}
	}
	return true
}

// variantPredicateMatches evaluates a VariantPredicate against rc. Every SET
// sub-predicate must pass (AND). A request the proxy can't map to a video
// variant (RungIndex < 0 — audio, init, master, unmatched) NEVER matches a
// variant predicate; that is exactly what stops a video-scoped rule from
// hitting audio/init (#917).
func variantPredicateMatches(vAny any, rc RequestClass) bool {
	v, ok := vAny.(map[string]any)
	if !ok {
		return false
	}
	if rc.RungIndex < 0 {
		return false
	}
	if ri, ok := v["rung_indexes"].([]any); ok && len(ri) > 0 {
		if !containsInt(ri, rc.RungIndex) {
			return false
		}
	}
	if rp, ok := v["rung_positions"].([]any); ok && len(rp) > 0 {
		if !rungPositionMatches(rp, rc) {
			return false
		}
	}
	if res, ok := v["resolutions"].([]any); ok && len(res) > 0 {
		if rc.Resolution == "" || !containsStringFold(res, rc.Resolution) {
			return false
		}
	}
	if raw, ok := v["bandwidth_above"]; ok && raw != nil {
		if !(rc.BandwidthBps > intFromInterface(raw)) {
			return false
		}
	}
	if raw, ok := v["bandwidth_below"]; ok && raw != nil {
		if !(rc.BandwidthBps > 0 && rc.BandwidthBps < intFromInterface(raw)) {
			return false
		}
	}
	if cp := ruleString(v["codec_prefix"]); cp != "" {
		// Codecs is not yet sourced from manifest_variants; an empty rc.Codecs
		// can't satisfy a codec_prefix predicate, so this fails closed until
		// CODECS is plumbed. Documented limitation, not a silent match.
		if !strings.HasPrefix(rc.Codecs, cp) {
			return false
		}
	}
	return true
}

// rungPositionMatches resolves symbolic ladder positions (top / bottom / …)
// against the current ladder size and reports whether rc's rung is among them.
func rungPositionMatches(positions []any, rc RequestClass) bool {
	n := rc.LadderSize
	for _, p := range positions {
		s, _ := p.(string)
		target := -1
		switch s {
		case "bottom":
			target = 0
		case "second_from_bottom":
			target = 1
		case "top":
			target = n - 1
		case "second_from_top":
			target = n - 2
		}
		if target >= 0 && target == rc.RungIndex {
			return true
		}
	}
	return false
}

// urlMatchMatches evaluates a UrlMatch (exact / basename / substring / regex)
// against rc.Path. Patterns OR together.
func urlMatchMatches(uAny any, rc RequestClass) bool {
	u, ok := uAny.(map[string]any)
	if !ok {
		return false
	}
	mode := ruleString(u["mode"])
	patterns, ok := u["patterns"].([]any)
	if !ok || len(patterns) == 0 {
		return false
	}
	base := pathBase(rc.Path)
	for _, pAny := range patterns {
		p, _ := pAny.(string)
		if p == "" {
			continue
		}
		switch mode {
		case "exact":
			if rc.Path == p {
				return true
			}
		case "basename":
			if base == p {
				return true
			}
		case "substring":
			if strings.Contains(rc.Path, p) {
				return true
			}
		case "regex":
			if re, err := regexp.Compile(p); err == nil && re.MatchString(rc.Path) {
				return true
			}
		}
	}
	return false
}

func ruleString(v any) string {
	s, _ := v.(string)
	return s
}

func containsStringFold(arr []any, target string) bool {
	for _, x := range arr {
		if s, ok := x.(string); ok && strings.EqualFold(s, target) {
			return true
		}
	}
	return false
}

func containsInt(arr []any, target int) bool {
	for _, x := range arr {
		if intFromInterface(x) == target {
			return true
		}
	}
	return false
}
