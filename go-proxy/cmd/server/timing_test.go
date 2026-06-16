package main

import "testing"

// #613: mergeTotalTiming lifts the provisional headers-complete TotalMs to
// TTFB+Transfer, but only when that is larger — so it's safe to call
// uniformly from the logEntry closure regardless of which path produced the
// row.
func TestMergeTotalTiming(t *testing.T) {
	cases := []struct {
		name             string
		ttfb, transfer   float64
		total, wantTotal float64
	}{
		{
			// The ~26% regression case: TotalMs was set at headers-complete
			// (≈TTFB) before the body transferred; lift to TTFB+Transfer.
			name: "lifts pre-transfer total", ttfb: 0.16, transfer: 103, total: 0.18, wantTotal: 103.16,
		},
		{
			// Already complete (or larger) — leave it (idempotent re-call).
			name: "idempotent when already combined", ttfb: 0.16, transfer: 103, total: 103.16, wantTotal: 103.16,
		},
		{
			// Fault row: TTFB+Transfer are 0, TotalMs carries the latency to
			// the status line — must not be clobbered to 0.
			name: "fault row keeps wait total", ttfb: 0, transfer: 0, total: 12.5, wantTotal: 12.5,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := &NetworkLogEntry{TTFBMs: c.ttfb, TransferMs: c.transfer, TotalMs: c.total}
			mergeTotalTiming(e)
			if e.TotalMs != c.wantTotal {
				t.Errorf("TotalMs = %.3f, want %.3f", e.TotalMs, c.wantTotal)
			}
		})
	}
}

func TestMergeTotalTimingNil(t *testing.T) {
	mergeTotalTiming(nil) // must not panic
}
