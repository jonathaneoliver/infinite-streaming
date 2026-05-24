package plays

import "strings"

// LabelFilter expresses the tristate label predicate the v2 list
// endpoints and the chat tools both consume: every Has value must
// be on the row; none of the Not values may be. Empty fields = no
// constraint.
//
// Pattern semantics (per entry):
//   - "critical=frozen"          exact match (no wildcards)
//   - "critical=%"               LIKE: matches every label starting
//                                with "critical=" — includes both
//                                direct (critical=frozen) and
//                                synthesized (critical=*stall_…).
//   - "%=*%"                     LIKE: every synthesized label
//   - "%stall%"                  LIKE: substring match on "stall"
//
// `%` = zero or more chars (SQL LIKE). `_` is escaped to a literal
// underscore via the ESCAPE clause so real event names with `_`
// (`segment_stall`, `stall_severe_startup`) keep their exact-match
// semantics — backward-compatible with every caller that pre-dates
// this change.
type LabelFilter struct {
	Has []string
	Not []string
}

// applyTo appends the CH WHERE fragments + matching parameters for
// this filter against `column` (e.g. `labels` or
// `labels_agg.labels_distinct`). Idempotent on empty input.
func (f LabelFilter) applyTo(clauses []string, params map[string]string, column string) ([]string, map[string]string) {
	for i, v := range f.Has {
		key := "label_has_" + itoa(i)
		params[key] = escapeLikeUnderscores(v)
		// arrayExists with LIKE per pattern (vs hasAll on the
		// array) lets each Has entry be a glob. Multiple Has entries
		// AND together via separate clauses.
		clauses = append(clauses,
			"arrayExists(x -> x LIKE {"+key+":String} ESCAPE '\\\\', "+column+")")
	}
	for i, v := range f.Not {
		key := "label_not_" + itoa(i)
		params[key] = escapeLikeUnderscores(v)
		clauses = append(clauses,
			"NOT arrayExists(x -> x LIKE {"+key+":String} ESCAPE '\\\\', "+column+")")
	}
	return clauses, params
}

// escapeLikeUnderscores turns `_` into `\_` so the ESCAPE clause
// treats it as a literal. Without this, a pattern like
// `critical=segment_stall` would silently match
// `critical=segmentXstall` (where X is any char) and break the
// existing exact-match contract for callers that don't know about
// LIKE semantics. `%` stays unescaped — that's the wildcard.
func escapeLikeUnderscores(s string) string {
	if !strings.ContainsRune(s, '_') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for _, r := range s {
		if r == '_' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

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
