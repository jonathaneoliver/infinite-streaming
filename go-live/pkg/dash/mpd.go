package dash

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/beevik/etree"
)

const (
	dashNS                   = "urn:mpeg:dash:schema:mpd:2011"
	maxLiveWindowDurationSec = 36.0
	llDashAvailabilityOffset = 1.0
	llDashTargetDelaySec     = 3.0
	llDashMinBufferSec       = 2.0
	llDashUpdatePeriodSec    = 1.0
)

type timelineElement struct {
	T int64
	D int64
	R int64
}

type timelineData struct {
	Timescale               int
	SegmentDurations        []int64
	OriginalTimelineEntries []timelineElement
	LoopDurationTicks       int64
}

type virtualSegment struct {
	Media         string
	Offset        int64
	Length        int64
	DurationTicks int64
}

type MPDData struct {
	mu sync.Mutex

	Tree *etree.Document
	Root *etree.Element
	Path string
	Rel  string

	TotalDuration   float64
	SegmentDuration float64
	SegmentCount    int
	MinSegmentCount *int

	Timelines map[string]*timelineData
	RepTimescale map[string]int

	BaseTimelines            map[string]*timelineData
	VirtualSegmentsCreated   map[int]bool
	TempSegmentCountByDur    map[int]int
	TempSegmentDurationByDur map[int]float64
	PartialDurationByDur     map[int]float64
	VirtualSegmentsByDur     map[int]map[string][]virtualSegment

	AvailabilityStartTime *time.Time
	StreamStartTime       float64
	PeriodCounter         int
	PeriodCounterInit     bool
}

var (
	mpdCacheMu sync.RWMutex
	mpdCache   = make(map[string]*MPDData)
)

func ns(tag string) string {
	return fmt.Sprintf("{%s}%s", dashNS, tag)
}

func localName(tag string) string {
	if strings.HasPrefix(tag, "{") {
		if idx := strings.Index(tag, "}"); idx != -1 {
			return tag[idx+1:]
		}
	}
	return tag
}

func findFirstByLocal(elem *etree.Element, name string) *etree.Element {
	if elem == nil {
		return nil
	}
	for _, child := range elem.FindElements(".//*") {
		if localName(child.Tag) == name {
			return child
		}
	}
	return nil
}

func findAllByLocal(elem *etree.Element, name string) []*etree.Element {
	if elem == nil {
		return nil
	}
	var out []*etree.Element
	for _, child := range elem.FindElements(".//*") {
		if localName(child.Tag) == name {
			out = append(out, child)
		}
	}
	return out
}

