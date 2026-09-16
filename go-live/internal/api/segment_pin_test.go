package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSegmentPin(t *testing.T) {
	cases := []struct {
		name string
		want int
	}{
		// Pinned: natively encoded at one length, never repackaged.
		{"fpv_p200_h264_1s", 1},
		{"fpv_p200_h264_2s", 2},
		{"fpv_p200_h264_6s", 6},
		{"insane_fpv_shots_hydrofoil_windsurfing_p200_h264_6s", 6},

		// _xs is the repackaging source, not a pin.
		{"fpv_p200_h264_xs", 0},
		{"insane_fpv_shots_hydrofoil_windsurfing_p200_h264_xs", 0},

		// Unsuffixed legacy content keeps repackaging.
		{"bucks_bunny_p200_h264", 0},
		{"redbull_p200_av1", 0},

		// A double underscore still ends in _6s and is treated as pinned —
		// the name that prompted the rename, kept here so the behaviour is
		// pinned down rather than accidental.
		{"insane_fpv_shots_hydrofoil_windsurfing_p200_h264__6s", 6},

		// Only 1/2/6 are real variants; nothing else is a pin.
		{"clip_p200_h264_3s", 0},
		{"clip_p200_h264_10s", 0},
		{"clip_p200_h264_0s", 0},

		// The marker must be a suffix. A mid-name _6s is part of the title.
		{"fpv_6s_p200_h264", 0},
		{"my_2s_clip_p200_h264", 0},

		// Case-insensitive, matching the codec pattern's behaviour.
		{"clip_p200_h264_6S", 6},

		{"", 0},
	}
	for _, c := range cases {
		if got := segmentPin(c.name); got != c.want {
			t.Errorf("segmentPin(%q) = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestRejectPinnedVariant(t *testing.T) {
	cases := []struct {
		desc       string
		content    string
		variant    string
		duration   int
		llMode     bool
		wantReject bool
	}{
		{"pinned clip asked for its own length", "fpv_p200_h264_1s", "1s", 1, false, false},
		{"pinned clip asked for another length", "fpv_p200_h264_1s", "2s", 2, false, true},
		{"pinned 6s asked for 1s", "fpv_p200_h264_6s", "1s", 1, false, true},

		// LL is partial-segment availability, not a segment length, so a pin
		// must not suppress it.
		{"pinned clip asked for LL", "fpv_p200_h264_1s", "ll", 6, true, false},
		{"pinned clip LL by variant name", "fpv_p200_h264_6s", "ll", 6, false, false},

		// Unpinned content is never refused.
		{"xs clip asked for 2s", "fpv_p200_h264_xs", "2s", 2, false, false},
		{"legacy clip asked for 1s", "bucks_bunny_p200_h264", "1s", 1, false, false},
	}

	for _, c := range cases {
		w := httptest.NewRecorder()
		got := rejectPinnedVariant(w, c.content, c.variant, c.duration, c.llMode)
		if got != c.wantReject {
			t.Errorf("%s: rejectPinnedVariant = %v, want %v", c.desc, got, c.wantReject)
			continue
		}
		if c.wantReject {
			if w.Code != http.StatusNotFound {
				t.Errorf("%s: status = %d, want 404", c.desc, w.Code)
			}
			// The reason must name the clip and its pin, or the 404 is
			// indistinguishable from "content not found".
			body := w.Body.String()
			if !contains(body, c.content) || !contains(body, "pinned") {
				t.Errorf("%s: body %q does not explain the pin", c.desc, body)
			}
		} else if w.Code != http.StatusOK {
			t.Errorf("%s: wrote status %d when it should not have responded", c.desc, w.Code)
		}
	}
}

func TestRejectPinnedDurationLabel(t *testing.T) {
	cases := []struct {
		content    string
		label      string
		wantReject bool
	}{
		{"fpv_p200_h264_2s", "2s", false},
		{"fpv_p200_h264_2s", "6s", true},
		{"fpv_p200_h264_xs", "6s", false},
		{"bucks_bunny_p200_h264", "1s", false},
		// An unrecognised label is not this guard's business.
		{"fpv_p200_h264_2s", "banana", false},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		if got := rejectPinnedDurationLabel(w, c.content, c.label); got != c.wantReject {
			t.Errorf("rejectPinnedDurationLabel(%q, %q) = %v, want %v", c.content, c.label, got, c.wantReject)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
