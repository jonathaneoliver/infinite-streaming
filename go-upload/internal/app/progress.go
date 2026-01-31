package app

import (
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

func (t *EncodingProgressTracker) ParseLine(line string) *int {
	switch {
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
			return t.setProgress("encoding", 20+(t.currentVariantNum-1))
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
				base := 18
				rangeSize := 57
				variantSize := float64(rangeSize) / float64(t.totalVariants)
				progress := int(float64(base) + float64(t.currentVariantNum-1)*variantSize + percent*variantSize)
				return t.setProgress("encoding", progress)
			}
		case "audio":
			return t.setProgress("audio", 75+int(percent*3))
		}
	}
	return nil
}

func (t *EncodingProgressTracker) setProgress(phase string, progress int) *int {
	t.currentPhase = phase
	t.lastProgress = progress
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
