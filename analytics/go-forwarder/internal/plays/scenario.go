package plays

import (
	"strconv"
	"strings"
)

// enrichScenario adds a nested "scenario" object to each play row — the
// run-IDENTITY of a play ("what it IS": test, platform, device, versions)
// as opposed to the event labels ("what HAPPENED during it"). #678 promotes
// this off the dashboard, which assembled it client-side in Sessions.vue,
// into the API contract so the Sessions list, the Session Viewer header, and
// chat tools all read one typed shape.
//
// Sourced as a hybrid, deliberately:
//   - typed columns (device/player/app/os/content) — authoritative, already
//     selected by the agg query; not lossy the way sanitized LowCardinality
//     labels are.
//   - testing= label tails (test/platform/run_id) — no typed column exists.
//
// The object is omitted entirely (rather than emitted as {}) when a play
// carries none of these — e.g. a pre-#550 row with no device taxonomy and no
// harness stamp.
func enrichScenario(rows []map[string]any) {
	for _, row := range rows {
		sc := map[string]any{}
		// Typed columns — authoritative.
		putIfStr(sc, "device_class", row["device_class"])
		putIfStr(sc, "device_model", row["device_model"])
		putIfStr(sc, "player_tech", row["player_tech"])
		putIfStr(sc, "player_tech_version", row["player_tech_version"])
		putIfStr(sc, "app_version", row["app_version"])
		putIfStr(sc, "content_id", row["content_id"])
		if os := joinOSVersion(row["os_version_major"], row["os_version_minor"]); os != "" {
			sc["os_version"] = os
		}
		// Harness identity — only present in the testing= label tier.
		test, platform, runID := scenarioLabelTails(row["label_histogram"])
		putIfStr(sc, "test", test)
		putIfStr(sc, "platform", platform)
		putIfStr(sc, "run_id", runID)
		// #679 — server-side identity: manifest variant (derived from the
		// master manifest the player loaded) + the go-live build that served
		// the play (from the X-Served-By header the proxy captured).
		if v := manifestVariant(asString(row["master_manifest_url"])); v != "" {
			sc["manifest_variant"] = v
		}
		if b := serverBuild(asString(row["served_by"])); b != "" {
			sc["server_build"] = b
		}

		if len(sc) == 0 {
			continue
		}
		row["scenario"] = sc
	}
}

// manifestVariant classifies the LL / 2s / 6s manifest from the master
// playlist URL the player loaded. go-live names the slower variants with a
// "2s"/"6s" path segment or filename suffix (e.g. .../2s/master.m3u8 or
// .../master_6s.m3u8); the low-latency variant has neither marker. Empty
// when no master URL was captured (can't tell).
func manifestVariant(masterURL string) string {
	if masterURL == "" {
		return ""
	}
	switch {
	case strings.Contains(masterURL, "/2s/") || strings.Contains(masterURL, "_2s"):
		return "2s"
	case strings.Contains(masterURL, "/6s/") || strings.Contains(masterURL, "_6s"):
		return "6s"
	default:
		return "ll"
	}
}

// serverBuild extracts the build tag from the X-Served-By header value.
// go-live emits "go-live/<build>" once compiled with -ldflags; a plain
// "go-live" (dev build, no ldflags) carries no build info, so returns "".
func serverBuild(servedBy string) string {
	const prefix = "go-live/"
	if strings.HasPrefix(servedBy, prefix) {
		return strings.TrimPrefix(servedBy, prefix)
	}
	return ""
}

// scenarioLabelTails pulls the test / platform / run_id values out of the
// testing= label tier of a play's label_histogram. The histogram arrives as
// the JSON-decoded ClickHouse groupArray((label, n)): []any of []any{string,
// number}. First occurrence wins (these keys are single-valued per play).
func scenarioLabelTails(hist any) (test, platform, runID string) {
	pairs, ok := hist.([]any)
	if !ok {
		return
	}
	for _, p := range pairs {
		pair, ok := p.([]any)
		if !ok || len(pair) == 0 {
			continue
		}
		label, ok := pair[0].(string)
		if !ok {
			continue
		}
		switch {
		case test == "" && strings.HasPrefix(label, "testing=test_"):
			test = strings.TrimPrefix(label, "testing=test_")
		case platform == "" && strings.HasPrefix(label, "testing=platform_"):
			platform = strings.TrimPrefix(label, "testing=platform_")
		case runID == "" && strings.HasPrefix(label, "testing=run_id_"):
			runID = strings.TrimPrefix(label, "testing=run_id_")
		}
	}
	return
}

// putIfStr sets m[key] only when v stringifies to a non-empty value, so the
// scenario object never carries empty-string noise.
func putIfStr(m map[string]any, key string, v any) {
	if s := asString(v); s != "" {
		m[key] = s
	}
}

// asString coerces a JSON-decoded ClickHouse cell to a string. Numbers come
// back as float64 (e.g. os_version_major as a UInt); integer-valued ones
// render without a spurious ".0".
func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

// joinOSVersion renders "major.minor" (or just "major" when minor is absent),
// empty when there's no major.
func joinOSVersion(maj, min any) string {
	ma := asString(maj)
	if ma == "" {
		return ""
	}
	if mi := asString(min); mi != "" {
		return ma + "." + mi
	}
	return ma
}
