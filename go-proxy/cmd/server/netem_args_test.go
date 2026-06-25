package main

import (
	"strings"
	"testing"
)

// TestNetemImpairmentArgs locks the #826 wiring: the netem argument list must
// express delay+jitter as a normal distribution (with optional correlation)
// and loss as CORRELATED/bursty (`loss PCT CORR%`) — not netem's default
// independent-uniform loss. The "loss is verifiably bursty, not uniform"
// acceptance criterion lives here as a pure assertion (no tc / Linux needed).
func TestNetemImpairmentArgs(t *testing.T) {
	cases := []struct {
		name string
		p    NetemParams
		want string // exact space-joined arg list after the `netem` token
	}{
		{
			name: "clean link → no-op",
			p:    NetemParams{},
			want: "",
		},
		{
			name: "rate-only carries no netem args",
			p:    NetemParams{}, // rate lives on the qdisc/class, not netem
			want: "",
		},
		{
			name: "delay only → auto-jitter (5% of mean), normal distribution",
			p:    NetemParams{DelayMs: 100},
			want: "delay 100ms 5ms distribution normal",
		},
		{
			name: "small delay (≤19ms) → auto-jitter rounds to zero, plain delay",
			p:    NetemParams{DelayMs: 18},
			want: "delay 18ms",
		},
		{
			name: "explicit jitter + correlation → delay TIME JITTER CORR distribution normal",
			p:    NetemParams{DelayMs: 150, JitterMs: 80, JitterCorrelationPct: 25},
			want: "delay 150ms 80ms 25% distribution normal",
		},
		{
			name: "explicit jitter, no correlation → omit corr term",
			p:    NetemParams{DelayMs: 40, JitterMs: 20},
			want: "delay 40ms 20ms distribution normal",
		},
		{
			name: "uniform loss (no correlation) — legacy",
			p:    NetemParams{LossPct: 1},
			want: "loss 1.00%",
		},
		{
			name: "CORRELATED loss — bursty, the #826 acceptance shape",
			p:    NetemParams{LossPct: 3, LossCorrelationPct: 80},
			want: "loss 3.00% 80%",
		},
		{
			name: "mobile-poor profile: delay+jitter+corr AND correlated loss",
			p:    NetemParams{DelayMs: 150, LossPct: 3, JitterMs: 80, LossCorrelationPct: 50, JitterCorrelationPct: 25},
			want: "delay 150ms 80ms 25% distribution normal loss 3.00% 50%",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(netemImpairmentArgs(tc.p), " ")
			if got != tc.want {
				t.Errorf("netemImpairmentArgs(%+v)\n  got:  %q\n  want: %q", tc.p, got, tc.want)
			}
		})
	}
}

// TestNetemCorrelatedLossIsNotUniform is the explicit guard for caveat 1:
// when a loss correlation is set, the argument list must NOT be the bare
// `loss N%` uniform form.
func TestNetemCorrelatedLossIsNotUniform(t *testing.T) {
	args := strings.Join(netemImpairmentArgs(NetemParams{LossPct: 3, LossCorrelationPct: 80}), " ")
	if !strings.Contains(args, "loss 3.00% 80%") {
		t.Fatalf("correlated loss not expressed; got %q", args)
	}
	if args == "loss 3.00%" {
		t.Fatalf("loss is uniform (no correlation term) — violates #826 caveat 1")
	}
}
