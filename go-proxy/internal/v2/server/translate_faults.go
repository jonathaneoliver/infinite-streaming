package server

import "fmt"

// Storage + validation for the v2 fault_rules array.
//
// Since #925 the proxy runtime evaluates fault_rules NATIVELY
// (evaluateFaultRules in package main reads `_v2_fault_rules` directly), so
// there is no longer any translation to v1 surface fields. This file just
// stores the array and validates rule shape — the name is kept for the many
// handler call sites.

// unsupportedFaultRuleError describes why a v2 fault_rule is malformed.
// Surfaced as 501 with detail by the handler.
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

// translateFaultRules stores the v2 fault_rules array as the sole fault input
// for the native evaluator, and resets the per-rule cadence cursors so a
// re-armed rule opens a FRESH window (#643) rather than resuming the previous
// arm's half-consumed one. Returns an unsupportedFaultRuleError for a malformed
// rule (surfaced as 501). rules==nil clears all fault config.
func translateFaultRules(s map[string]any, rules []any) error {
	// Re-arm reset: drop the native per-rule cadence state (the #925 successor
	// to clearing v1's <surface>_failure_at cursors) so a config change doesn't
	// resume the previous arm's window.
	delete(s, "_faultrule_state")
	if rules == nil {
		delete(s, "_v2_fault_rules")
		return nil
	}
	for _, raw := range rules {
		rule, ok := raw.(map[string]any)
		if !ok {
			return &unsupportedFaultRuleError{Reason: "fault_rules entries must be objects"}
		}
		if err := validateFaultRule(rule); err != nil {
			return err
		}
	}
	s["_v2_fault_rules"] = rules
	return nil
}

// validateFaultRule rejects structurally-malformed rules (a client 4xx/501).
// The native matcher tolerates absent/loose fields and honours any fault type,
// so only a broken filter/url_match shape is an error.
func validateFaultRule(rule map[string]any) error {
	id, _ := rule["id"].(string)
	filterAny, hasFilter := rule["filter"]
	if !hasFilter || filterAny == nil {
		return nil
	}
	f, ok := filterAny.(map[string]any)
	if !ok {
		return &unsupportedFaultRuleError{RuleID: id, Reason: "filter must be an object"}
	}
	um, has := f["url_match"]
	if !has || um == nil {
		return nil
	}
	umMap, ok := um.(map[string]any)
	if !ok {
		return &unsupportedFaultRuleError{RuleID: id, Reason: "filter.url_match must be an object"}
	}
	if mode, _ := umMap["mode"].(string); mode != "" {
		switch mode {
		case "substring", "basename", "exact", "regex":
		default:
			return &unsupportedFaultRuleError{RuleID: id, Reason: fmt.Sprintf("filter.url_match.mode=%q is not recognised", mode)}
		}
	}
	patterns, _ := umMap["patterns"].([]any)
	for _, p := range patterns {
		if str, ok := p.(string); ok && str != "" {
			return nil
		}
	}
	return &unsupportedFaultRuleError{RuleID: id, Reason: "filter.url_match.patterns must contain at least one non-empty string"}
}

// faultRulesFromSession returns the stored v2 fault_rules array (raw), used by
// the per-rule sub-resource handlers to locate and mutate a single rule.
func faultRulesFromSession(s map[string]any) []any {
	if s == nil {
		return nil
	}
	if v, ok := s["_v2_fault_rules"].([]any); ok {
		return v
	}
	return nil
}

// findFaultRuleIndex returns the index of the rule with the given id, or -1.
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
