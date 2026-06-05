package ladder

import (
	"fmt"
	"sort"
)

// MinPeakSpacing is the smallest ratio allowed between two adjacent
// rungs' peak (BANDWIDTH) values before ValidateLadder flags them as too
// tightly spaced. Below ~1.5× a player can't tell the rungs apart and
// oscillates between them. Per #551 the real hazards are inversion,
// duplicate BANDWIDTH, and tight peak spacing — NOT avg→peak band
// overlap, which is normal capped-VBR and is never flagged.
const MinPeakSpacing = 1.5

// Hazard is one structural problem found in a published manifest ladder.
type Hazard struct {
	Kind   string // "inversion" | "duplicate_bandwidth" | "tight_spacing"
	Detail string
}

// ValidateLadder audits the published manifest ladder (not the shaped
// limit ladder) for the three #551 hazards. It considers only variants
// with a positive peak, sorts them ascending by peak, and reports:
//
//   - duplicate_bandwidth — two variants share a BANDWIDTH value
//   - tight_spacing       — adjacent peak ratio < MinPeakSpacing
//   - inversion           — average decreases while peak increases
//     (the avg column disagrees with the peak ordering)
//
// avg→peak band overlap across rungs is intentionally NOT reported.
// Returns nil when the ladder is clean.
func ValidateLadder(vs []Variant) []Hazard {
	list := make([]Variant, 0, len(vs))
	for _, v := range vs {
		if v.PeakBps > 0 {
			list = append(list, v)
		}
	}
	if len(list) < 2 {
		return nil
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].PeakBps < list[j].PeakBps })

	var hz []Hazard
	for i := 1; i < len(list); i++ {
		prev, cur := list[i-1], list[i]
		if cur.PeakBps == prev.PeakBps {
			hz = append(hz, Hazard{
				Kind:   "duplicate_bandwidth",
				Detail: fmt.Sprintf("%s and %s share BANDWIDTH=%d", label(prev), label(cur), cur.PeakBps),
			})
			continue // ratio is 1.0; the duplicate finding already covers it
		}
		if ratio := float64(cur.PeakBps) / float64(prev.PeakBps); ratio < MinPeakSpacing {
			hz = append(hz, Hazard{
				Kind:   "tight_spacing",
				Detail: fmt.Sprintf("%s→%s peak ratio %.2fx < %.2fx", label(prev), label(cur), ratio, MinPeakSpacing),
			})
		}
		if prev.AvgBps > 0 && cur.AvgBps > 0 && cur.AvgBps < prev.AvgBps {
			hz = append(hz, Hazard{
				Kind:   "inversion",
				Detail: fmt.Sprintf("%s avg %d < %s avg %d while peak rises", label(cur), cur.AvgBps, label(prev), prev.AvgBps),
			})
		}
	}
	return hz
}

func label(v Variant) string {
	if v.Resolution == "" {
		return fmt.Sprintf("bw=%d", v.PeakBps)
	}
	return v.Resolution
}
