package server

import (
	"encoding/json"
	"fmt"
	"strings"
)

// JSON Merge Patch (RFC 7396) helpers used by v2 PATCH handlers.
//
// The handlers want two things from a Merge Patch body:
//
//  1. The set of *leaf paths* the patch will write — used to compare
//     against the resource's per-field revisions for conflict
//     detection (see FieldRevisions.Conflicts).
//
//  2. An applied result — the patched object — that the handler
//     marshals back into the v1 SessionData map for storage.
//
// Both are derived from a generic decode (`map[string]any`) so the
// helpers don't need to know which resource they're patching.

// LeafPaths returns the dot-separated leaf paths a Merge Patch body
// would write against an empty target. Examples:
//
//	{"shape": {"rate_mbps": 5}}                  → ["shape.rate_mbps"]
//	{"shape": {"rate_mbps": null}}               → ["shape.rate_mbps"]
//	{"labels": null}                             → ["labels"]
//	{"labels": {"a": "x", "b": null}}            → ["labels.a", "labels.b"]
//	{"fault_rules": [...]}                       → ["fault_rules"]
//
// Arrays are treated as opaque leaves — Merge Patch replaces an array
// wholesale (RFC 7396 §1, second bullet), so the per-field concurrency
// scope for an array field is the field itself, not its elements.
func LeafPaths(patch map[string]any) []string {
	out := []string{}
	collectLeafPaths(patch, "", &out)
	return out
}

func collectLeafPaths(node any, prefix string, out *[]string) {
	switch v := node.(type) {
	case map[string]any:
		if len(v) == 0 {
			// {} on a sub-object is a no-op per Merge Patch — but
			// the top-level body always reaches this path with a
			// non-empty map, so empty inner maps are vanishingly
			// rare and harmless to ignore.
			if prefix != "" {
				*out = append(*out, prefix)
			}
			return
		}
		for k, child := range v {
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			collectLeafPaths(child, path, out)
		}
	default:
		// Scalars, nulls, arrays — opaque leaves.
		if prefix != "" {
			*out = append(*out, prefix)
		}
	}
}

// ApplyMergePatch applies an RFC 7396 Merge Patch body to a target
// object in place.
//
// Semantics (verbatim from the RFC):
//   - patch[k] == null  →  delete target[k]
//   - patch[k] is an object and target[k] is an object → recurse
//   - otherwise → target[k] = patch[k] (replace)
//
// Returns the (possibly modified) target. The function never returns
// an error — the Merge Patch RFC has no failure modes once the body
// is valid JSON.
func ApplyMergePatch(target, patch map[string]any) map[string]any {
	if target == nil {
		target = map[string]any{}
	}
	for k, pv := range patch {
		if pv == nil {
			delete(target, k)
			continue
		}
		pm, pIsMap := pv.(map[string]any)
		if !pIsMap {
			target[k] = pv
			continue
		}
		tv, exists := target[k]
		tm, tIsMap := tv.(map[string]any)
		if !exists || !tIsMap {
			tm = map[string]any{}
		}
		target[k] = ApplyMergePatch(tm, pm)
	}
	return target
}

// DecodePatch unmarshals a Merge Patch body into a generic map. The
// body must be a JSON object — top-level scalars, arrays, or `null`
// are rejected (RFC 7396 allows them in principle, but the v2 API
// surface uses object-shaped resources only, so a non-object patch
// is always a client error).
//
// Returns the decoded map and the leaf paths it would write.
func DecodePatch(body []byte) (map[string]any, []string, error) {
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, nil, fmt.Errorf("empty merge patch body")
	}
	var patch any
	if err := json.Unmarshal(body, &patch); err != nil {
		return nil, nil, fmt.Errorf("invalid json: %w", err)
	}
	m, ok := patch.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("merge patch must be a json object")
	}
	return m, LeafPaths(m), nil
}