func LoadMPD(folder, uri string) (*MPDData, error) {
	path := filepath.Join(folder, filepath.FromSlash(uri))

	mpdCacheMu.RLock()
	if data, ok := mpdCache[path]; ok {
		mpdCacheMu.RUnlock()
		return data, nil
	}
	mpdCacheMu.RUnlock()

	doc := etree.NewDocument()
	if err := doc.ReadFromFile(path); err != nil {
		return nil, err
	}
	root := doc.Root()
	if root == nil {
		return nil, fmt.Errorf("mpd root missing")
	}

	data := &MPDData{
		Tree:                     doc,
		Root:                     root,
		Path:                     path,
		Rel:                      filepath.Dir(uri),
		Timelines:                make(map[string]*timelineData),
		RepTimescale:             make(map[string]int),
		BaseTimelines:            make(map[string]*timelineData),
		VirtualSegmentsCreated:   make(map[int]bool),
		TempSegmentCountByDur:    make(map[int]int),
		TempSegmentDurationByDur: make(map[int]float64),
		PartialDurationByDur:     make(map[int]float64),
		VirtualSegmentsByDur:     make(map[int]map[string][]virtualSegment),
	}

	if mpdDuration := root.SelectAttrValue("mediaPresentationDuration", ""); mpdDuration != "" {
		if parsed, err := parseDuration(mpdDuration); err == nil {
			data.TotalDuration = parsed
		}
	}

	periods := root.FindElements(".//" + ns("Period"))
	if len(periods) == 0 {
		periods = findAllByLocal(root, "Period")
	}

	for _, period := range periods {
		if periodDuration := period.SelectAttrValue("duration", ""); periodDuration != "" {
			if parsed, err := parseDuration(periodDuration); err == nil {
				data.TotalDuration = parsed
			}
		}

		periodTemplate := period.FindElement(".//" + ns("SegmentTemplate"))
		if periodTemplate == nil {
			periodTemplate = findFirstByLocal(period, "SegmentTemplate")
		}
		periodList := period.FindElement(".//" + ns("SegmentList"))
		if periodList == nil {
			periodList = findFirstByLocal(period, "SegmentList")
		}

		adaptations := period.FindElements(".//" + ns("AdaptationSet"))
		if len(adaptations) == 0 {
			adaptations = findAllByLocal(period, "AdaptationSet")
		}

		for _, adaptation := range adaptations {
			adaptTemplate := adaptation.FindElement(".//" + ns("SegmentTemplate"))
			if adaptTemplate == nil {
				adaptTemplate = findFirstByLocal(adaptation, "SegmentTemplate")
			}
			if adaptTemplate == nil {
				adaptTemplate = periodTemplate
			}
			adaptList := adaptation.FindElement(".//" + ns("SegmentList"))
			if adaptList == nil {
				adaptList = findFirstByLocal(adaptation, "SegmentList")
			}
			if adaptList == nil {
				adaptList = periodList
			}
			reps := adaptation.FindElements(".//" + ns("Representation"))
			if len(reps) == 0 {
				reps = findAllByLocal(adaptation, "Representation")
			}

			for _, representation := range reps {
				repID := representation.SelectAttrValue("id", "")

				segTemplate := representation.FindElement(".//" + ns("SegmentTemplate"))
				if segTemplate == nil {
					segTemplate = adaptTemplate
				}
				if segTemplate == nil {
					segTemplate = findFirstByLocal(representation, "SegmentTemplate")
				}
				if segTemplate != nil {
					timescale := parseInt(segTemplate.SelectAttrValue("timescale", "1"), 1)
					if repID != "" {
						data.RepTimescale[repID] = timescale
					}
					if durationStr := segTemplate.SelectAttrValue("duration", ""); durationStr != "" {
						durationVal := parseFloat(durationStr, 0)
						data.SegmentDuration = durationVal / float64(timescale)
					}

					segTimeline := segTemplate.FindElement(".//" + ns("SegmentTimeline"))
					if segTimeline == nil {
						segTimeline = findFirstByLocal(segTemplate, "SegmentTimeline")
					}
					if segTimeline != nil {
						segmentCount := 0
						var durations []int64
						for _, s := range findAllByLocal(segTimeline, "S") {
							repeat := parseInt(s.SelectAttrValue("r", "0"), 0) + 1
							d := parseInt64(s.SelectAttrValue("d", "0"), 0)
							segmentCount += repeat
							for i := 0; i < repeat; i++ {
								durations = append(durations, d)
							}
						}
						trackMinSegmentCount(data, segmentCount)
						if data.SegmentCount == 0 {
							data.SegmentCount = segmentCount
						}
						if data.SegmentDuration == 0 && len(durations) > 0 {
							totalTicks := int64(0)
							for _, d := range durations {
								totalTicks += d
							}
							timescale := parseInt(segTemplate.SelectAttrValue("timescale", "1"), 1)
							if timescale > 0 {
								data.SegmentDuration = float64(totalTicks) / float64(len(durations)) / float64(timescale)
							}
						}
						if data.TotalDuration == 0 && data.SegmentDuration > 0 {
							data.TotalDuration = float64(segmentCount) * data.SegmentDuration
						}
						if repID != "" && len(durations) > 0 {
							timescale := parseInt(segTemplate.SelectAttrValue("timescale", "1"), 1)
							loopTicks := int64(0)
							for _, d := range durations {
								loopTicks += d
							}
							data.Timelines[repID] = &timelineData{
								Timescale:        timescale,
								SegmentDurations: durations,
								LoopDurationTicks: loopTicks,
							}
						}
					}
				}

				segList := representation.FindElement(".//" + ns("SegmentList"))
				if segList == nil {
					segList = adaptList
				}
				if segList == nil {
					segList = findFirstByLocal(representation, "SegmentList")
				}
				if segList != nil {
					timescale := float64(parseInt(segList.SelectAttrValue("timescale", "1"), 1))
					durationStr := segList.SelectAttrValue("duration", "")
					if repID != "" {
						data.RepTimescale[repID] = int(timescale)
					}

					segTimeline := segList.FindElement(".//" + ns("SegmentTimeline"))
					if segTimeline == nil {
						segTimeline = findFirstByLocal(segList, "SegmentTimeline")
					}
					if segTimeline != nil {
						segmentCount := 0
						totalDurationTicks := int64(0)
						for _, s := range findAllByLocal(segTimeline, "S") {
							d := parseInt64(s.SelectAttrValue("d", "0"), 0)
							repeat := parseInt(s.SelectAttrValue("r", "0"), 0) + 1
							segmentCount += repeat
							totalDurationTicks += d * int64(repeat)
						}
						if segmentCount > 0 && data.SegmentDuration == 0 {
							data.SegmentDuration = float64(totalDurationTicks) / float64(segmentCount) / timescale
						}
						trackMinSegmentCount(data, segmentCount)
						if data.SegmentCount == 0 {
							data.SegmentCount = segmentCount
						}

						timeline, err := parseSegmentTimeline(segTimeline, int(timescale))
						if err == nil && repID != "" {
							loopTicks := int64(0)
							for _, d := range timeline.SegmentDurations {
								loopTicks += d
							}
							timeline.LoopDurationTicks = loopTicks
							data.Timelines[repID] = timeline
						}

						segmentURLs := segList.FindElements(".//" + ns("SegmentURL"))
						if len(segmentURLs) == 0 {
							segmentURLs = findAllByLocal(segList, "SegmentURL")
						}
						if timeline != nil && len(timeline.SegmentDurations) != len(segmentURLs) {
							fmt.Fprintf(os.Stderr, "WARNING: Rep %s timeline count mismatch: %d durations vs %d URLs\n",
								repID, len(timeline.SegmentDurations), len(segmentURLs))
						}
					} else if durationStr != "" {
						data.SegmentDuration = parseFloat(durationStr, 0) / timescale
					}

					if segTimeline == nil {
						segmentURLs := segList.FindElements(".//" + ns("SegmentURL"))
						if len(segmentURLs) == 0 {
							segmentURLs = findAllByLocal(segList, "SegmentURL")
						}
						repSegmentCount := len(segmentURLs)
						trackMinSegmentCount(data, repSegmentCount)
						if data.SegmentCount == 0 {
							data.SegmentCount = repSegmentCount
						}
					}

					if data.TotalDuration == 0 && data.SegmentDuration > 0 && data.SegmentCount > 0 {
						data.TotalDuration = float64(data.SegmentCount) * data.SegmentDuration
					}
				}
			}
		}
	}

	if data.MinSegmentCount != nil {
		data.SegmentCount = *data.MinSegmentCount
		if data.SegmentDuration > 0 {
			data.TotalDuration = float64(data.SegmentCount) * data.SegmentDuration
		}
	}

	if data.SegmentDuration == 0 && data.TotalDuration > 0 && data.SegmentCount > 0 {
		data.SegmentDuration = data.TotalDuration / float64(data.SegmentCount)
	}

	if data.SegmentCount == 0 && data.SegmentDuration > 0 && data.TotalDuration > 0 {
		derived := int(math.Round(data.TotalDuration / data.SegmentDuration))
		if derived > 0 {
			data.SegmentCount = derived
		}
	}

	if data.SegmentCount == 0 {
		firstRep := findFirstByLocal(root, "Representation")
		if firstRep != nil {
			segmentURLs := findAllByLocal(firstRep, "SegmentURL")
			if len(segmentURLs) > 0 {
				data.SegmentCount = len(segmentURLs)
				if data.MinSegmentCount == nil {
					min := data.SegmentCount
					data.MinSegmentCount = &min
				}
				fmt.Fprintf(os.Stderr, "INFO: Derived segment count from SegmentURL list: %d\n", data.SegmentCount)
			}
		}
	}

	if data.SegmentDuration == 0 && data.TotalDuration > 0 && data.SegmentCount > 0 {
		data.SegmentDuration = data.TotalDuration / float64(data.SegmentCount)
	}

	if (data.SegmentDuration == 0 || data.SegmentDuration < 0.5) && len(data.Timelines) > 0 {
		for _, timeline := range data.Timelines {
			if len(timeline.SegmentDurations) == 0 || timeline.Timescale == 0 {
				continue
			}
			totalTicks := int64(0)
			for _, d := range timeline.SegmentDurations {
				totalTicks += d
			}
			avgSeconds := float64(totalTicks) / float64(len(timeline.SegmentDurations)) / float64(timeline.Timescale)
			if avgSeconds > 0 {
				data.SegmentDuration = avgSeconds
				break
			}
		}
	}

	expectedTotal := data.SegmentDuration * float64(data.SegmentCount)
	if expectedTotal > 0 && (data.TotalDuration == 0 || data.TotalDuration < expectedTotal) {
		data.TotalDuration = expectedTotal
	}

	if data.SegmentDuration > 0 && data.SegmentCount > 0 {
		for repID, timescale := range data.RepTimescale {
			if _, ok := data.Timelines[repID]; ok {
				continue
			}
			if timescale == 0 {
				timescale = 1
			}
			durationTicks := int64(math.Round(data.SegmentDuration * float64(timescale)))
			if durationTicks <= 0 {
				continue
			}
			segments := make([]int64, data.SegmentCount)
			for i := 0; i < data.SegmentCount; i++ {
				segments[i] = durationTicks
			}
			loopTicks := durationTicks * int64(data.SegmentCount)
			data.Timelines[repID] = &timelineData{
				Timescale:        timescale,
				SegmentDurations: segments,
				LoopDurationTicks: loopTicks,
			}
			fmt.Fprintf(os.Stderr, "INFO: Rep %s missing SegmentTimeline; using synthetic durations\n", repID)
		}
	}

	mpdCacheMu.Lock()
	mpdCache[path] = data
	mpdCacheMu.Unlock()

	return data, nil
}

