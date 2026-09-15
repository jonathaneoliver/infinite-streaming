package app

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// ResultTracker records what an encode actually did, parsed from the ladder
// script's own output as the job runs. A job's config is only the request: it
// can say hardware encoding wasn't ruled out, but not which encoder ran, and
// on a Linux host without VideoToolbox every encode is software regardless
// (#1017).
//
// Result shape (stored as the job's `result` JSON):
//
//	{
//	  "encoders": {"h264": "libx264 (software)", "hevc": "VideoToolbox (hardware) - 13x faster"},
//	  "padding":  "none" | "applied",
//	  "padding_video_s": 1.234   // only when applied
//	}
type ResultTracker struct {
	mu   sync.Mutex
	jobs map[string]map[string]interface{}
}

func NewResultTracker() *ResultTracker {
	return &ResultTracker{jobs: make(map[string]map[string]interface{})}
}

var (
	ansiEscapeRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	// "✓ HEVC encoder: libx265 (software)" / "✓ H.264 encoder: VideoToolbox (hardware) - 5x faster"
	encoderLineRE = regexp.MustCompile(`\b(H\.264|HEVC|AV1) encoder: (.+?)\s*$`)
	// "Video padding: 1.234s (118.766s → 120.000s)"
	videoPaddingRE = regexp.MustCompile(`Video padding: ([0-9]+(?:\.[0-9]+)?)s`)
)

var encoderCodecKey = map[string]string{"H.264": "h264", "HEVC": "hevc", "AV1": "av1"}

// Parse inspects one line of script output. When the line adds or changes
// something about the job's result it returns a copy of the full result to
// persist; otherwise nil.
func (r *ResultTracker) Parse(jobID, line string) map[string]interface{} {
	clean := ansiEscapeRE.ReplaceAllString(line, "")

	r.mu.Lock()
	defer r.mu.Unlock()
	res := r.jobs[jobID]
	if res == nil {
		res = map[string]interface{}{}
		r.jobs[jobID] = res
	}
	changed := false

	if m := encoderLineRE.FindStringSubmatch(clean); m != nil {
		encoders, _ := res["encoders"].(map[string]interface{})
		if encoders == nil {
			encoders = map[string]interface{}{}
			res["encoders"] = encoders
		}
		key := encoderCodecKey[m[1]]
		if encoders[key] != m[2] {
			encoders[key] = m[2]
			changed = true
		}
	}

	switch {
	case strings.Contains(clean, "Padding disabled"):
		if res["padding"] != "none" {
			res["padding"] = "none"
			delete(res, "padding_video_s")
			changed = true
		}
	case videoPaddingRE.MatchString(clean):
		m := videoPaddingRE.FindStringSubmatch(clean)
		if secs, err := strconv.ParseFloat(m[1], 64); err == nil {
			mode := "applied"
			if secs == 0 {
				mode = "none" // already on a segment boundary; nothing to pad
			}
			if res["padding"] != mode || res["padding_video_s"] != secs {
				res["padding"] = mode
				if mode == "applied" {
					res["padding_video_s"] = secs
				} else {
					delete(res, "padding_video_s")
				}
				changed = true
			}
		}
	}

	if !changed {
		return nil
	}
	return copyResult(res)
}

// Forget drops a finished job's in-memory state.
func (r *ResultTracker) Forget(jobID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.jobs, jobID)
}

func copyResult(src map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(src))
	for k, v := range src {
		if inner, ok := v.(map[string]interface{}); ok {
			cp := make(map[string]interface{}, len(inner))
			for ik, iv := range inner {
				cp[ik] = iv
			}
			out[k] = cp
			continue
		}
		out[k] = v
	}
	return out
}
