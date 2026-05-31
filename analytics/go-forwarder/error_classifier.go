// #550 Phase 2 — error-code classification.
//
// Maps iOS-emitted (error_domain, error_code, last_failed_kind)
// tuples to the controlled `playback_reason` vocab. Forwarder calls
// classifyError() when stamping a session_end row with a failure
// status, or when promoting an `error` event row's classification
// for label stamping.
//
// Three-tier lookup matches the design captured in #550:
//   t1: (domain, code, kind) — most specific
//   t2: (domain, code, "")   — code-specific, kind-agnostic
//   t3: domain → fallback
// Misses return "unknown" and the row gets a `qoe_unknown_error`
// label so the table can grow in response to observed errors.
//
// Seed entries cover the canonical Apple error codes documented in
// the design. New mappings are additive; co-locate one assertion per
// entry in error_classifier_test.go.

package main

type errorKey struct {
	domain string
	code   int32
	kind   string
}

var t1ErrorMap = map[errorKey]string{
	// HTTP layer — request_kind disambiguates manifest vs segment.
	{"http", 404, "master_manifest"}: "manifest_404",
	{"http", 404, "playlist"}:        "manifest_404",
	{"http", 404, "media_segment"}:   "segment_404",
	{"http", 404, "init_segment"}:    "segment_404",
	{"http", 404, "drm_key"}:         "drm_license_failed",
	// 5xx — generic per-kind. Specific codes handled by classifyHTTPStatus.
}

var t2ErrorMap = map[errorKey]string{
	// CoreMediaErrorDomain — most DRM + decoder paths.
	{"CoreMediaErrorDomain", -12318, ""}: "drm_license_failed",
	{"CoreMediaErrorDomain", -12642, ""}: "drm_license_failed", // SPC denied / CKC rejected
	{"CoreMediaErrorDomain", -11819, ""}: "decoder_init",
	{"CoreMediaErrorDomain", -12158, ""}: "decoder_runtime",
	{"CoreMediaErrorDomain", -11829, ""}: "decoder_init",
	{"CoreMediaErrorDomain", -17383, ""}: "unknown", // CMCD+FairPlay (FB17086130) — flag separately

	// NSURLErrorDomain — network layer.
	{"NSURLErrorDomain", -1001, ""}: "network_timeout",
	{"NSURLErrorDomain", -1003, ""}: "network_disconnected",
	{"NSURLErrorDomain", -1004, ""}: "network_disconnected",
	{"NSURLErrorDomain", -1005, ""}: "network_disconnected",
	{"NSURLErrorDomain", -1009, ""}: "network_disconnected",
	{"NSURLErrorDomain", -1100, ""}: "segment_404",
	{"NSURLErrorDomain", -1200, ""}: "network_disconnected",
	{"NSURLErrorDomain", -999, ""}:  "user_initiated",

	// AVFoundationErrorDomain.
	{"AVFoundationErrorDomain", -11824, ""}: "drm_license_failed",
	{"AVFoundationErrorDomain", -11829, ""}: "bitrate_unmet",
	{"AVFoundationErrorDomain", -11862, ""}: "drm_license_failed",
}

var t3ErrorFallback = map[string]string{
	"CoreMediaErrorDomain":    "decoder_runtime",
	"NSURLErrorDomain":        "network_timeout",
	"AVFoundationErrorDomain": "unknown",
	"http":                    "unknown",
	"nft":                     "network_disconnected",
}

// classifyError returns the controlled `playback_reason` vocab value
// for a given (domain, code, kind) tuple. Returns "unknown" on miss
// so the forwarder can stamp a `qoe_unknown_error` label and the
// mapping table can be grown over time in response to observed
// errors. Empty domain → "unknown" (no signal to classify on).
func classifyError(domain string, code int32, kind string) string {
	if domain == "" {
		return "unknown"
	}
	// HTTP code-range handling (sparse: only specific kind-tuples
	// land in t1; the broader 5xx range delegates to classifyHTTPStatus).
	if domain == "http" {
		if r := classifyHTTPStatus(code, kind); r != "" {
			return r
		}
	}
	if r, ok := t1ErrorMap[errorKey{domain, code, kind}]; ok {
		return r
	}
	if r, ok := t2ErrorMap[errorKey{domain, code, ""}]; ok {
		return r
	}
	if r, ok := t3ErrorFallback[domain]; ok {
		return r
	}
	return "unknown"
}

// classifyHTTPStatus handles HTTP code ranges (manifest vs segment
// distinguished by request_kind).
func classifyHTTPStatus(code int32, kind string) string {
	if code == 404 {
		switch kind {
		case "media_segment", "init_segment":
			return "segment_404"
		case "drm_key":
			return "drm_license_failed"
		default:
			return "manifest_404"
		}
	}
	if code >= 500 && code < 600 {
		switch kind {
		case "media_segment", "init_segment":
			return "segment_5xx"
		case "drm_key":
			return "drm_license_failed"
		default:
			return "manifest_5xx"
		}
	}
	if code >= 400 && code < 500 {
		// Other 4xx (auth, bad request) — rare on streaming traffic;
		// folds into the generic manifest_4xx bucket.
		return "manifest_4xx"
	}
	return ""
}
