package api

import (
	"fmt"
	"net/http"
	"regexp"
)

// segmentPinPattern matches a trailing `_1s` / `_2s` / `_6s` on a content name.
//
// Such a clip is encoded natively at that one segment length and is served only
// at it — we do not repackage it into the other lengths. `_xs` (and an
// unsuffixed name) carries no pin and keeps the normal repackaging behaviour,
// where a single source is recomposed into 1s/2s/6s and LL.
//
// Mirrored in go-upload/internal/util/content.go: these are separate Go modules
// with no shared package, so the rule is stated in both. The catalogue there
// must not advertise a length this file will refuse.
var segmentPinPattern = regexp.MustCompile(`(?i)_(1|2|6)s$`)

// segmentPin returns the segment length a content name is pinned to, or 0 when
// it carries no pin.
func segmentPin(content string) int {
	m := segmentPinPattern.FindStringSubmatch(content)
	if m == nil {
		return 0
	}
	switch m[1] {
	case "1":
		return 1
	case "2":
		return 2
	case "6":
		return 6
	}
	return 0
}

// rejectPinnedVariant reports whether a request for `duration` on `content`
// should be refused, writing a 404 naming the pin when so.
//
// A pinned clip serves exactly one segment length. Quietly serving its native
// length under another name would hand a caller data that is not what it asked
// for — the same silent-wrong-answer failure the DASH byte-range path used to
// have — so an explicit 404 is the safer contract. LL is exempt: it is
// partial-segment availability rather than a segment length, and a natively
// encoded clip carrying partials can still be served low-latency.
func rejectPinnedVariant(w http.ResponseWriter, content, variant string, duration int, llMode bool) bool {
	pin := segmentPin(content)
	if pin == 0 || llMode || variant == "ll" {
		return false
	}
	if duration == pin {
		return false
	}
	msg := fmt.Sprintf(
		"%s is pinned to %ds segments and is not repackaged; requested %s. Available: %ds and LL.",
		content, pin, variant, pin,
	)
	logf("[GO-LIVE][PIN] refused content=%s pin=%ds requested=%s\n", content, pin, variant)
	http.Error(w, msg, http.StatusNotFound)
	return true
}

// rejectPinnedDurationLabel is the HLS-route form of rejectPinnedVariant, where
// the duration arrives as the path label "1s" / "2s" / "6s". These routes never
// carry LL, which has its own unsuffixed route.
func rejectPinnedDurationLabel(w http.ResponseWriter, content, label string) bool {
	var duration int
	switch label {
	case "1s":
		duration = 1
	case "2s":
		duration = 2
	case "6s":
		duration = 6
	default:
		return false
	}
	return rejectPinnedVariant(w, content, label, duration, false)
}
