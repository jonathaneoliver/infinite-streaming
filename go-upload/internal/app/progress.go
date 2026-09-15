package app

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
)

type ProgressTracker struct {
	mu       sync.Mutex
	trackers map[string]*EncodingProgressTracker
}

func NewProgressTracker() *ProgressTracker {
	return &ProgressTracker{trackers: make(map[string]*EncodingProgressTracker)}
}

// isFFmpegProgressLine reports whether line looks like a single key=value pair
// emitted by ffmpeg's `-progress pipe:1` output (e.g. "out_time=00:00:00.44",
// "frame=11", "progress=continue"). Used to filter the noisy progress stream
// out of human-readable encoding logs while still allowing the parser to read
// `out_time=` for smooth meter updates.
func isFFmpegProgressLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	eq := strings.Index(trimmed, "=")
	if eq <= 0 || eq == len(trimmed)-1 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := trimmed[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	// Value must not contain spaces (ffmpeg's normal stderr lines have multiple
	// space-separated key=value pairs and would fail this check).
	value := trimmed[eq+1:]
	return !strings.ContainsAny(value, " \t")
}

func (p *ProgressTracker) tracker(jobID string, cfg map[string]interface{}) *EncodingProgressTracker {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.trackers[jobID] == nil {
		p.trackers[jobID] = NewEncodingProgressTracker(cfg)
	}
	return p.trackers[jobID]
}

func (p *ProgressTracker) Parse(jobID string, line string, cfg map[string]interface{}) *int {
	t := p.tracker(jobID, cfg)
	if val := t.ParseLine(line); val != nil {
		return val
	}
	return nil
}

func (p *ProgressTracker) Message(jobID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t := p.trackers[jobID]; t != nil {
		if msg := t.ProgressMessage(); msg != "" && msg != t.lastBroadcast {
			t.lastBroadcast = msg
			return msg
		}
	}
	return ""
}

type EncodingProgressTracker struct {
	config             map[string]interface{}
	currentPhase       string
	currentVariantNum  int
	currentVariantInfo string
	totalVariants      int
	currentPass        int // 0 single-pass, 1 or 2 within a two-pass rung
	sourceDuration     float64
	lastProgress       int
	lastBroadcast      string
}

func NewEncodingProgressTracker(cfg map[string]interface{}) *EncodingProgressTracker {
	tracker := &EncodingProgressTracker{
		config:        cfg,
		totalVariants: estimateVariants(cfg),
		sourceDuration: func() float64 {
			if metadata, ok := cfg["metadata"].(map[string]interface{}); ok {
				if dur, ok := metadata["duration"].(float64); ok {
					return dur
				}
			}
			if val, ok := cfg["duration_limit"].(float64); ok {
				return val
			}
			if val, ok := cfg["duration_limit"].(int); ok {
				return float64(val)
			}
			return 100
		}(),
	}
	return tracker
}

func estimateVariants(cfg map[string]interface{}) int {
	codecSelection, _ := cfg["codec_selection"].(string)
	maxRes, _ := cfg["max_resolution"].(string)
	resTiers := map[string]int{
		"360p":  1,
		"540p":  2,
		"720p":  3,
		"1080p": 4,
		"1440p": 5,
		"2160p": 6,
	}
	numRes := resTiers[maxRes]
	if numRes == 0 {
		numRes = 2
	}
	if codecSelection == "both" || codecSelection == "" {
		return numRes * 2
	}
	return numRes
}

