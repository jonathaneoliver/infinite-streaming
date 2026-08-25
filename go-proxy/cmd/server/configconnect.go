package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Config-on-connect (#712): parse `proxy.*` URL args on the bootstrap request
// into a JSON-Merge-Patch-shaped map[string]any, so the session can be
// materialized atomically at allocation time and the patch fed through the
// SAME translator the PATCH API uses (v2server.ApplyConfigPatch). The URL-arg
// vocabulary therefore rides the API model and cannot drift from it.
//
// Encoding (bracket/dotted notation, the de-facto qs/Rails/PHP convention,
// also what go-playground/form and gorilla/schema implement):
//
//	proxy.shape.rate_mbps=2.5
//	proxy.fault_rules[0].type=corrupted
//	proxy.fault_rules[0].filter.request_kind[1]=segment
//	proxy.cfg=<base64url(JSON)>            # full-fidelity escape hatch / base tier
//
// Tier precedence: proxy.cfg is the base; individual proxy.<path> args override
// it per-field. Bracket notation lives in the KEY, so values stay clean — no
// comma/equals dodging.

// proxyArgPrefix is the namespace every config-on-connect URL arg lives under.
const proxyArgPrefix = "proxy."

// appArgPrefix is the client-side config-on-connect namespace (#800). An
// `app.<field>` arg is sugar for `proxy.app_config.<field>` — it rides the same
// merge-patch tree and translator as proxy.*, but lands under the PlayerPatch
// `app_config` object the player reads back at its next play boundary. Kept as a
// distinct top-level prefix (not literal `proxy.app_config.`) so client vs
// server config stay visually separate on the bootstrap URL.
const appArgPrefix = "app."

// proxyCfgKey is the base64url(JSON) escape-hatch key (full PlayerPatch body).
const proxyCfgKey = "proxy.cfg"

// segmentRe splits a path segment into its base key and trailing bracket
// indices, e.g. `request_kind[1]` → ("request_kind", "[1]"), `filter` →
// ("filter", "").
var segmentRe = regexp.MustCompile(`^([^\[\]]+)((?:\[[^\[\]]*\])*)$`)

// bracketRe extracts each `[...]` index body from a segment's bracket suffix.
var bracketRe = regexp.MustCompile(`\[([^\]]*)\]`)

// pathStep is one hop in a parsed arg path — either a map key or an array index.
type pathStep struct {
	key     string
	index   int
	isIndex bool
}

// parseProxyArgs reads every `proxy.*` query arg into a nested merge-patch map.
// Returns hasProxy=false (and a nil patch) when no proxy.* args are present, so
// the bootstrap handler can skip materialization entirely. A malformed arg
// (bad base64, bad JSON, non-numeric array index, path type conflict) returns
// an error so the caller can reject the request with 400 rather than allocate a
// half-configured session.
func parseProxyArgs(q url.Values) (patch map[string]any, hasProxy bool, err error) {
	patch = map[string]any{}

	// Tier 1 (base): proxy.cfg = base64url(JSON) of a full PlayerPatch.
	if cfg := q.Get(proxyCfgKey); cfg != "" {
		hasProxy = true
		raw, derr := decodeBase64URL(cfg)
		if derr != nil {
			return nil, false, fmt.Errorf("proxy.cfg base64url decode: %w", derr)
		}
		if jerr := json.Unmarshal(raw, &patch); jerr != nil {
			return nil, false, fmt.Errorf("proxy.cfg JSON unmarshal: %w", jerr)
		}
		if patch == nil { // explicit JSON null
			patch = map[string]any{}
		}
	}

	// Tier 2 (overrides): individual proxy.<path> args (per-field overlaying
	// cfg) plus app.<field> client-config args (#800), both projected onto the
	// same merge-patch tree.
	for key, vals := range q {
		if len(vals) == 0 {
			continue
		}
		var path string
		switch {
		case key == proxyCfgKey:
			continue
		case strings.HasPrefix(key, proxyArgPrefix):
			path = strings.TrimPrefix(key, proxyArgPrefix)
		case strings.HasPrefix(key, appArgPrefix):
			// app.<field> → app_config.<field> in the PlayerPatch tree (#800).
			path = "app_config." + strings.TrimPrefix(key, appArgPrefix)
		default:
			continue
		}
		hasProxy = true
		steps, perr := parseArgPath(path)
		if perr != nil {
			return nil, false, fmt.Errorf("config arg %q: %w", key, perr)
		}
		// Last value wins for a repeated key (arrays are expressed via [i],
		// not repetition, so this only matters for accidental duplicates).
		if _, serr := setPath(patch, steps, coerceURLValue(vals[len(vals)-1])); serr != nil {
			return nil, false, fmt.Errorf("proxy arg %q: %w", key, serr)
		}
	}

	if !hasProxy {
		return nil, false, nil
	}
	return patch, true, nil
}

// parseArgPath tokenizes a dotted/bracketed arg path into ordered steps.
//
// `labels` is special-cased: v2 label keys may themselves contain `.` / `/` /
// `-` (`[a-z][a-z0-9_./-]{0,62}`), so everything after `labels.` is taken as a
// single key verbatim rather than split further. Dotted label keys via the flat
// form would otherwise be mis-nested; use proxy.cfg for anything exotic.
func parseArgPath(path string) ([]pathStep, error) {
	if path == "" {
		return nil, fmt.Errorf("empty path")
	}
	if rest, ok := strings.CutPrefix(path, "labels."); ok {
		if rest == "" {
			return nil, fmt.Errorf("empty label key")
		}
		return []pathStep{{key: "labels"}, {key: rest}}, nil
	}

	var steps []pathStep
	for _, seg := range strings.Split(path, ".") {
		m := segmentRe.FindStringSubmatch(seg)
		if m == nil {
			return nil, fmt.Errorf("malformed segment %q", seg)
		}
		steps = append(steps, pathStep{key: m[1]})
		for _, b := range bracketRe.FindAllStringSubmatch(m[2], -1) {
			idx, err := strconv.Atoi(b[1])
			if err != nil || idx < 0 {
				return nil, fmt.Errorf("non-numeric array index %q in %q", b[1], seg)
			}
			steps = append(steps, pathStep{index: idx, isIndex: true})
		}
	}
	return steps, nil
}

// setPath walks/creates the nested container described by steps and writes
// value at the leaf, returning the (possibly newly created) container so the
// caller can reassign it (slices grow by value in Go). Type conflicts — a path
// that expects an object where an array already exists, or vice versa — error.
func setPath(cur any, steps []pathStep, value any) (any, error) {
	if len(steps) == 0 {
		return value, nil
	}
	head := steps[0]
	if head.isIndex {
		arr, ok := cur.([]any)
		if cur == nil {
			arr = []any{}
		} else if !ok {
			return nil, fmt.Errorf("path type conflict: array expected at index %d", head.index)
		}
		for len(arr) <= head.index {
			arr = append(arr, nil)
		}
		child, err := setPath(arr[head.index], steps[1:], value)
		if err != nil {
			return nil, err
		}
		arr[head.index] = child
		return arr, nil
	}
	m, ok := cur.(map[string]any)
	if cur == nil {
		m = map[string]any{}
	} else if !ok {
		return nil, fmt.Errorf("path type conflict: object expected at key %q", head.key)
	}
	child, err := setPath(m[head.key], steps[1:], value)
	if err != nil {
		return nil, err
	}
	m[head.key] = child
	return m, nil
}

// coerceURLValue maps a raw string URL value to the JSON type the translator
// expects: "true"/"false" → bool, a finite number → float64 (json.Unmarshal's
// numeric type), everything else stays a string. This matches every field in
// the #712 vocab — rate_mbps→float, strip_resolution→bool, type→string,
// resolutions[0]="1080p"→string. Non-finite tokens ("inf"/"nan") stay strings
// so they can't slip through as numbers.
func coerceURLValue(s string) any {
	switch s {
	case "true":
		return true
	case "false":
		return false
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
		return f
	}
	return s
}

// decodeBase64URL accepts base64url with or without padding.
func decodeBase64URL(s string) ([]byte, error) {
	if raw, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

// stripProxyArgs rebuilds a raw query string with every proxy.* and app.* arg
// removed, preserving the original order and verbatim encoding of the kept pairs
// (player_id, play_id, any passthrough). Used so the 302 redirect to the
// session port carries no config args — config has already been materialized,
// and a clean redirect URL keeps proxy.* off the session port and out of the
// child-request space.
func stripProxyArgs(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	parts := strings.Split(rawQuery, "&")
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		name := p
		if eq := strings.IndexByte(p, '='); eq >= 0 {
			name = p[:eq]
		}
		if decoded, err := url.QueryUnescape(name); err == nil {
			name = decoded
		}
		if strings.HasPrefix(name, proxyArgPrefix) || strings.HasPrefix(name, appArgPrefix) {
			continue
		}
		kept = append(kept, p)
	}
	return strings.Join(kept, "&")
}
