package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
)

// allowed_variants spec resolution (#772). A config-class experiment stores a
// human-meaningful ladder spec like "drop-top-rung"; the proxy's
// allowed_variants is a concrete keep-set matched by URI OR resolution
// (variantAllowed in go-proxy). So before applying it we resolve the spec
// against the content's actual master playlist into resolution heights (e.g.
// ["1440","1080","720","480","360"]) — height matching is segment-duration
// agnostic. Without this, an unresolved spec reached the proxy verbatim and
// filtered the ladder to nothing → a spurious startup failure.

var (
	reMasterBandwidth  = regexp.MustCompile(`(?:^|,)BANDWIDTH=(\d+)`)
	reMasterResolution = regexp.MustCompile(`(?:^|,)RESOLUTION=(\d+)x(\d+)`)
)

type masterRung struct {
	bandwidth int
	height    int
	uri       string
}

// resolveAllowedVariants turns an allowed_variants spec into a concrete
// keep-set the proxy understands. An explicit comma-separated list (URIs or
// resolutions) passes through unchanged. Ladder specs — "drop-top-rung",
// "drop-top-<N>", "keep-bottom-<N>" — are resolved against the live master
// ladder and returned as resolution heights. A bare single token (no comma, not
// a known spec) is returned as-is so callers can pass a one-off resolution.
func resolveAllowedVariants(ctx context.Context, client *api.Client, content, spec string) ([]string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	if strings.Contains(spec, ",") {
		return splitTrimNonEmpty(spec), nil
	}

	// A single literal token (e.g. "1080p" or one URI) — pass through.
	if !isLadderSpec(spec) {
		return []string{spec}, nil
	}

	rungs, err := fetchMasterRungs(ctx, client, content)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", spec, err)
	}
	return ladderKeepSet(spec, rungs)
}

// isLadderSpec reports whether a spec needs the master ladder to resolve.
func isLadderSpec(spec string) bool {
	return spec == "drop-top-rung" ||
		spec == "alternating_variants" ||
		strings.HasPrefix(spec, "drop-top-") ||
		strings.HasPrefix(spec, "keep-bottom-")
}

// ladderKeepSet resolves a ladder spec against the rungs into a keep-set of
// resolution heights (matched by the proxy's variantAllowed). Pure — the
// network fetch is the caller's job — so the drop/keep arithmetic is testable.
func ladderKeepSet(spec string, rungs []masterRung) ([]string, error) {
	if len(rungs) == 0 {
		return nil, fmt.Errorf("resolve %q: no variants in master", spec)
	}
	drop, keepBottom := 0, 0
	alternating := false
	switch {
	case spec == "drop-top-rung":
		drop = 1
	case spec == "alternating_variants":
		alternating = true
	case strings.HasPrefix(spec, "drop-top-"):
		n, err := strconv.Atoi(strings.TrimPrefix(spec, "drop-top-"))
		if err != nil || n < 1 {
			return nil, fmt.Errorf("allowed_variants %q: bad drop-top count", spec)
		}
		drop = n
	case strings.HasPrefix(spec, "keep-bottom-"):
		n, err := strconv.Atoi(strings.TrimPrefix(spec, "keep-bottom-"))
		if err != nil || n < 1 {
			return nil, fmt.Errorf("allowed_variants %q: bad keep-bottom count", spec)
		}
		keepBottom = n
	default:
		return nil, fmt.Errorf("allowed_variants %q: not a ladder spec", spec)
	}

	// Descending by bandwidth: rungs[0] is the top rung.
	sort.Slice(rungs, func(i, j int) bool { return rungs[i].bandwidth > rungs[j].bandwidth })

	var kept []masterRung
	switch {
	case alternating:
		// Keep every 2nd rung on the bandwidth-sorted ladder — the #820
		// "keep every other (2x)" thinning (11 → 6). Keeps the top + bottom.
		for i := 0; i < len(rungs); i += 2 {
			kept = append(kept, rungs[i])
		}
	case keepBottom > 0:
		if keepBottom >= len(rungs) {
			kept = rungs
		} else {
			kept = rungs[len(rungs)-keepBottom:]
		}
	default:
		if drop >= len(rungs) {
			return nil, fmt.Errorf("resolve %q: would drop all %d rungs", spec, len(rungs))
		}
		kept = rungs[drop:]
	}

	// Express the keep-set as resolution heights (matched by variantAllowed),
	// falling back to the URI for a rung with no RESOLUTION.
	out := make([]string, 0, len(kept))
	seen := map[string]bool{}
	for _, r := range kept {
		var key string
		if r.height > 0 {
			key = strconv.Itoa(r.height)
		} else if r.uri != "" {
			key = r.uri
		} else {
			continue
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out, nil
}

// fetchMasterRungs GETs the content's master playlist directly off the origin
// (no proxy session needed) and parses each variant's bandwidth + resolution.
func fetchMasterRungs(ctx context.Context, client *api.Client, content string) ([]masterRung, error) {
	url := fmt.Sprintf("%s/go-live/%s/master_6s.m3u8", strings.TrimRight(client.BaseURL, "/"), content)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("GET %s: %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return parseMasterRungs(string(body)), nil
}

func parseMasterRungs(body string) []masterRung {
	var out []masterRung
	lines := strings.Split(body, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(strings.ToUpper(line), "#EXT-X-STREAM-INF:") {
			continue
		}
		// Match against the attributes only (after the "…STREAM-INF:" prefix), so
		// the `(?:^|,)BANDWIDTH=` anchor sees BANDWIDTH at the start and doesn't
		// confuse it with AVERAGE-BANDWIDTH.
		attrs := line[strings.IndexByte(line, ':')+1:]
		r := masterRung{}
		if m := reMasterBandwidth.FindStringSubmatch(attrs); m != nil {
			r.bandwidth, _ = strconv.Atoi(m[1])
		}
		if m := reMasterResolution.FindStringSubmatch(attrs); m != nil {
			r.height, _ = strconv.Atoi(m[2])
		}
		for j := i + 1; j < len(lines); j++ {
			uri := strings.TrimSpace(lines[j])
			if uri == "" || strings.HasPrefix(uri, "#") {
				continue
			}
			r.uri = uri
			i = j
			break
		}
		if r.bandwidth > 0 || r.uri != "" {
			out = append(out, r)
		}
	}
	return out
}