func trackMinSegmentCount(data *MPDData, count int) {
	if data.MinSegmentCount == nil {
		data.MinSegmentCount = &count
		return
	}
	if count < *data.MinSegmentCount {
		*data.MinSegmentCount = count
	}
}

func parseDuration(durationStr string) (float64, error) {
	value := strings.TrimSpace(durationStr)
	if strings.HasPrefix(value, "PT") {
		value = strings.TrimPrefix(value, "PT")
	}
	if strings.HasSuffix(value, "S") {
		value = strings.TrimSuffix(value, "S")
	}
	return strconv.ParseFloat(value, 64)
}

func formatDuration(seconds float64) string {
	return "PT" + strconv.FormatFloat(seconds, 'f', -1, 64) + "S"
}

func parseSegmentTimeline(segTimeline *etree.Element, timescale int) (*timelineData, error) {
	segmentDurations := make([]int64, 0)
	originalElements := make([]timelineElement, 0)

	for _, s := range findAllByLocal(segTimeline, "S") {
		t := parseInt64(s.SelectAttrValue("t", "0"), 0)
		d := parseInt64(s.SelectAttrValue("d", "0"), 0)
		r := parseInt64(s.SelectAttrValue("r", "0"), 0)

		originalElements = append(originalElements, timelineElement{T: t, D: d, R: r})
		for i := int64(0); i < r+1; i++ {
			segmentDurations = append(segmentDurations, d)
		}
	}

	return &timelineData{
		Timescale:               timescale,
		SegmentDurations:        segmentDurations,
		OriginalTimelineEntries: originalElements,
	}, nil
}

