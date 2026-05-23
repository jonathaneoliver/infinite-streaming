package plays

import "strings"

// LabelFilter expresses the tristate label predicate the v2 list
// endpoints and the chat tools both consume: every Has value must be
// on the row; none of the Not values may be. Empty fields = no
// constraint.
type LabelFilter struct {
	Has []string
	Not []string
}

// applyTo appends the CH WHERE fragments + matching parameters for
// this filter against `column` (e.g. `labels` or `labels_agg.labels_distinct`).
// Idempotent on empty input.
func (f LabelFilter) applyTo(clauses []string, params map[string]string, column string) ([]string, map[string]string) {
	if len(f.Has) > 0 {
		names := make([]string, len(f.Has))
		for i, v := range f.Has {
			key := "label_has_" + itoa(i)
			params[key] = v
			names[i] = "{" + key + ":String}"
		}
		clauses = append(clauses, "hasAll("+column+", ["+strings.Join(names, ", ")+"])")
	}
	if len(f.Not) > 0 {
		names := make([]string, len(f.Not))
		for i, v := range f.Not {
			key := "label_not_" + itoa(i)
			params[key] = v
			names[i] = "{" + key + ":String}"
		}
		clauses = append(clauses, "NOT hasAny("+column+", ["+strings.Join(names, ", ")+"])")
	}
	return clauses, params
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
