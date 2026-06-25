package sweep

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Named link profiles (#826). Each resolves to a Shape so a matrix spec (or the
// `harness shape --profile` CLI) can say `proxy.shape: home` instead of spelling
// out delay/loss/jitter. Conventions:
//
//   - Our four hand-tuned recipes (clean / home / mobile-good / mobile-poor) are
//     pure-impairment OVERLAYS — they set NO rate_mbps, so they layer on top of an
//     existing bandwidth spec without clobbering its cap. They carry jitter (the
//     LL killer) with a ~25% delay correlation and bursty loss correlation.
//   - The Apple Network Link Conditioner presets (nlc-*) DO set rate_mbps (the
//     downlink figure — asymmetric uplink is a follow-on, see #826). NLC has no
//     jitter term, so JitterMs stays 0 (the proxy still applies a tight ~5%
//     auto-jitter for ABR realism). nlc-high-latency-dns is intentionally absent:
//     it impairs DNS resolution, not the data path, so netem can't express it.
//     nlc-100-loss routes a total outage through netem 100% loss.
//
// Delays are one-way (observed RTT ≈ delay; only the proxy's egress is shaped).
var linkProfiles = map[string]Shape{
	// --- hand-tuned impairment overlays (no rate cap) ---
	"clean": {},
	"home": {
		DelayMs: fp(20), LossPct: fp(0.2), JitterMs: fp(5),
		LossCorrelationPct: fp(25), JitterCorrelationPct: fp(25),
	},
	"mobile-good": {
		DelayMs: fp(40), LossPct: fp(0.5), JitterMs: fp(20),
		LossCorrelationPct: fp(25), JitterCorrelationPct: fp(25),
	},
	"mobile-poor": {
		DelayMs: fp(150), LossPct: fp(3), JitterMs: fp(80),
		LossCorrelationPct: fp(50), JitterCorrelationPct: fp(25),
	},

	// --- Apple Network Link Conditioner presets (rate = downlink Mbps) ---
	// nlc-wifi-ac is Apple's 802.11ac preset (1100 Mbps DL = effectively
	// unimpaired); we cap it at our 100 Mbps test ceiling so no profile drives
	// the throughput cap above the slider's range.
	"nlc-wifi-ac": {RateMbps: fp(100), DelayMs: fp(1)},
	"nlc-wifi":    {RateMbps: fp(40), DelayMs: fp(1)},
	"nlc-lte":     {RateMbps: fp(50), DelayMs: fp(65)},
	"nlc-dsl":     {RateMbps: fp(2), DelayMs: fp(5)},
	"nlc-3g":      {RateMbps: fp(0.780), DelayMs: fp(100)},
	"nlc-edge":    {RateMbps: fp(0.240), DelayMs: fp(400)},
	"nlc-very-bad": {
		RateMbps: fp(1), DelayMs: fp(500), LossPct: fp(10), LossCorrelationPct: fp(25),
	},
	"nlc-100-loss": {LossPct: fp(100)},
}

// fp is a small float64-pointer helper for the profile table.
func fp(v float64) *float64 { return &v }

// ResolveLinkProfile returns a COPY of the named profile's Shape. The bool is
// false for an unknown name. Returning a copy keeps callers from mutating the
// shared table.
func ResolveLinkProfile(name string) (Shape, bool) {
	s, ok := linkProfiles[strings.TrimSpace(name)]
	if !ok {
		return Shape{}, false
	}
	return cloneProfileShape(s), true
}

// LinkProfileNames returns the known profile ids, sorted for stable error
// messages / help text.
func LinkProfileNames() []string {
	names := make([]string, 0, len(linkProfiles))
	for k := range linkProfiles {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// cloneProfileShape deep-copies the pointer fields so a resolved profile never
// shares backing storage with the registry.
func cloneProfileShape(s Shape) Shape {
	cp := s
	cp.RateMbps = clonef(s.RateMbps)
	cp.DelayMs = clonef(s.DelayMs)
	cp.LossPct = clonef(s.LossPct)
	cp.JitterMs = clonef(s.JitterMs)
	cp.LossCorrelationPct = clonef(s.LossCorrelationPct)
	cp.JitterCorrelationPct = clonef(s.JitterCorrelationPct)
	return cp
}

func clonef(v *float64) *float64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

// UnmarshalJSON lets `proxy.shape` accept EITHER a named link profile (a JSON
// string, e.g. `"home"`) OR the full object form. A string resolves through the
// profile registry; an object decodes strictly (DisallowUnknownFields) so a
// typo'd knob inside the block fails fast rather than silently dropping — the
// same guarantee the charmatrix layer relies on for the object axis form.
func (s *Shape) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var name string
		if err := json.Unmarshal(trimmed, &name); err != nil {
			return err
		}
		prof, ok := ResolveLinkProfile(name)
		if !ok {
			return fmt.Errorf("unknown link profile %q (known: %s)", name, strings.Join(LinkProfileNames(), ", "))
		}
		*s = prof
		return nil
	}
	// Object form. Decode through an alias type so this method isn't called
	// recursively, and keep strict unknown-field checking.
	type shapeAlias Shape
	var a shapeAlias
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return err
	}
	*s = Shape(a)
	return nil
}