func ensureVirtualSegments(data *MPDData, duration int) (int, float64) {
	if duration != 2 {
		return data.SegmentCount, data.SegmentDuration
	}
	if data.VirtualSegmentsByDur[duration] == nil {
		if len(data.BaseTimelines) == 0 {
			for repID, timeline := range data.Timelines {
				baseCopy := &timelineData{
					Timescale:               timeline.Timescale,
					SegmentDurations:        append([]int64(nil), timeline.SegmentDurations...),
					OriginalTimelineEntries: timeline.OriginalTimelineEntries,
					LoopDurationTicks:       timeline.LoopDurationTicks,
				}
				data.BaseTimelines[repID] = baseCopy
			}
		}
		data.VirtualSegmentsByDur[duration] = make(map[string][]virtualSegment)
		reps := data.Root.FindElements(".//" + ns("Representation"))
		if len(reps) == 0 {
			reps = findAllByLocal(data.Root, "Representation")
		}
		for _, rep := range reps {
			repID := rep.SelectAttrValue("id", "")
			if repID == "" {
				continue
			}
			segments, err := buildVirtualSegmentsForRep(data, rep, repID, duration)
			if err != nil {
				fmt.Fprintf(os.Stderr, "WARN: Failed to build virtual segments for rep %s: %v\n", repID, err)
				continue
			}
			data.VirtualSegmentsByDur[duration][repID] = segments
		}
	}

	segmentCount := 0
	for _, segments := range data.VirtualSegmentsByDur[duration] {
		if len(segments) > 0 {
			segmentCount = len(segments)
			break
		}
	}
	if segmentCount > 0 {
		data.TempSegmentCountByDur[duration] = segmentCount
		data.TempSegmentDurationByDur[duration] = float64(duration)
	}
	return segmentCount, float64(duration)
}

func buildVirtualSegmentsForRep(data *MPDData, rep *etree.Element, repID string, duration int) ([]virtualSegment, error) {
	segMeta := collectSegmentListMeta(rep)
	segmentURLs := segMeta.segmentURLs
	if len(segmentURLs) == 0 {
		segmentURLs = collectSegmentURLs(rep)
	}
	if len(segmentURLs) == 0 {
		return nil, fmt.Errorf("no SegmentURL entries")
	}

	timescale := 1
	if segMeta.timescale != "" {
		timescale = parseInt(segMeta.timescale, 1)
	} else if repTimescale, ok := data.RepTimescale[repID]; ok {
		timescale = repTimescale
	}
	if timescale <= 0 {
		timescale = 1
	}

	baseTimeline := data.BaseTimelines[repID]
	if baseTimeline == nil {
		baseTimeline = data.Timelines[repID]
	}
	var baseDurations []int64
	if baseTimeline != nil && len(baseTimeline.SegmentDurations) > 0 {
		baseDurations = baseTimeline.SegmentDurations
	}
	if len(baseDurations) == 0 {
		segmentTicks := int64(math.Round(data.SegmentDuration * float64(timescale)))
		baseDurations = make([]int64, len(segmentURLs))
		for i := range baseDurations {
			baseDurations[i] = segmentTicks
		}
	}

	virtualSegments := make([]virtualSegment, 0)
	for i, segURL := range segmentURLs {
		media := segURL.SelectAttrValue("media", "")
		if media == "" {
			continue
		}
		segmentTicks := baseDurations[minInt(i, len(baseDurations)-1)]
		segmentDuration := float64(segmentTicks) / float64(timescale)
		fragments, err := loadByterangesForSegment(data.Path, media)
		if err != nil || len(fragments) == 0 {
			virtualSegments = append(virtualSegments, virtualSegment{
				Media:         media,
				Offset:        0,
				Length:        0,
				DurationTicks: segmentTicks,
			})
			continue
		}

		fragmentDuration := segmentDuration / float64(len(fragments))
		groupSize := int(math.Round(float64(duration) / fragmentDuration))
		if groupSize < 1 {
			groupSize = 1
		}

		for j := 0; j < len(fragments); j += groupSize {
			end := j + groupSize
			if end > len(fragments) {
				end = len(fragments)
			}
			length := int64(0)
			for _, frag := range fragments[j:end] {
				length += frag.Length
			}
			durTicks := int64(math.Round(fragmentDuration * float64(end-j) * float64(timescale)))
			virtualSegments = append(virtualSegments, virtualSegment{
				Media:         media,
				Offset:        fragments[j].Offset,
				Length:        length,
				DurationTicks: durTicks,
			})
		}
	}

	return virtualSegments, nil
}

func loadByterangesForSegment(mpdPath, mediaPath string) ([]byterangePayloadFragment, error) {
	segmentPath := filepath.Join(filepath.Dir(mpdPath), filepath.FromSlash(strings.TrimPrefix(mediaPath, "/")))
	byterangesPath := segmentPath + ".byteranges"
	payloadBytes, err := os.ReadFile(byterangesPath)
	if err != nil {
		return nil, err
	}
	var payload byterangePayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, err
	}
	return payload.Fragments, nil
}

type byterangePayloadFragment struct {
	Offset      int64 `json:"offset"`
	Length      int64 `json:"length"`
	Independent bool  `json:"independent"`
}

