package main

import "testing"

// TestTcAddAlreadyExists guards the #745 fix: EnsureRootClass/EnsureRootQdisc
// must treat a concurrent-create "already exists" as success (so the loser of
// a shared-root race still installs its per-port leaf class), while still
// failing on genuine errors. Real `tc` needs Linux + privileges, so we test
// the error classifier directly against the kernel's actual RTNETLINK strings.
func TestTcAddAlreadyExists(t *testing.T) {
	benign := []string{
		"RTNETLINK answers: File exists",
		"Error: Exclusivity flag on, cannot modify.",
		"rtnetlink answers: file exists\n",
	}
	for _, s := range benign {
		if !tcAddAlreadyExists([]byte(s)) {
			t.Errorf("expected benign already-exists for %q", s)
		}
	}

	fatal := []string{
		"RTNETLINK answers: No such file or directory",
		"RTNETLINK answers: Operation not permitted",
		"Error: Invalid handle.",
		"",
		"some unrelated tc failure",
	}
	for _, s := range fatal {
		if tcAddAlreadyExists([]byte(s)) {
			t.Errorf("expected fatal (not already-exists) for %q", s)
		}
	}
}