func (t *EncodingProgressTracker) parseTime(line string) (float64, bool) {
	idx := strings.Index(line, "time=")
	if idx == -1 {
		return 0, false
	}
	segment := line[idx+5:]
	if len(segment) < 11 {
		return 0, false
	}
	timeStr := segment[:11]
	parts := strings.Split(timeStr, ":")
	if len(parts) != 3 {
		return 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	s, err3 := strconv.ParseFloat(parts[2], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, false
	}
	return float64(h)*3600 + float64(m)*60 + s, true
}

// encodingCeiling is where the per-variant encoding share of the bar ends;
// the audio phase takes over at this value.
const encodingCeiling = 75

var selectedVariantsRE = regexp.MustCompile(`Selected (\d+) variants for encoding`)

func (t *EncodingProgressTracker) ParseLine(line string) *int {
	switch {
	case strings.Contains(line, "variants for encoding"):
		// The ladder script prints the real rung count once tiers are chosen.
		// Prefer it to estimateVariants(), which assumes one rung per
		// resolution and badly undercounts multi-rung ladders (#1018).
		if m := selectedVariantsRE.FindStringSubmatch(line); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				t.totalVariants = n
			}
		}
		return nil
	case strings.Contains(line, "Two-pass: pass 1/2"):
		t.currentPass = 1
		return nil
	case strings.Contains(line, "Two-pass: pass 2/2"):
		t.currentPass = 2
		return nil
	case strings.Contains(line, "Phase 1: Input Validation"):
		return t.setProgress("validation", 5)
	case strings.Contains(line, "Phase 1b: Tool Checks"):
		return t.setProgress("tools", 8)
	case strings.Contains(line, "Phase 2: Creating Mezzanine"):
		return t.setProgress("mezzanine", 15)
	case strings.Contains(line, "Phase 2b: Selecting Resolution Tiers"):
		return t.setProgress("tiers", 18)
	case strings.Contains(line, "Phase 3: Encoding Video Variants"):
		t.currentPhase = "encoding"
		t.currentVariantNum = 0
		return t.setProgress("encoding", 20)
	case strings.Contains(line, "Encoding:"):
		if strings.Contains(line, "H264") || strings.Contains(line, "HEVC") || strings.Contains(line, "AV1") {
			t.currentVariantNum++
			t.currentVariantInfo = line
			t.currentPass = 0
			return t.setProgress("encoding", min(20+(t.currentVariantNum-1), encodingCeiling))
		}
	case strings.Contains(line, "Phase 4: Creating Audio Mezzanine"):
		return t.setProgress("audio", 75)
	case strings.Contains(line, "Phase 5: Packaging"):
		return t.setProgress("packaging_1", 80)
	case strings.Contains(line, "Phase 6: Packaging"):
		return t.setProgress("packaging_2", 85)
	case strings.Contains(line, "Phase 7: Generating HLS"):
		return t.setProgress("hls", 90)
	case strings.Contains(line, "Encoding Complete"):
		return t.setProgress("complete", 95)
	}

	if current, ok := t.parseTime(line); ok && t.sourceDuration > 0 && t.currentPhase != "" {
		percent := current / t.sourceDuration
		if percent > 1 {
			percent = 1
		}
		switch t.currentPhase {
		case "mezzanine":
			return t.setProgress("mezzanine", 8+int(percent*7))
		case "encoding":
			if t.currentVariantNum > 0 {
				// A two-pass rung runs ffmpeg twice and each pass reports out_time
				// from zero, so give pass 1 the first half of the rung and pass 2
				// the second; otherwise pass 2 would replay pass 1's range.
				switch t.currentPass {
				case 1:
					percent = percent / 2
				case 2:
					percent = 0.5 + percent/2
				}
				base := 18
				rangeSize := float64(encodingCeiling - base)
				variantSize := rangeSize / float64(t.totalVariants)
				progress := int(float64(base) + float64(t.currentVariantNum-1)*variantSize + percent*variantSize)
				return t.setProgress("encoding", min(progress, encodingCeiling))
			}
		case "audio":
			return t.setProgress("audio", 75+int(percent*3))
		}
	}
	return nil
}

// setProgress records the phase and returns the progress to publish. The
// value is clamped to 0–100 and never moves backwards: later phases set fixed
// values (audio 75, packaging 80, ...) and a new two-pass pass restarts
// out_time, either of which would otherwise make the bar jump back (#1018).
func (t *EncodingProgressTracker) setProgress(phase string, progress int) *int {
	t.currentPhase = phase
	progress = max(0, min(progress, 100))
	if progress > t.lastProgress {
		t.lastProgress = progress
	}
	return &t.lastProgress
}

func (t *EncodingProgressTracker) ProgressMessage() string {
	if t.currentPhase == "encoding" && t.currentVariantInfo != "" {
		return "Encoding variant " + itoa(t.currentVariantNum) + "/" + itoa(t.totalVariants) + ": " + t.currentVariantInfo
	}
	return ""
}

func itoa(val int) string {
	return fmtInt(val)
}

func fmtInt(val int) string {
	if val == 0 {
		return "0"
	}
	sign := ""
	if val < 0 {
		sign = "-"
		val = -val
	}
	buf := make([]byte, 0, 12)
	for val > 0 {
		buf = append([]byte{byte('0' + val%10)}, buf...)
		val /= 10
	}
	return sign + string(buf)
}