func GenerateLiveMPD(data *MPDData, timeNow time.Time, streamName string, duration int, llMode bool) ([]byte, error) {
	data.mu.Lock()
	defer data.mu.Unlock()

	doc := etree.NewDocument()
	doc.SetRoot(data.Root.Copy())
	root := doc.Root()

	totalDuration := data.TotalDuration
	baseSegmentDuration := data.SegmentDuration
	segmentDuration := data.SegmentDuration
	segmentCount := data.SegmentCount

	fmt.Fprintf(os.Stderr, "[GO-LIVE:DASH] Base MPD: totalDuration=%.3f segmentDuration=%.3f segmentCount=%d\n",
		totalDuration, segmentDuration, segmentCount)

	if duration == 2 {
		virtualCount, virtualDuration := ensureVirtualSegments(data, duration)
		if virtualCount > 0 {
			segmentCount = virtualCount
			segmentDuration = virtualDuration
			fmt.Fprintf(os.Stderr, "DEBUG: Using %ds virtual segments count=%d\n", duration, segmentCount)
		}
	}

    if totalDuration == 0 || segmentCount == 0 {
        fmt.Fprintf(os.Stderr, "[GO-LIVE:DASH] Skipping live MPD generation: totalDuration=%.3f segmentDuration=%.3f segmentCount=%d\n",
            totalDuration, segmentDuration, segmentCount)
        return doc.WriteToBytes()
    }

	if data.AvailabilityStartTime == nil {
		start := timeFromSeconds(float64(timeNow.UnixNano())/1e9 - totalDuration)
		data.AvailabilityStartTime = &start
		data.StreamStartTime = float64(timeNow.UnixNano()) / 1e9
		fmt.Fprintf(os.Stderr, "🕐 [%s] Setting availabilityStartTime to %s (stream epoch)\n",
			streamName, data.AvailabilityStartTime.UTC().Format(time.RFC3339))
	}

	timeNowSeconds := float64(timeNow.UnixNano()) / 1e9
	elapsedSinceStart := timeNowSeconds - data.StreamStartTime
	if elapsedSinceStart < 0 {
		elapsedSinceStart = 0
	}
	timeOffset := math.Mod(elapsedSinceStart, totalDuration)
	loopCount := int(elapsedSinceStart / totalDuration)
	segmentOffset := int(timeOffset / segmentDuration)
	totalElapsedSegments := (loopCount * segmentCount) + segmentOffset
	elapsedTime := timeOffset
	availabilityOffset := segmentDuration
	if llMode {
		if duration == 2 {
			availabilityOffset = detectPartialDuration(data, duration, baseSegmentDuration)
		} else {
			availabilityOffset = detectPartialDuration(data, duration, segmentDuration)
		}
	}

	if !data.PeriodCounterInit {
		data.PeriodCounter = 0
		data.PeriodCounterInit = true
		fmt.Fprintf(os.Stderr, "🔢 [%s] Initializing period counter to 0\n", streamName)
	}

	availabilityStartTime := data.AvailabilityStartTime.UTC()
	publishTime := timeNow.UTC()

	root.RemoveAttr("mediaPresentationDuration")
	root.RemoveAttr("type")
	root.RemoveAttr("availabilityStartTime")
	root.RemoveAttr("publishTime")
	root.RemoveAttr("suggestedPresentationDelay")
	root.RemoveAttr("minBufferTime")
	root.RemoveAttr("minimumUpdatePeriod")
	root.RemoveAttr("timeShiftBufferDepth")

	root.CreateAttr("type", "dynamic")
	root.CreateAttr("availabilityStartTime", availabilityStartTime.Format(time.RFC3339))
	publishLayout := time.RFC3339
	if llMode {
		publishLayout = time.RFC3339Nano
	}
	root.CreateAttr("publishTime", publishTime.Format(publishLayout))
	if llMode {
		root.CreateAttr("suggestedPresentationDelay", formatDuration(llDashTargetDelaySec))
		root.CreateAttr("minBufferTime", formatDuration(llDashMinBufferSec))
		root.CreateAttr("minimumUpdatePeriod", formatDuration(llDashUpdatePeriodSec))
	} else {
		root.CreateAttr("suggestedPresentationDelay", formatDuration(segmentDuration*3))
		root.CreateAttr("minBufferTime", formatDuration(segmentDuration*2))
		root.CreateAttr("minimumUpdatePeriod", formatDuration(segmentDuration))
	}
	root.CreateAttr("timeShiftBufferDepth", formatDuration(maxLiveWindowDurationSec))

	// BaseURL creation intentionally omitted to match python behavior (no insert).
	removeServiceDescriptions(root)
	if llMode {
		insertServiceDescription(root, llDashTargetDelaySec, llDashTargetDelaySec*0.5, llDashTargetDelaySec*1.5)
	}

	windowSegmentCount := int(maxLiveWindowDurationSec / segmentDuration)
	windowEndLogical := totalElapsedSegments + 1
	windowStartLogical := windowEndLogical - windowSegmentCount
	if windowStartLogical < 0 {
		windowStartLogical += segmentCount
		windowEndLogical += segmentCount
	}

	windowStartLoop := windowStartLogical / segmentCount
	windowEndLoop := (windowEndLogical - 1) / segmentCount
	crossesLoop := windowStartLoop != windowEndLoop

	fmt.Fprintf(os.Stderr,
		"[GO-LIVE:DASH] Window duration=%.3fs segDur=%.3fs segCount=%d windowSegs=%d elapsed=%.3fs elapsedSegs=%d start=%d end=%d loops=%d->%d\n",
		maxLiveWindowDurationSec, segmentDuration, segmentCount, windowSegmentCount, elapsedTime,
		totalElapsedSegments, windowStartLogical, windowEndLogical, windowStartLoop, windowEndLoop)

	periodsToRemove := root.FindElements(".//" + ns("Period"))
	if len(periodsToRemove) == 0 {
		periodsToRemove = findAllByLocal(root, "Period")
	}
	for _, period := range periodsToRemove {
		if parent := period.Parent(); parent != nil {
			parent.RemoveChild(period)
		}
	}

	templatePeriod := data.Root.FindElement(".//" + ns("Period"))
	if templatePeriod == nil {
		templatePeriod = findFirstByLocal(data.Root, "Period")
	}
	if templatePeriod == nil {
		return nil, fmt.Errorf("no Period element found in original MPD")
	}

	segmentProgress := elapsedTime - (float64(segmentOffset) * segmentDuration)
	segmentFraction := 0.0
	if segmentDuration > 0 {
		segmentFraction = segmentProgress / segmentDuration
	}
	if segmentFraction < 0 {
		segmentFraction = 0
	} else if segmentFraction > 1 {
		segmentFraction = 1
	}
	currentLogical := totalElapsedSegments

	if crossesLoop {
		loopBoundary := (windowStartLoop + 1) * segmentCount
		tailStart := windowStartLogical
		tailEnd := loopBoundary
		headStart := loopBoundary
		headEnd := windowEndLogical

		periodTail := templatePeriod.Copy()
		periodTail.RemoveAttr("duration")
		periodTail.CreateAttr("id", fmt.Sprintf("loop_%d", windowStartLoop))
		periodTail.CreateAttr("start", formatDuration(float64(windowStartLoop)*totalDuration))
		buildPeriodSegments(periodTail, data, tailStart, tailEnd, duration, segmentDuration, availabilityOffset, streamName, llMode, currentLogical, segmentFraction)
		root.AddChild(periodTail)

		periodHead := templatePeriod.Copy()
		periodHead.RemoveAttr("duration")
		periodHead.CreateAttr("id", fmt.Sprintf("loop_%d", windowEndLoop))
		periodHead.CreateAttr("start", formatDuration(float64(windowEndLoop)*totalDuration))
		buildPeriodSegments(periodHead, data, headStart, headEnd, duration, segmentDuration, availabilityOffset, streamName, llMode, currentLogical, segmentFraction)
		root.AddChild(periodHead)

		fmt.Fprintf(os.Stderr,
			"🎬 [%s] Multi-period window: loop %d (segments [%d-%d)) + loop %d (segments [%d-%d)), elapsed: %.1fs\n",
			streamName, windowStartLoop, tailStart, tailEnd, windowEndLoop, headStart, headEnd, elapsedTime)
	} else {
		period := templatePeriod.Copy()
		period.RemoveAttr("duration")
		period.CreateAttr("id", fmt.Sprintf("loop_%d", windowStartLoop))
		period.CreateAttr("start", formatDuration(float64(windowStartLoop)*totalDuration))
		buildPeriodSegments(period, data, windowStartLogical, windowEndLogical, duration, segmentDuration, availabilityOffset, streamName, llMode, currentLogical, segmentFraction)
		root.AddChild(period)

		fmt.Fprintf(os.Stderr, "🎬 [%s] Single-period window: loop %d, segments [%d-%d), elapsed: %.1fs\n",
			streamName, windowStartLoop, windowStartLogical, windowEndLogical, elapsedTime)
	}

	doc.Indent(2)
	return doc.WriteToBytes()
}

