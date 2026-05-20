package main

// label_filter.go — server-side label_has / label_not query-param
// support for the /api/v2/{snapshots,network_requests,control_events}
// list endpoints and /api/v2/plays. Push down the same tristate
// include / exclude semantics the Sessions page's hierarchical filter
// renders client-side, so harness CLI consumers (and any direct curl)
// don't have to pull whole pages and jq them.
//
// AND-within-includes: every requested `label_has=` value must be on
//   the row.
// AND-within-excludes: none of the `label_not=` values may be on the
//   row.
//
// Empty lists → no constraint. Implemented via CH's hasAll() and
// hasAny() array predicates.

import (
	"net/url"
	"strings"
)

// readLabelFilters parses `label_has` and `label_not` query params
// from the URL. Each may be repeated. Returns sanitised slices with
// blank entries stripped.
func readLabelFilters(q url.Values) (has, not []string) {
	for _, v := range q["label_has"] {
		v = strings.TrimSpace(v)
		if v != "" {
			has = append(has, v)
		}
	}
	for _, v := range q["label_not"] {
		v = strings.TrimSpace(v)
		if v != "" {
			not = append(not, v)
		}
	}
	return has, not
}

// applyLabelFilters appends ClickHouse WHERE-clause fragments and
// matching params for the given include/exclude label lists. Returns
// the updated clauses + params. Caller owns the maps; idempotent on
// empty input.
//
// `labelsCol` is the CH column name (`labels` for direct tables;
// callers querying via a CTE may need to qualify, e.g. `t.labels`).
func applyLabelFilters(
	clauses []string,
	params map[string]string,
	labelsCol string,
	has, not []string,
) ([]string, map[string]string) {
	if len(has) > 0 {
		// CH array literal: ['a','b'] inlined via a String-typed
		// parameter holding the JSON-encoded list, then parsed at
		// query time via JSONExtractArrayRaw → arrayMap to
		// hasAll(). Cleaner alternative: pass each as a separate
		// {name:String} placeholder. Pick the second — fewer moving
		// parts and oapi-codegen-friendly.
		names := make([]string, len(has))
		for i, v := range has {
			key := "label_has_" + itoa(i)
			params[key] = v
			names[i] = "{" + key + ":String}"
		}
		clauses = append(clauses,
			"hasAll("+labelsCol+", ["+strings.Join(names, ", ")+"])")
	}
	if len(not) > 0 {
		names := make([]string, len(not))
		for i, v := range not {
			key := "label_not_" + itoa(i)
			params[key] = v
			names[i] = "{" + key + ":String}"
		}
		clauses = append(clauses,
			"NOT hasAny("+labelsCol+", ["+strings.Join(names, ", ")+"])")
	}
	return clauses, params
}

// itoa — tiny stdlib-free int-to-string for label_has_0 / label_not_0
// placeholder names. Bounded by the number of labels an operator can
// reasonably pass on one request (<100).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

