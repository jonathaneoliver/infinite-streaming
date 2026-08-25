package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// A/B "arm" primitives (#variant-thin). An A/B groups N players for shared
// bandwidth control but gives each a different *initial treatment* — e.g. one
// on the full ladder, another on "every other" rung. The variant ladder is a
// property of the CONTENT, so we read it from the catalogue (/api/content
// variants[], added in go-upload) rather than parsing a playlist or priming a
// proxy session — the catalogue is fault-immune and session-independent.
//
// The keep-set is expressed in RESOLUTION terms ("640x360"); the proxy's
// allowed_variants now matches by resolution (go-proxy variantAllowed), so the
// same keep-set works across 2s/6s/LL whose served URIs differ.

// Variant mirrors one rung of the /api/content `variants[]` ladder (go-upload
// util.Variant). Resolution-keyed; AverageBandwidth is 0 when the source
// playlist omits AVERAGE-BANDWIDTH.
type Variant struct {
	Resolution       string `json:"resolution"`
	Height           int    `json:"height"`
	Bandwidth        int    `json:"bandwidth"`
	AverageBandwidth int    `json:"average_bandwidth"`
}

// FetchContentVariants reads /api/content and returns the variant ladder for
// the named clip (the catalogue `name`, e.g. "insane_new_p200_h264"). Returns
// an error if the clip isn't found or carries no variants[]. Read straight from
// go-upload — never through a proxy session — so faults/content-manipulation on
// any session can't corrupt the list.
func FetchContentVariants(ctx context.Context, contentName string) ([]Variant, error) {
	u, err := url.Parse(strings.TrimRight(bootstrapBaseURL(), "/"))
	if err != nil {
		return nil, fmt.Errorf("FetchContentVariants: parse base URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s://%s/api/content", u.Scheme, u.Host), nil)
	if err != nil {
		return nil, err
	}
	resp, err := bootstrapHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("FetchContentVariants GET /api/content: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("FetchContentVariants: read body: %w", err)
	}
	var items []struct {
		Name     string    `json:"name"`
		Variants []Variant `json:"variants"`
	}
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, fmt.Errorf("FetchContentVariants: decode: %w", err)
	}
	for _, it := range items {
		if it.Name == contentName {
			if len(it.Variants) == 0 {
				return nil, fmt.Errorf("FetchContentVariants: %q has no variants[] (older go-upload?)", contentName)
			}
			return it.Variants, nil
		}
	}
	return nil, fmt.Errorf("FetchContentVariants: clip %q not in /api/content", contentName)
}

// ApplyKeep resolves a variant-keep rule into a RESOLUTION keep-set ("640x360",
// …) suitable for content.allowed_variants. Returns nil for "all"/"" (no
// thinning ⇒ full ladder). Rules:
//
//   - "every_other": on the bandwidth-sorted ladder keep indices 0,2,4,… PLUS
//     the last, so floor AND ceiling survive — identical to the dashboard's
//     keepEveryOther() (ContentManipulation.vue), which on a geometric ladder
//     yields the classic 2× subset.
//
// Unknown rules fall through to "keep all" (nil) so a typo degrades to no-op
// rather than an empty (all-stripped) ladder.
func ApplyKeep(rule string, variants []Variant) []string {
	asc := append([]Variant(nil), variants...)
	sort.Slice(asc, func(i, j int) bool { return asc[i].Bandwidth < asc[j].Bandwidth })
	switch rule {
	case "every_other":
		n := len(asc)
		keep := make([]string, 0, (n+1)/2+1)
		for i, v := range asc {
			if i%2 == 0 || i == n-1 {
				keep = append(keep, v.Resolution)
			}
		}
		return keep
	default: // "", "all", or unrecognised ⇒ no thinning
		return nil
	}
}

// ContentAllowedVariantsConfig builds the config-on-connect BootstrapConfig keys
// that pin content.allowed_variants to a resolution keep-set, applied per-member
// at ALLOCATE (before the first master fetch). Empty keep-set ⇒ nil (full
// ladder). The keys ride the existing proxy.<path>[i] array nesting
// ConfigureOnConnect already forwards — no proxy change needed.
func ContentAllowedVariantsConfig(allowedVariants []string) BootstrapConfig {
	if len(allowedVariants) == 0 {
		return nil
	}
	cfg := BootstrapConfig{}
	for i, v := range allowedVariants {
		cfg[fmt.Sprintf("content.allowed_variants[%d]", i)] = v
	}
	return cfg
}
