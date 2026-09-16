package main

import "testing"

// The serviceable gate (#949) builds a ClickHouse Array(String) param from the
// caller's platform allow-list. A mistake here either injects SQL or silently
// matches nothing, so pin the exact formatting + escaping.
func TestChStringArrayParam(t *testing.T) {
	cases := map[string]struct {
		in   []string
		want string
	}{
		"empty":  {nil, "[]"},
		"single": {[]string{"ipad-sim"}, "['ipad-sim']"},
		"multi":  {[]string{"ipad-sim", "iphone"}, "['ipad-sim','iphone']"},
		// A quote or backslash in an element must be escaped, not break the literal.
		"quote":     {[]string{"a'b"}, `['a\'b']`},
		"backslash": {[]string{`a\b`}, `['a\\b']`},
	}
	for name, c := range cases {
		if got := chStringArrayParam(c.in); got != c.want {
			t.Errorf("%s: chStringArrayParam(%q) = %q, want %q", name, c.in, got, c.want)
		}
	}
}

func TestTrimStrings(t *testing.T) {
	got := trimStrings([]string{" ipad-sim ", "", "iphone", "   "})
	if len(got) != 2 || got[0] != "ipad-sim" || got[1] != "iphone" {
		t.Errorf("trimStrings dropped/kept wrong entries: %q", got)
	}
	if trimStrings(nil) != nil {
		t.Errorf("trimStrings(nil) should be nil")
	}
}