func buildPeriodSegments(period *etree.Element, data *MPDData, startLogical, endLogical, duration int, segmentDuration float64, availabilityOffset float64, streamName string, llMode bool, currentLogical int, segmentFraction float64) {
	adaptations := period.FindElements(".//" + ns("AdaptationSet"))
	if len(adaptations) == 0 {
		adaptations = findAllByLocal(period, "AdaptationSet")
	}
	for _, adaptation := range adaptations {
		reps := adaptation.FindElements(".//" + ns("Representation"))
		if len(reps) == 0 {
			reps = findAllByLocal(adaptation, "Representation")
		}
		for _, representation := range reps {
			repID := representation.SelectAttrValue("id", "")
			if repID == "" {
				continue
			}
			if _, ok := data.Timelines[repID]; !ok {
				if timeline := buildTimelineFromRepresentation(representation); timeline != nil {
					data.Timelines[repID] = timeline
				}
			}
			if _, ok := data.Timelines[repID]; !ok {
				continue
			}
			buildExplicitSegmentList(representation, data, startLogical, endLogical, repID, streamName, duration, segmentDuration, availabilityOffset, llMode, currentLogical, segmentFraction)
		}
	}
}

func collectSegmentURLs(rep *etree.Element) []*etree.Element {
	segList := rep.FindElement(".//" + ns("SegmentList"))
	if segList == nil {
		for _, child := range rep.ChildElements() {
			if localName(child.Tag) == "SegmentList" {
				segList = child
				break
			}
		}
	}
	if segList == nil {
		segList = findFirstByLocal(rep, "SegmentList")
	}
	if segList == nil {
		return nil
	}
	urls := segList.FindElements(".//" + ns("SegmentURL"))
	if len(urls) == 0 {
		urls = findAllByLocal(segList, "SegmentURL")
	}
	return urls
}

func removeSegmentLists(rep *etree.Element) {
	var toRemove []*etree.Element
	for _, child := range rep.FindElements(".//*") {
		if localName(child.Tag) == "SegmentList" {
			toRemove = append(toRemove, child)
		}
	}
	for _, node := range toRemove {
		if parent := node.Parent(); parent != nil {
			parent.RemoveChild(node)
		}
	}
}

type segmentListMeta struct {
	timescale      string
	initialization *etree.Element
	segmentURLs    []*etree.Element
}

