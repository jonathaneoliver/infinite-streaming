package app

import "testing"

// The post-encode warm-up HEADs /go-live over plain HTTP, so it must target
// nginx's cleartext loopback listener. It used INFINITE_STREAM_LISTEN_PORT
// (30000), which is HTTPS-only when TLS is on: every warm-up got a 400.
func TestGoLiveWarmBaseUsesCleartextLoopback(t *testing.T) {
	t.Setenv("INFINITE_STREAM_LISTEN_PORT", "30000")

	t.Setenv("INFINITE_STREAM_UPSTREAM_PORT", "")
	if got, want := goLiveWarmBase(), "http://127.0.0.1:30005/go-live"; got != want {
		t.Fatalf("default: goLiveWarmBase() = %q, want %q", got, want)
	}

	t.Setenv("INFINITE_STREAM_UPSTREAM_PORT", "31005")
	if got, want := goLiveWarmBase(), "http://127.0.0.1:31005/go-live"; got != want {
		t.Fatalf("override: goLiveWarmBase() = %q, want %q", got, want)
	}
}
