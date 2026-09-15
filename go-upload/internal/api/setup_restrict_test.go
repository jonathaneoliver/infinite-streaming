package api

import "testing"

func TestRestrictPublicHostsEnabled(t *testing.T) {
	cases := map[string]bool{"": false, "0": false, "false": false, "no": false, "1": true, "true": true, "TRUE": true, "yes": true, " on ": true}
	for val, want := range cases {
		t.Setenv("INFINITE_STREAM_RESTRICT_PUBLIC_HOSTS", val)
		if got := restrictPublicHostsEnabled(); got != want {
			t.Errorf("INFINITE_STREAM_RESTRICT_PUBLIC_HOSTS=%q: got %v, want %v", val, got, want)
		}
	}
}