func collectSegmentListMeta(rep *etree.Element) segmentListMeta {
	segList := rep.FindElement(".//" + ns("SegmentList"))
	if segList == nil {
		for _, child := range rep.ChildElements() {
			if localName(child.Tag) == "SegmentList" {
				segList = child
				break
			}
		}
	}
	if segList == nil {
		segList = findFirstByLocal(rep, "SegmentList")
	}
	if segList == nil {
		return segmentListMeta{}
	}

	timescale := segList.SelectAttrValue("timescale", "")
	initElem := segList.FindElement(".//" + ns("Initialization"))
	if initElem == nil {
		initElem = findFirstByLocal(segList, "Initialization")
	}
	var initCopy *etree.Element
	if initElem != nil {
		initCopy = initElem.Copy()
	}

	urls := segList.FindElements(".//" + ns("SegmentURL"))
	if len(urls) == 0 {
		urls = findAllByLocal(segList, "SegmentURL")
	}

	return segmentListMeta{
		timescale:      timescale,
		initialization: initCopy,
		segmentURLs:    urls,
	}
}

func buildTimelineFromRepresentation(rep *etree.Element) *timelineData {
	segList := rep.FindElement(".//" + ns("SegmentList"))
	if segList == nil {
		segList = findFirstByLocal(rep, "SegmentList")
	}
	if segList == nil {
		return nil
	}

	timescale := parseInt(segList.SelectAttrValue("timescale", "1"), 1)
	segTimeline := segList.FindElement(".//" + ns("SegmentTimeline"))
	if segTimeline == nil {
		segTimeline = findFirstByLocal(segList, "SegmentTimeline")
	}
	if segTimeline == nil {
		segmentURLs := findAllByLocal(segList, "SegmentURL")
		if len(segmentURLs) == 0 {
			return nil
		}
		durations := make([]int64, len(segmentURLs))
		for i := range durations {
			durations[i] = 1
		}
		return &timelineData{
			Timescale:        timescale,
			SegmentDurations: durations,
			LoopDurationTicks: int64(len(durations)),
		}
	}

	timeline, err := parseSegmentTimeline(segTimeline, timescale)
	if err != nil {
		return nil
	}
	loopTicks := int64(0)
	for _, d := range timeline.SegmentDurations {
		loopTicks += d
	}
	timeline.LoopDurationTicks = loopTicks
	return timeline
}

func buildExplicitSegmentList(representation *etree.Element, data *MPDData, windowStartLogical, windowEndLogical int, repID, streamName string, duration int, segmentDuration float64, availabilityOffset float64, llMode bool, currentLogical int, segmentFraction float64) {
	timeline := data.Timelines[repID]
	segmentDurations := timeline.SegmentDurations
	segmentCount := len(segmentDurations)
	virtualSegments := []virtualSegment{}
	usePartials := llMode

	if duration == 2 {
		if byDur, ok := data.VirtualSegmentsByDur[duration]; ok {
			virtualSegments = byDur[repID]
			if len(virtualSegments) > 0 {
				segmentCount = len(virtualSegments)
				segmentDurations = make([]int64, 0, segmentCount)
				for _, seg := range virtualSegments {
					segmentDurations = append(segmentDurations, seg.DurationTicks)
				}
			}
		}
	}
	if segmentCount == 0 {
		return
	}

	segMeta := collectSegmentListMeta(representation)
	removeSegmentLists(representation)

	segList := etree.NewElement("SegmentList")
	if segMeta.timescale != "" {
		segList.CreateAttr("timescale", segMeta.timescale)
	}
	if segMeta.initialization != nil {
		if duration == 2 || duration == 4 {
			initURL := segMeta.initialization.SelectAttrValue("sourceURL", "")
			if initURL != "" {
				segMeta.initialization.RemoveAttr("sourceURL")
				segMeta.initialization.CreateAttr("sourceURL", prefixDashPath(data.Rel, initURL))
			}
		}
		segList.AddChild(segMeta.initialization)
	}
	representation.AddChild(segList)

	segList.RemoveAttr("timescale")
	segList.RemoveAttr("presentationTimeOffset")
	segList.CreateAttr("timescale", strconv.Itoa(timeline.Timescale))
	segList.CreateAttr("presentationTimeOffset", "0")
	if llMode {
		if availabilityOffset <= 0 {
			availabilityOffset = llDashAvailabilityOffset
		}
		segList.CreateAttr("availabilityTimeOffset", fmt.Sprintf("%.3f", availabilityOffset))
		segList.CreateAttr("availabilityTimeComplete", "false")
	} else {
		segList.CreateAttr("availabilityTimeComplete", "true")
	}

	originalSegmentURLs := segMeta.segmentURLs
	if len(originalSegmentURLs) == 0 {
		originalSegmentURLs = findAllByLocal(segList, "SegmentURL")
	}

	segTimeline := etree.NewElement("SegmentTimeline")
	segList.AddChild(segTimeline)

	segmentInLoopStart := windowStartLogical % segmentCount
	cumulativeTime := int64(0)
	for i := 0; i < segmentInLoopStart; i++ {
		cumulativeTime += segmentDurations[i]
	}

	for logical := windowStartLogical; logical < windowEndLogical; logical++ {
		physical := logical % segmentCount
		durationTicks := segmentDurations[physical]
		baseMediaPath := ""
		if physical < len(originalSegmentURLs) {
			baseMediaPath = originalSegmentURLs[physical].SelectAttrValue("media", "")
		}
		if baseMediaPath == "" {
			baseMediaPath = fmt.Sprintf("segment_%05d.m4s", physical)
		}

		if len(virtualSegments) > 0 {
			seg := virtualSegments[physical]
			baseMediaPath = seg.Media
		}

		segmentBasePath := baseMediaPath
		if duration == 2 {
			segmentBasePath = prefixDashPath(data.Rel, segmentBasePath)
		}

		if usePartials && len(virtualSegments) == 0 {
			fragments, err := loadByterangesForSegment(data.Path, baseMediaPath)
			if err == nil && len(fragments) > 0 {
				fragCount := int64(len(fragments))
				baseDur := durationTicks / fragCount
				remainder := durationTicks % fragCount
				partialLimit := len(fragments) - 1
				if logical == currentLogical {
					partialLimit = int(math.Floor(segmentFraction * float64(len(fragments))))
					if partialLimit < 0 {
						partialLimit = 0
					} else if partialLimit >= len(fragments) {
						partialLimit = len(fragments) - 1
					}
				}
				for idx, frag := range fragments {
					if idx > partialLimit {
						break
					}
					dur := baseDur
					if int64(idx) < remainder {
						dur++
					}
					if dur <= 0 {
						dur = 1
					}
					s := etree.NewElement("S")
					s.CreateAttr("t", strconv.FormatInt(cumulativeTime, 10))
					s.CreateAttr("d", strconv.FormatInt(dur, 10))
					segTimeline.AddChild(s)
					cumulativeTime += dur

					segURL := etree.NewElement("SegmentURL")
					segURL.CreateAttr("media", segmentBasePath)
					end := frag.Offset + frag.Length - 1
					if end < frag.Offset {
						end = frag.Offset
					}
					segURL.CreateAttr("mediaRange", fmt.Sprintf("%d-%d", frag.Offset, end))
					segList.AddChild(segURL)
				}
				continue
			}
		}

		s := etree.NewElement("S")
		s.CreateAttr("t", strconv.FormatInt(cumulativeTime, 10))
		s.CreateAttr("d", strconv.FormatInt(durationTicks, 10))
		segTimeline.AddChild(s)
		cumulativeTime += durationTicks

		mediaRange := ""
		if len(virtualSegments) > 0 {
			seg := virtualSegments[physical]
			if seg.Length > 0 {
				end := seg.Offset + seg.Length - 1
				if end < seg.Offset {
					end = seg.Offset
				}
				mediaRange = fmt.Sprintf("%d-%d", seg.Offset, end)
			}
		}

		segURL := etree.NewElement("SegmentURL")
		segURL.CreateAttr("media", segmentBasePath)
		if mediaRange != "" {
			segURL.CreateAttr("mediaRange", mediaRange)
		}
		segList.AddChild(segURL)
	}
}

