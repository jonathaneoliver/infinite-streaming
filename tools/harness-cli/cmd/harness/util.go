package main

import "strings"

// shortRev truncates a control_revision token for human display.
// The full token is 32+ hex chars; the first 8 are unambiguous in
// practice and fit the operator's mental model of "this is the
// fingerprint of the state after the write".
func shortRev(rev string) string {
	rev = strings.Trim(rev, `"`)
	if rev == "" {
		return "—"
	}
	if len(rev) > 8 {
		return rev[:8]
	}
	return rev
}
