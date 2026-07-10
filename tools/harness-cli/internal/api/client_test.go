package api

import "testing"

// TestWithBaseURL covers the per-arm cross-server clone (#942): a new base returns
// a distinct client rebound to that origin while reusing the transport + auth; an
// empty or identical base returns the receiver unchanged (no needless rebuild).
func TestWithBaseURL(t *testing.T) {
	c, err := New(Options{BaseURL: "https://a.example:21000", Insecure: true, BasicAuth: "u:p"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Empty base → same client (pointer identity, no rebuild).
	if got, err := c.WithBaseURL(""); err != nil || got != c {
		t.Errorf("WithBaseURL(\"\") = (%p, %v), want (%p, nil)", got, err, c)
	}
	// Same base (with a trailing slash) → same client.
	if got, err := c.WithBaseURL("https://a.example:21000/"); err != nil || got != c {
		t.Errorf("WithBaseURL(same) = (%p, %v), want the receiver", got, err)
	}
	// Different base → distinct client at the new origin, auth carried over.
	got, err := c.WithBaseURL("https://b.example:28000")
	if err != nil {
		t.Fatalf("WithBaseURL(new): %v", err)
	}
	if got == c {
		t.Fatal("WithBaseURL(new) returned the receiver; want a distinct client")
	}
	if got.BaseURL != "https://b.example:28000" {
		t.Errorf("BaseURL = %q, want https://b.example:28000", got.BaseURL)
	}
	if got.BasicAuth != c.BasicAuth || got.HTTP != c.HTTP {
		t.Errorf("clone should reuse auth + HTTP transport")
	}
}