func parseInt(value string, fallback int) int {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func parseInt64(value string, fallback int64) int64 {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func parseFloat(value string, fallback float64) float64 {
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

type byterangePayload struct {
	Fragments []byterangePayloadFragment `json:"fragments"`
}

func detectPartialDuration(data *MPDData, duration int, segmentDuration float64) float64 {
	if data == nil {
		return llDashAvailabilityOffset
	}
	if cached, ok := data.PartialDurationByDur[duration]; ok {
		return cached
	}

	segmentURL := findFirstByLocal(data.Root, "SegmentURL")
	if segmentURL == nil {
		return llDashAvailabilityOffset
	}
	mediaPath := segmentURL.SelectAttrValue("media", "")
	if mediaPath == "" {
		return llDashAvailabilityOffset
	}
	mediaPath = strings.TrimPrefix(mediaPath, "/")
	segmentPath := filepath.Join(filepath.Dir(data.Path), filepath.FromSlash(mediaPath))
	byterangesPath := segmentPath + ".byteranges"
	payloadBytes, err := os.ReadFile(byterangesPath)
	if err != nil {
		return llDashAvailabilityOffset
	}
	var payload byterangePayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return llDashAvailabilityOffset
	}
	if len(payload.Fragments) == 0 {
		return llDashAvailabilityOffset
	}
	partialDuration := segmentDuration / float64(len(payload.Fragments))
	data.PartialDurationByDur[duration] = partialDuration
	return partialDuration
}

func removeServiceDescriptions(root *etree.Element) {
	for _, child := range root.ChildElements() {
		if localName(child.Tag) == "ServiceDescription" {
			root.RemoveChild(child)
		}
	}
}

func insertServiceDescription(root *etree.Element, target, min, max float64) {
	service := etree.NewElement("ServiceDescription")
	latency := etree.NewElement("Latency")
	latency.CreateAttr("target", fmt.Sprintf("%.3f", target))
	latency.CreateAttr("min", fmt.Sprintf("%.3f", min))
	latency.CreateAttr("max", fmt.Sprintf("%.3f", max))
	service.AddChild(latency)

	insertIndex := 0
	for i, child := range root.ChildElements() {
		if localName(child.Tag) == "Period" {
			insertIndex = i
			break
		}
		insertIndex = i + 1
	}
	root.InsertChildAt(insertIndex, service)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func prefixDashPath(rel, media string) string {
	if media == "" {
		return media
	}
	if strings.HasPrefix(media, "/") {
		return media
	}
	rel = filepath.ToSlash(rel)
	return fmt.Sprintf("/go-live/%s/%s", rel, strings.TrimPrefix(media, "/"))
}

func timeFromSeconds(seconds float64) time.Time {
	sec := math.Floor(seconds)
	nsec := (seconds - sec) * 1e9
	return time.Unix(int64(sec), int64(nsec)).UTC()
}
