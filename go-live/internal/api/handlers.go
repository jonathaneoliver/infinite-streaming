package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/boss/go-live/internal/manager"
	"github.com/boss/go-live/pkg/dash"
	"github.com/boss/go-live/pkg/fileutil"
	"github.com/boss/go-live/pkg/generator"
	"github.com/boss/go-live/pkg/parser"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

var (
	infiniteOutputDir = getEnvAny([]string{"INFINITE_STREAM_OUTPUT_DIR", "INFINITE_OUTPUT_DIR", "BOSS_OUTPUT_DIR"}, "/boss/dynamic_content")
	infiniteContentDir = getEnvAny([]string{"INFINITE_STREAM_CONTENT_DIR", "INFINITE_CONTENT_DIR", "BOSS_CONTENT_DIR"}, "/content")
	goLiveDir      = getEnv("GO_LIVE_OUTPUT_DIR", filepath.Join(infiniteContentDir, "go-live"))
)

type dashCacheEntry struct {
	data    []byte
	updated time.Time
	running bool
}

var (
	dashCacheMu sync.Mutex
	dashCache   = make(map[string]*dashCacheEntry)
)

type tickStats struct {
	LastTick  float64 `json:"last_tick"`
	Avg5m     float64 `json:"avg_5m"`
	Variants  int     `json:"variants"`
	Audio     int     `json:"audio"`
	UpdatedAt string  `json:"updated_at"`
}

var (
	tickStatsMu        sync.RWMutex
	tickStatsByContent = make(map[string]tickStats)
)

var (
	dashTickMu   sync.RWMutex
	dashTickByKey = make(map[string]tickStats)
)

var (
	rangeTickMu   sync.RWMutex
	rangeTickByKey = make(map[string]tickStats)
)

var (
	hlsWorkerMu sync.Mutex
	hlsWorkers  = make(map[string]*hlsWorker)
)

type hlsWorker struct {
	content   string
	inputPath string
	prefix    string
	cancel    context.CancelFunc
	mu         sync.Mutex
	mpdRelPath string
	mpdData    *dash.MPDData
}


func getEnv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getEnvAny(keys []string, fallback string) string {
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return fallback
}

func isVerboseLoggingEnabled() bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv("GO_LIVE_VERBOSE_LOGS")))
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func logf(format string, args ...any) {
	ts := time.Now().Format("2006-01-02 15:04:05.000")
	fmt.Fprintf(os.Stderr, "%s %s", ts, fmt.Sprintf(format, args...))
}

func parseAudioMediaURIs(masterPath string) []string {
	data, err := os.ReadFile(masterPath)
	if err != nil {
		return nil
	}

	uris := make(map[string]struct{})
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#EXT-X-MEDIA:") {
			continue
		}
		if !strings.Contains(line, "TYPE=AUDIO") {
			continue
		}
		if uri := extractAttributeValue(line, "URI"); uri != "" {
			uris[uri] = struct{}{}
		}
	}

	out := make([]string, 0, len(uris))
	for uri := range uris {
		out = append(out, uri)
	}
	return out
}

func extractAttributeValue(line, key string) string {
	search := key + "="
	idx := strings.Index(line, search)
	if idx == -1 {
		return ""
	}
	rest := line[idx+len(search):]
	if len(rest) == 0 {
		return ""
	}
	if rest[0] == '"' {
		rest = rest[1:]
		end := strings.Index(rest, "\"")
		if end == -1 {
			return ""
		}
		return rest[:end]
	}
	end := strings.Index(rest, ",")
	if end == -1 {
		return rest
	}
	return rest[:end]
}

type Handler struct {
	Manager *manager.ProcessManager
	Tracker *StreamTracker
}

func (h *Handler) Healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Served-By", "go-live")
	w.Write([]byte("go-live OK"))
}

func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	procs := h.Manager.Status()
	streams := []StreamStatus{}
	idleSeconds := defaultIdleTimeoutSeconds
	if h.Tracker != nil {
		streams = h.Tracker.Snapshot(time.Now())
		for i := range streams {
			streams[i] = h.decorateStreamStats(streams[i])
		}
		idleSeconds = int(h.Tracker.IdleTimeout().Seconds())
	}

	response := map[string]interface{}{
		"mode":            "go-live",
		"idle_timeout":    idleSeconds,
		"active_processes": len(procs),
		"processes":        procs,
		"active_streams":   streams,
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Served-By", "go-live")
	json.NewEncoder(w).Encode(response)
}

func (h *Handler) TickStats(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	content := vars["content"]

	tickStatsMu.RLock()
	stats, ok := tickStatsByContent[content]
	tickStatsMu.RUnlock()
	if !ok {
		http.Error(w, "stats not available", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Served-By", "go-live")
	json.NewEncoder(w).Encode(stats)
}

func (h *Handler) DashTickStats(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	content := vars["content"]
	duration := r.URL.Query().Get("duration")
	if duration == "" {
		duration = "ll"
	}
	key := fmt.Sprintf("%s|%s", content, duration)

	dashTickMu.RLock()
	stats, ok := dashTickByKey[key]
	dashTickMu.RUnlock()
	if !ok {
		http.Error(w, "stats not available", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Served-By", "go-live")
	json.NewEncoder(w).Encode(stats)
}

func routePrefix(path string) string {
	return "/go-live"
}

func durationMasterFilename(duration string) string {
	return fmt.Sprintf("master_%s.m3u8", duration)
}

func durationVariantFilename(duration, variant string) string {
	label := variantLabel(variant)
	return fmt.Sprintf("playlist_%s_%s.m3u8", duration, label)
}

func durationOutputPath(content, leaf string) string {
	return filepath.Join(goLiveDir, content, leaf)
}

func durationVariantOutputPath(content, duration, variant string) string {
	if strings.HasPrefix(variant, fmt.Sprintf("playlist_%s_", duration)) {
		return durationOutputPath(content, variant)
	}
	return durationOutputPath(content, durationVariantFilename(duration, variant))
}

func ensureHLSWorker(h *Handler, content, inputPath, prefix string) *hlsWorker {
	if h == nil || h.Manager == nil {
		return nil
	}
	processID := "hls-worker-" + content
	hlsWorkerMu.Lock()
	worker := hlsWorkers[content]
	if worker == nil || !h.Manager.IsRunning(processID) {
		ctx, cancel := context.WithCancel(context.Background())
		h.Manager.Spawn(processID, inputPath, goLiveDir, cancel)
		worker = &hlsWorker{
			content:   content,
			inputPath: inputPath,
			prefix:    prefix,
			cancel:    cancel,
		}
		hlsWorkers[content] = worker
		hlsWorkerMu.Unlock()
		go runUnifiedHLSWorker(ctx, worker)
		time.Sleep(500 * time.Millisecond)
		return worker
	}
	hlsWorkerMu.Unlock()
	if worker != nil && prefix != "" && strings.Contains(prefix, "/go-live") {
		worker.mu.Lock()
		worker.prefix = prefix
		worker.mu.Unlock()
	}
	if worker != nil && worker.mpdData == nil {
		mpdRelPath := filepath.Join(content, "manifest.mpd")
		if mpdData, err := dash.LoadMPD(infiniteOutputDir, mpdRelPath); err == nil {
			worker.mu.Lock()
			worker.mpdRelPath = mpdRelPath
			worker.mpdData = mpdData
			worker.mu.Unlock()
		}
	}
	return worker
}

func ensureDashWorker(h *Handler, content, inputPath, mpdRelPath string, mpdData *dash.MPDData) *hlsWorker {
	worker := ensureHLSWorker(h, content, inputPath, "")
	if worker == nil {
		return nil
	}
	worker.mu.Lock()
	worker.mpdRelPath = mpdRelPath
	worker.mpdData = mpdData
	worker.mu.Unlock()
	return worker
}

func parseDurationOrFallback(input string, fallback time.Duration) time.Duration {
	parsed, err := time.ParseDuration(input)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func sleepUntilNextSegment(timeOffsetSec, segSeconds, totalSeconds float64) time.Duration {
	if segSeconds <= 0 {
		segSeconds = 1
	}
	if totalSeconds <= 0 {
		return 500 * time.Millisecond
	}
	next := (math.Floor(timeOffsetSec/segSeconds) + 1) * segSeconds
	if next > totalSeconds {
		next = totalSeconds
	}
	delta := next - timeOffsetSec
	if delta < 0 {
		delta = 0
	}
	return time.Duration(delta * float64(time.Second))
}

func runUnifiedHLSWorker(ctx context.Context, worker *hlsWorker) {
	if worker == nil {
		return
	}
	content := worker.content
	inputPath := worker.inputPath
	logf("[GO-LIVE] HLS worker started: content=%s\n", content)
	logf("  Input: %s\n", inputPath)

	loader := &parser.PlaylistLoader{}
	folder := filepath.Dir(inputPath)
	filename := filepath.Base(inputPath)

	masterInfo, err := loader.LoadPlaylistInfo(folder, filename)
	if err != nil {
		logf("ERROR: Failed to load master playlist: %v\n", err)
		return
	}
	if !masterInfo.IsVariant {
		logf("ERROR: Not a variant playlist\n")
		return
	}

	minDuration, maxDuration, err := loader.GetVariantsDuration(folder, filename)
	if err != nil {
		logf("ERROR: Failed to get variant durations: %v\n", err)
		return
	}
	totalDuration := minDuration
	if totalDuration <= 0 {
		totalDuration = maxDuration
	}
	if totalDuration <= 0 {
		totalDuration = 1
	}

	audioURIs := parseAudioMediaURIs(inputPath)
	variantURIs := make([]string, 0, len(masterInfo.MasterPlaylist.Variants))
	for _, variant := range masterInfo.MasterPlaylist.Variants {
		if variant == nil {
			continue
		}
		variantURIs = append(variantURIs, variant.URI)
	}

	llMasterWritten := false
	master2sWritten := false
	master6sWritten := false
	lastLLUpdate := time.Time{}
	lastSeg2 := int64(-1)
	lastSeg6 := int64(-1)
	lastDashSeg2 := int64(-1)
	lastDashSeg6 := int64(-1)
	var recentLLTicks []struct {
		at  time.Time
		dur time.Duration
	}
	var recent2sTicks []struct {
		at  time.Time
		dur time.Duration
	}
	var recent6sTicks []struct {
		at  time.Time
		dur time.Duration
	}
	var recentDashTicks []struct {
		at  time.Time
		dur time.Duration
	}

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logf("[GO-LIVE] HLS worker stopped: content=%s\n", content)
			return
		case <-ticker.C:
		}

		now := time.Now()
		timeNow := float64(now.UnixNano()) / 1e9
		timeOffset := math.Mod(timeNow, totalDuration)
		worker.mu.Lock()
		prefix := worker.prefix
		mpdRelPath := worker.mpdRelPath
		mpdData := worker.mpdData
		worker.mu.Unlock()

		if !llMasterWritten {
			if err := writeMasterPlaylist(inputPath, filepath.Join(goLiveDir, content, "master.m3u8")); err != nil {
				logf("ERROR: Failed to write LL master playlist: %v\n", err)
			} else {
				llMasterWritten = true
			}
		}
		if !master2sWritten {
			masterData, err := os.ReadFile(inputPath)
			if err == nil {
				updated := rewriteMasterForDuration(masterData, "2s", variantURIs, audioURIs)
				if err := fileutil.WriteAtomic(durationOutputPath(content, durationMasterFilename("2s")), updated); err == nil {
					master2sWritten = true
				} else {
					logf("ERROR: Failed to write 2s master playlist: %v\n", err)
				}
			}
		}
		if !master6sWritten {
			masterData, err := os.ReadFile(inputPath)
			if err == nil {
				updated := rewriteMasterForDuration(masterData, "6s", variantURIs, audioURIs)
				if err := fileutil.WriteAtomic(durationOutputPath(content, durationMasterFilename("6s")), updated); err == nil {
					master6sWritten = true
				} else {
					logf("ERROR: Failed to write 6s master playlist: %v\n", err)
				}
			}
		}

		updatedLL := false
		if lastLLUpdate.IsZero() || now.Sub(lastLLUpdate) >= 200*time.Millisecond {
			tickStart := time.Now()
			for _, variant := range masterInfo.MasterPlaylist.Variants {
				if variant == nil {
					continue
				}
				variantInfo, byteranges, err := loader.LoadPlaylistInfoWithByteranges(folder, variant.URI)
				if err != nil {
					logf("ERROR: Failed to load variant %s: %v\n", variant.URI, err)
					continue
				}
				variantFilename := filepath.Base(variant.URI)
				if strings.Contains(variant.URI, "/") {
					variantFilename = variant.URI
				}
				variantOutputPath := filepath.Join(goLiveDir, content, variantFilename)
				os.MkdirAll(filepath.Dir(variantOutputPath), 0755)
				llhls := &generator.LLHLSGenerator{}
				playlistContent, err := llhls.GenerateVariantPlaylist(
					variantInfo.MediaPlaylist,
					byteranges,
					variantInfo.RelPath,
					variantInfo.SegmentMap,
					timeNow,
					minDuration,
					maxDuration,
				)
				if err != nil {
					logf("ERROR: Failed to generate variant %s: %v\n", variant.URI, err)
					continue
				}
				if err := fileutil.WriteAtomic(variantOutputPath, []byte(playlistContent)); err != nil {
					logf("ERROR: Failed to write variant %s: %v\n", variant.URI, err)
					continue
				}
			}
			for _, audioURI := range audioURIs {
				variantInfo, byteranges, err := loader.LoadPlaylistInfoWithByteranges(folder, audioURI)
				if err != nil {
					logf("ERROR: Failed to load audio %s: %v\n", audioURI, err)
					continue
				}
				audioOutputPath := filepath.Join(goLiveDir, content, audioURI)
				os.MkdirAll(filepath.Dir(audioOutputPath), 0755)
				llhls := &generator.LLHLSGenerator{}
				playlistContent, err := llhls.GenerateVariantPlaylist(
					variantInfo.MediaPlaylist,
					byteranges,
					variantInfo.RelPath,
					variantInfo.SegmentMap,
					timeNow,
					minDuration,
					maxDuration,
				)
				if err != nil {
					logf("ERROR: Failed to generate audio %s: %v\n", audioURI, err)
					continue
				}
				if err := fileutil.WriteAtomic(audioOutputPath, []byte(playlistContent)); err != nil {
					logf("ERROR: Failed to write audio %s: %v\n", audioURI, err)
					continue
				}
			}
			tickElapsed := time.Since(tickStart)
			recentLLTicks = append(recentLLTicks, struct {
				at  time.Time
				dur time.Duration
			}{at: time.Now(), dur: tickElapsed})
			cutoff := time.Now().Add(-5 * time.Minute)
			total := time.Duration(0)
			count := 0
			pruned := recentLLTicks[:0]
			for _, sample := range recentLLTicks {
				if sample.at.After(cutoff) {
					pruned = append(pruned, sample)
					total += sample.dur
					count++
				}
			}
			recentLLTicks = pruned
			avg := 0.0
			if count > 0 {
				avg = total.Seconds() / float64(count)
			}
			logf("[GO-LIVE:HLS][LL] tick=%.3fs avg_5m=%.3fs variants=%d audio=%d\n",
				tickElapsed.Seconds(), avg, len(masterInfo.MasterPlaylist.Variants), len(audioURIs))
			tickStatsMu.Lock()
			tickStatsByContent[content] = tickStats{
				LastTick:  tickElapsed.Seconds(),
				Avg5m:     avg,
				Variants:  len(masterInfo.MasterPlaylist.Variants),
				Audio:     len(audioURIs),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			}
			tickStatsMu.Unlock()
			lastLLUpdate = now
			updatedLL = true
			_ = updatedLL
		}

		currentSeg2 := int64(math.Floor(timeOffset / 2.0))
		if currentSeg2 != lastSeg2 {
			tickStart := time.Now()
			for _, variant := range masterInfo.MasterPlaylist.Variants {
				if variant == nil {
					continue
				}
				variantInfo, byteranges, err := loader.LoadPlaylistInfoWithByteranges(folder, variant.URI)
				if err != nil {
					logf("ERROR: Failed to load variant %s: %v\n", variant.URI, err)
					continue
				}
				variantFilename := filepath.Base(variant.URI)
				if strings.Contains(variant.URI, "/") {
					variantFilename = variant.URI
				}
				variantOutputPath := durationVariantOutputPath(content, "2s", variantFilename)
				os.MkdirAll(filepath.Dir(variantOutputPath), 0755)
				rangeGen := &generator.RangeHLSGenerator{}
				playlistContent, err := rangeGen.GenerateVariantPlaylist(
					variantInfo.MediaPlaylist,
					byteranges,
					variantInfo.RelPath,
					variantInfo.SegmentMap,
					timeNow,
					minDuration,
					maxDuration,
					"2s",
					prefix,
					content,
					totalDuration,
					timeOffset,
				)
				if err != nil {
					logf("ERROR: Failed to generate 2s variant %s: %v\n", variant.URI, err)
					continue
				}
				if err := fileutil.WriteAtomic(variantOutputPath, []byte(playlistContent)); err != nil {
					logf("ERROR: Failed to write 2s variant %s: %v\n", variant.URI, err)
					continue
				}
			}
			for _, audioURI := range audioURIs {
				variantInfo, byteranges, err := loader.LoadPlaylistInfoWithByteranges(folder, audioURI)
				if err != nil {
					logf("ERROR: Failed to load audio %s: %v\n", audioURI, err)
					continue
				}
				audioOutputPath := durationVariantOutputPath(content, "2s", audioURI)
				os.MkdirAll(filepath.Dir(audioOutputPath), 0755)
				rangeGen := &generator.RangeHLSGenerator{}
				playlistContent, err := rangeGen.GenerateVariantPlaylist(
					variantInfo.MediaPlaylist,
					byteranges,
					variantInfo.RelPath,
					variantInfo.SegmentMap,
					timeNow,
					minDuration,
					maxDuration,
					"2s",
					prefix,
					content,
					totalDuration,
					timeOffset,
				)
				if err != nil {
					logf("ERROR: Failed to generate 2s audio %s: %v\n", audioURI, err)
					continue
				}
				if err := fileutil.WriteAtomic(audioOutputPath, []byte(playlistContent)); err != nil {
					logf("ERROR: Failed to write 2s audio %s: %v\n", audioURI, err)
					continue
				}
			}
			tickElapsed := time.Since(tickStart)
			recent2sTicks = append(recent2sTicks, struct {
				at  time.Time
				dur time.Duration
			}{at: time.Now(), dur: tickElapsed})
			cutoff := time.Now().Add(-5 * time.Minute)
			total := time.Duration(0)
			count := 0
			pruned := recent2sTicks[:0]
			for _, sample := range recent2sTicks {
				if sample.at.After(cutoff) {
					pruned = append(pruned, sample)
					total += sample.dur
					count++
				}
			}
			recent2sTicks = pruned
			avg := 0.0
			if count > 0 {
				avg = total.Seconds() / float64(count)
			}
			logf("[GO-LIVE:HLS][2s] tick=%.3fs avg_5m=%.3fs\n",
				tickElapsed.Seconds(), avg)
			rangeTickMu.Lock()
			rangeTickByKey[fmt.Sprintf("%s|%s", content, "2s")] = tickStats{
				LastTick:  tickElapsed.Seconds(),
				Avg5m:     avg,
				Variants:  len(masterInfo.MasterPlaylist.Variants),
				Audio:     len(audioURIs),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			}
			rangeTickMu.Unlock()
			lastSeg2 = currentSeg2
		}

		currentSeg6 := int64(math.Floor(timeOffset / 6.0))
		if currentSeg6 != lastSeg6 {
			tickStart := time.Now()
			for _, variant := range masterInfo.MasterPlaylist.Variants {
				if variant == nil {
					continue
				}
				variantInfo, byteranges, err := loader.LoadPlaylistInfoWithByteranges(folder, variant.URI)
				if err != nil {
					logf("ERROR: Failed to load variant %s: %v\n", variant.URI, err)
					continue
				}
				variantFilename := filepath.Base(variant.URI)
				if strings.Contains(variant.URI, "/") {
					variantFilename = variant.URI
				}
				variantOutputPath := durationVariantOutputPath(content, "6s", variantFilename)
				os.MkdirAll(filepath.Dir(variantOutputPath), 0755)
				rangeGen := &generator.RangeHLSGenerator{}
				playlistContent, err := rangeGen.GenerateVariantPlaylist(
					variantInfo.MediaPlaylist,
					byteranges,
					variantInfo.RelPath,
					variantInfo.SegmentMap,
					timeNow,
					minDuration,
					maxDuration,
					"6s",
					prefix,
					content,
					totalDuration,
					timeOffset,
				)
				if err != nil {
					logf("ERROR: Failed to generate 6s variant %s: %v\n", variant.URI, err)
					continue
				}
				if err := fileutil.WriteAtomic(variantOutputPath, []byte(playlistContent)); err != nil {
					logf("ERROR: Failed to write 6s variant %s: %v\n", variant.URI, err)
					continue
				}
			}
			for _, audioURI := range audioURIs {
				variantInfo, byteranges, err := loader.LoadPlaylistInfoWithByteranges(folder, audioURI)
				if err != nil {
					logf("ERROR: Failed to load audio %s: %v\n", audioURI, err)
					continue
				}
				audioOutputPath := durationVariantOutputPath(content, "6s", audioURI)
				os.MkdirAll(filepath.Dir(audioOutputPath), 0755)
				rangeGen := &generator.RangeHLSGenerator{}
				playlistContent, err := rangeGen.GenerateVariantPlaylist(
					variantInfo.MediaPlaylist,
					byteranges,
					variantInfo.RelPath,
					variantInfo.SegmentMap,
					timeNow,
					minDuration,
					maxDuration,
					"6s",
					prefix,
					content,
					totalDuration,
					timeOffset,
				)
				if err != nil {
					logf("ERROR: Failed to generate 6s audio %s: %v\n", audioURI, err)
					continue
				}
				if err := fileutil.WriteAtomic(audioOutputPath, []byte(playlistContent)); err != nil {
					logf("ERROR: Failed to write 6s audio %s: %v\n", audioURI, err)
					continue
				}
			}
			tickElapsed := time.Since(tickStart)
			recent6sTicks = append(recent6sTicks, struct {
				at  time.Time
				dur time.Duration
			}{at: time.Now(), dur: tickElapsed})
			cutoff := time.Now().Add(-5 * time.Minute)
			total := time.Duration(0)
			count := 0
			pruned := recent6sTicks[:0]
			for _, sample := range recent6sTicks {
				if sample.at.After(cutoff) {
					pruned = append(pruned, sample)
					total += sample.dur
					count++
				}
			}
			recent6sTicks = pruned
			avg := 0.0
			if count > 0 {
				avg = total.Seconds() / float64(count)
			}
			logf("[GO-LIVE:HLS][6s] tick=%.3fs avg_5m=%.3fs\n",
				tickElapsed.Seconds(), avg)
			rangeTickMu.Lock()
			rangeTickByKey[fmt.Sprintf("%s|%s", content, "6s")] = tickStats{
				LastTick:  tickElapsed.Seconds(),
				Avg5m:     avg,
				Variants:  len(masterInfo.MasterPlaylist.Variants),
				Audio:     len(audioURIs),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			}
			rangeTickMu.Unlock()
			lastSeg6 = currentSeg6
		}

		if mpdData != nil {
			tickStart := time.Now()
			durByVariant := map[string]time.Duration{}
			liveByVariant := map[string][]byte{}

			llStart := time.Now()
			llMPD, err := dash.GenerateLiveMPD(mpdData, time.Now(), fmt.Sprintf("go-live/%s", content), 6, true)
			if err != nil {
				logf("[GO-LIVE][DASH][LL] ERROR: %v\n", err)
			} else {
				liveByVariant["ll"] = llMPD
				durByVariant["ll"] = time.Since(llStart)
				logf("[GO-LIVE:DASH][LL] generated=%.3fs bytes=%d\n",
					durByVariant["ll"].Seconds(), len(llMPD))
				dashCacheMu.Lock()
				cacheKey := dashCacheKey(content, mpdRelPath, "ll")
				entry := dashCache[cacheKey]
				if entry == nil {
					entry = &dashCacheEntry{running: true}
					dashCache[cacheKey] = entry
				}
				entry.data = llMPD
				entry.updated = time.Now()
				dashCacheMu.Unlock()
			}

			if currentSeg2 != lastDashSeg2 {
				variantStart := time.Now()
				liveMPD, err := dash.GenerateLiveMPD(mpdData, time.Now(), fmt.Sprintf("go-live/%s", content), 2, false)
				if err != nil {
					logf("[GO-LIVE][DASH][2s] ERROR: %v\n", err)
				} else {
					liveByVariant["2s"] = liveMPD
					durByVariant["2s"] = time.Since(variantStart)
					logf("[GO-LIVE:DASH][2s] generated=%.3fs bytes=%d\n",
						durByVariant["2s"].Seconds(), len(liveMPD))
					lastDashSeg2 = currentSeg2
				}
			}
			if currentSeg6 != lastDashSeg6 {
				variantStart := time.Now()
				liveMPD, err := dash.GenerateLiveMPD(mpdData, time.Now(), fmt.Sprintf("go-live/%s", content), 6, false)
				if err != nil {
					logf("[GO-LIVE][DASH][6s] ERROR: %v\n", err)
				} else {
					liveByVariant["6s"] = liveMPD
					durByVariant["6s"] = time.Since(variantStart)
					logf("[GO-LIVE:DASH][6s] generated=%.3fs bytes=%d\n",
						durByVariant["6s"].Seconds(), len(liveMPD))
					lastDashSeg6 = currentSeg6
				}
			}

			if len(liveByVariant) > 0 || llMPD != nil {
				tickElapsed := time.Since(tickStart)
				recentDashTicks = append(recentDashTicks, struct {
					at  time.Time
					dur time.Duration
				}{at: time.Now(), dur: tickElapsed})
				cutoff := time.Now().Add(-5 * time.Minute)
				total := time.Duration(0)
				count := 0
				pruned := recentDashTicks[:0]
				for _, sample := range recentDashTicks {
					if sample.at.After(cutoff) {
						pruned = append(pruned, sample)
						total += sample.dur
						count++
					}
				}
				recentDashTicks = pruned
				avg := 0.0
				if count > 0 {
					avg = total.Seconds() / float64(count)
				}

				dashTickMu.Lock()
				key := fmt.Sprintf("%s|%s", content, "ll")
				dashTickByKey[key] = tickStats{
					LastTick:  tickElapsed.Seconds(),
					Avg5m:     avg,
					UpdatedAt: time.Now().UTC().Format(time.RFC3339),
				}
				if liveByVariant["2s"] != nil {
					key = fmt.Sprintf("%s|%s", content, "2s")
					dashTickByKey[key] = tickStats{
						LastTick:  tickElapsed.Seconds(),
						Avg5m:     avg,
						UpdatedAt: time.Now().UTC().Format(time.RFC3339),
					}
				}
				if liveByVariant["6s"] != nil {
					key = fmt.Sprintf("%s|%s", content, "6s")
					dashTickByKey[key] = tickStats{
						LastTick:  tickElapsed.Seconds(),
						Avg5m:     avg,
						UpdatedAt: time.Now().UTC().Format(time.RFC3339),
					}
				}
				dashTickMu.Unlock()

				dashCacheMu.Lock()
				for name, liveMPD := range liveByVariant {
					cacheKey := dashCacheKey(content, mpdRelPath, name)
					entry := dashCache[cacheKey]
					if entry == nil {
						entry = &dashCacheEntry{running: true}
						dashCache[cacheKey] = entry
					}
					entry.data = liveMPD
					entry.updated = time.Now()
				}
				dashCacheMu.Unlock()

				logf("[GO-LIVE:DASH] tick=%.3fs content=%s ll=%.3fs 2s=%.3fs 6s=%.3fs\n",
					tickElapsed.Seconds(), content,
					durByVariant["ll"].Seconds(), durByVariant["2s"].Seconds(), durByVariant["6s"].Seconds())
			}
		}
	}
}

func variantLabel(uri string) string {
	clean := strings.TrimSuffix(uri, ".m3u8")
	clean = strings.TrimSuffix(clean, "/")
	if strings.Contains(clean, "/") {
		dir := filepath.Dir(clean)
		if dir != "." {
			return strings.ReplaceAll(filepath.ToSlash(dir), "/", "_")
		}
	}
	return filepath.Base(clean)
}

func rewriteMasterForDuration(masterData []byte, duration string, variantURIs, audioURIs []string) []byte {
	updated := string(masterData)
	for _, uri := range variantURIs {
		updated = strings.ReplaceAll(updated, uri, durationVariantFilename(duration, uri))
	}
	for _, uri := range audioURIs {
		updated = strings.ReplaceAll(updated, uri, durationVariantFilename(duration, uri))
	}
	return []byte(updated)
}

// ServeSegment serves media segment files directly from the source content directory.
func (h *Handler) ServeSegment(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	content := vars["content"]
	pathPart := vars["path"]

	segmentPath := filepath.Join(infiniteOutputDir, content, filepath.FromSlash(pathPart))
	if _, err := os.Stat(segmentPath); err != nil {
		http.Error(w, fmt.Sprintf("Segment not found: %v", err), http.StatusNotFound)
		return
	}

	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("X-Served-By", "go-live")
	http.ServeFile(w, r, segmentPath)
}

func (h *Handler) Stop(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["id"]
	h.Manager.Stop(id)
	w.Write([]byte("stopped"))
}

func (h *Handler) OnDemandDashManifest(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	content := vars["content"]
	pathPart := vars["path"]

	variant, duration, llMode, mpdPathForLoad := parseDashVariant(pathPart)
	if variant == "ll" {
		if strings.Contains(pathPart, "manifest_2s.mpd") {
			variant = "2s"
			duration = 2
			llMode = false
		} else if strings.Contains(pathPart, "manifest_6s.mpd") {
			variant = "6s"
			duration = 6
			llMode = false
		}
	}

	mpdRelPath := filepath.Clean(filepath.Join(content, mpdPathForLoad))
	mpdData, err := dash.LoadMPD(infiniteOutputDir, mpdRelPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to load MPD: %v", err), http.StatusNotFound)
		return
	}

	cacheKey := dashCacheKey(content, mpdRelPath, variant)
	dashCacheMu.Lock()
	entry := dashCache[cacheKey]
	if entry == nil {
		entry = &dashCacheEntry{}
		dashCache[cacheKey] = entry
	}
	dashCacheMu.Unlock()

	inputPath := filepath.Join(infiniteOutputDir, content, "master.m3u8")
	ensureDashWorker(h, content, inputPath, mpdRelPath, mpdData)
	h.trackRequest(r, content, "dash-"+variant)
	h.ensureTracked(content, "dash-"+variant, "hls-worker-"+content)

	dashCacheMu.Lock()
	cached := entry.data
	dashCacheMu.Unlock()

	if cached == nil {
		liveMPD, genErr := dash.GenerateLiveMPD(mpdData, time.Now(), r.URL.Path, duration, llMode)
		if genErr != nil {
			http.Error(w, fmt.Sprintf("Failed to generate MPD: %v", genErr), http.StatusInternalServerError)
			return
		}
		logf("[GO-LIVE:DASH] Generated MPD content=%s duration=%ds bytes=%d\n", content, duration, len(liveMPD))
		dashCacheMu.Lock()
		entry.data = liveMPD
		entry.updated = time.Now()
		cached = liveMPD
		dashCacheMu.Unlock()
	}

	w.Header().Set("Content-Type", "application/dash+xml")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(cached)))
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("X-Served-By", "go-live")
	w.Write(cached)
}

type dashVariantSpec struct {
	name     string
	duration int
	llMode   bool
}

type dashGenEntry struct {
	running bool
	mpdData *dash.MPDData
	cancel  context.CancelFunc
	started time.Time
}

var (
	dashGenMu      sync.Mutex
	dashGenerators = make(map[string]*dashGenEntry)
)

func dashGenKey(content, mpdRelPath string) string {
	return fmt.Sprintf("%s|%s", content, mpdRelPath)
}

func dashCacheKey(content, mpdRelPath, variant string) string {
	return fmt.Sprintf("%s|%s|%s", content, variant, mpdRelPath)
}

func parseDashVariant(pathPart string) (string, int, bool, string) {
	normalized := strings.Trim(pathPart, "/")
	parts := strings.Split(normalized, "/")
	base := parts[len(parts)-1]
	dir := strings.Join(parts[:len(parts)-1], "/")

	var variant string
	duration := 6
	llMode := true
	lookupBase := base

	switch base {
	case "manifest_2s.mpd":
		variant = "2s"
		duration = 2
		llMode = false
		lookupBase = "manifest.mpd"
	case "manifest_6s.mpd":
		variant = "6s"
		duration = 6
		llMode = false
		lookupBase = "manifest.mpd"
	case "manifest.mpd":
		variant = "ll"
	default:
		if len(parts) >= 2 && (parts[0] == "2s" || parts[0] == "6s") {
			variant = parts[0]
			llMode = false
			if parsed, err := strconv.Atoi(strings.TrimSuffix(parts[0], "s")); err == nil {
				duration = parsed
			}
			lookupBase = parts[len(parts)-1]
			dir = strings.Join(parts[1:len(parts)-1], "/")
		} else {
			variant = "ll"
			lookupBase = base
		}
	}

	if dir == "" {
		return variant, duration, llMode, lookupBase
	}
	return variant, duration, llMode, filepath.ToSlash(filepath.Join(dir, lookupBase))
}

func runDashGeneratorAll(ctx context.Context, genKey string, mpdData *dash.MPDData, content string, mpdRelPath string) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var recentTicks []struct {
		at  time.Time
		dur time.Duration
	}

	variants := []dashVariantSpec{
		{name: "ll", duration: 6, llMode: true},
		{name: "2s", duration: 2, llMode: false},
		{name: "6s", duration: 6, llMode: false},
	}

	for {
		select {
		case <-ctx.Done():
			dashGenMu.Lock()
			if entry, ok := dashGenerators[genKey]; ok {
				entry.running = false
				entry.cancel = nil
			}
			dashGenMu.Unlock()
			return
		default:
		}

		tickStart := time.Now()
		liveByVariant := make(map[string][]byte, len(variants))
		durByVariant := make(map[string]time.Duration, len(variants))
		genError := false

		llStart := time.Now()
		llMPD, err := dash.GenerateLiveMPD(mpdData, time.Now(), fmt.Sprintf("go-live/%s", content), 6, true)
		if err != nil {
			logf("[GO-LIVE:DASH] ERROR: %v\n", err)
			genError = true
		} else {
			liveByVariant["ll"] = llMPD
			durByVariant["ll"] = time.Since(llStart)
			dashCacheMu.Lock()
			cacheKey := dashCacheKey(content, mpdRelPath, "ll")
			entry := dashCache[cacheKey]
			if entry == nil {
				entry = &dashCacheEntry{running: true}
				dashCache[cacheKey] = entry
			}
			entry.data = llMPD
			entry.updated = time.Now()
			dashCacheMu.Unlock()
		}

		if !genError {
			for _, variant := range variants[1:] {
				variantStart := time.Now()
				liveMPD, err := dash.GenerateLiveMPD(mpdData, time.Now(), fmt.Sprintf("go-live/%s", content), variant.duration, variant.llMode)
				if err != nil {
					logf("[GO-LIVE:DASH] ERROR: %v\n", err)
					genError = true
					continue
				}
				liveByVariant[variant.name] = liveMPD
				durByVariant[variant.name] = time.Since(variantStart)
			}
		}

		if !genError {
			tickElapsed := time.Since(tickStart)
			recentTicks = append(recentTicks, struct {
				at  time.Time
				dur time.Duration
			}{at: time.Now(), dur: tickElapsed})
			cutoff := time.Now().Add(-5 * time.Minute)
			total := time.Duration(0)
			count := 0
			pruned := recentTicks[:0]
			for _, sample := range recentTicks {
				if sample.at.After(cutoff) {
					pruned = append(pruned, sample)
					total += sample.dur
					count++
				}
			}
			recentTicks = pruned
			avg := 0.0
			if count > 0 {
				avg = total.Seconds() / float64(count)
			}

			dashTickMu.Lock()
			for _, variant := range variants {
				key := fmt.Sprintf("%s|%s", content, variant.name)
				dashTickByKey[key] = tickStats{
					LastTick:  tickElapsed.Seconds(),
					Avg5m:     avg,
					Variants:  0,
					Audio:     0,
					UpdatedAt: time.Now().UTC().Format(time.RFC3339),
				}
			}
			dashTickMu.Unlock()

			dashCacheMu.Lock()
			for _, variant := range variants[1:] {
				liveMPD := liveByVariant[variant.name]
				if liveMPD == nil {
					continue
				}
				cacheKey := dashCacheKey(content, mpdRelPath, variant.name)
				entry := dashCache[cacheKey]
				if entry == nil {
					entry = &dashCacheEntry{running: true}
					dashCache[cacheKey] = entry
				}
				entry.data = liveMPD
				entry.updated = time.Now()
			}
			dashCacheMu.Unlock()

			logf("[GO-LIVE:DASH] Tick generation: total=%.3fs avg_5m=%.3fs content=%s ll=%.3fs 2s=%.3fs 6s=%.3fs\n",
				tickElapsed.Seconds(), avg, content,
				durByVariant["ll"].Seconds(), durByVariant["2s"].Seconds(), durByVariant["6s"].Seconds())
		}
		select {
		case <-ctx.Done():
			dashGenMu.Lock()
			if entry, ok := dashGenerators[genKey]; ok {
				entry.running = false
				entry.cancel = nil
			}
			dashGenMu.Unlock()
			return
		case <-ticker.C:
		}
	}
}

func (h *Handler) ServeDashSegment(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	content := vars["content"]
	pathPart := vars["path"]

	segmentPath := filepath.Join(infiniteOutputDir, content, filepath.FromSlash(pathPart))
	if _, err := os.Stat(segmentPath); err != nil {
		http.Error(w, fmt.Sprintf("Segment not found: %v", err), http.StatusNotFound)
		return
	}

	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("X-Served-By", "go-live")
	http.ServeFile(w, r, segmentPath)
}

func (h *Handler) Spawn(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Input  string `json:"input"`
		Output string `json:"output"`
		Mode   string `json:"mode"` // "continuous" or "once"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("invalid request"))
		return
	}

	id := uuid.New().String()
	ctx, cancel := context.WithCancel(context.Background())
	h.Manager.Spawn(id, req.Input, req.Output, cancel)

	if req.Mode == "continuous" {
		go runContinuous(ctx, req.Input, req.Output)
	} else {
		go runOnce(ctx, req.Input, req.Output)
	}

	w.Header().Set("X-Served-By", "go-live")
	json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "running"})
}

// OnDemandMasterPlaylist handles requests like /go-live/{content}/master.m3u8
// This is the main entry point matching Python's lazy continuous mode
func (h *Handler) OnDemandMasterPlaylist(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	content := vars["content"]

	// Input: VOD master playlist from dynamic_content
	inputPath := filepath.Join(infiniteOutputDir, content, "master.m3u8")

	// Check if content exists
	if _, err := os.Stat(inputPath); os.IsNotExist(err) {
		http.Error(w, fmt.Sprintf("Content not found: %s", content), http.StatusNotFound)
		return
	}

	// Load playlist info
	loader := &parser.PlaylistLoader{}
	plInfo, err := loader.LoadPlaylistInfo(infiniteOutputDir, filepath.Join(content, "master.m3u8"))
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to load playlist: %v", err), http.StatusInternalServerError)
		return
	}

	if !plInfo.IsVariant {
		// Single media playlist - generate on demand
		http.Error(w, "Single media playlists not yet supported", http.StatusNotImplemented)
		return
	}

	prefix := routePrefix(r.URL.Path)
	worker := ensureHLSWorker(h, content, inputPath, prefix)
	if worker != nil {
		h.ensureTracked(content, "hls-ll", "hls-worker-"+content)
	}
	h.trackRequest(r, content, "hls-ll")

	// Read and serve source master playlist (preserves EXT-X-MEDIA audio tags)
	data, err := os.ReadFile(inputPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to read playlist: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("X-Served-By", "go-live")
	w.Write(data)
}

// OnDemandMasterPlaylistDuration handles requests like /go-live/{content}/{duration}/master.m3u8
// Generates non-LL playlists with virtual 2s/6s segments (no partials).
func (h *Handler) OnDemandMasterPlaylistDuration(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	content := vars["content"]
	duration := vars["duration"]
	prefix := routePrefix(r.URL.Path)
	mode := "hls-" + duration

	inputPath := filepath.Join(infiniteOutputDir, content, "master.m3u8")
	if _, err := os.Stat(inputPath); os.IsNotExist(err) {
		http.Error(w, fmt.Sprintf("Content not found: %s", content), http.StatusNotFound)
		return
	}

	outputPath := durationOutputPath(content, durationMasterFilename(duration))

	loader := &parser.PlaylistLoader{}
	plInfo, err := loader.LoadPlaylistInfo(infiniteOutputDir, filepath.Join(content, "master.m3u8"))
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to load playlist: %v", err), http.StatusInternalServerError)
		return
	}
	if !plInfo.IsVariant {
		http.Error(w, "Single media playlists not yet supported", http.StatusNotImplemented)
		return
	}

	worker := ensureHLSWorker(h, content, inputPath, prefix)
	if worker != nil {
		h.ensureTracked(content, mode, "hls-worker-"+content)
	}
	h.trackRequest(r, content, mode)

	data, err := os.ReadFile(outputPath)
	if err != nil {
		time.Sleep(200 * time.Millisecond)
		data, err = os.ReadFile(outputPath)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to read playlist: %v", err), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("X-Served-By", "go-live")
	w.Write(data)
}

// OnDemandVariantPlaylist handles requests like /go-live/{content}/{variant}.m3u8
// This serves the dynamically generated variant playlists
func (h *Handler) OnDemandVariantPlaylist(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	content := vars["content"]
	variant := vars["variant"]

	if strings.HasPrefix(variant, "master_2s") || strings.HasPrefix(variant, "master_6s") {
		duration := "2s"
		if strings.HasPrefix(variant, "master_6s") {
			duration = "6s"
		}
		r = mux.SetURLVars(r, map[string]string{
			"content":  content,
			"duration": duration,
		})
		h.OnDemandMasterPlaylistDuration(w, r)
		return
	}

	if strings.HasPrefix(variant, "playlist_2s_") || strings.HasPrefix(variant, "playlist_6s_") {
		duration := "2s"
		if strings.HasPrefix(variant, "playlist_6s_") {
			duration = "6s"
		}
		r = mux.SetURLVars(r, map[string]string{
			"content":  content,
			"duration": duration,
			"variant":  variant,
		})
		h.OnDemandVariantPlaylistDuration(w, r)
		return
	}

	if isVerboseLoggingEnabled() {
		logf("LL-HLS variant request: content=%s variant=%s\n", content, variant)
	}
	prefix := routePrefix(r.URL.Path)
	inputPath := filepath.Join(infiniteOutputDir, content, "master.m3u8")
	worker := ensureHLSWorker(h, content, inputPath, prefix)
	if worker != nil {
		h.ensureTracked(content, "hls-ll", "hls-worker-"+content)
	}
	h.trackRequest(r, content, "hls-ll")

	// Build output path (served from tmpfs)
	outputPath := filepath.Join(goLiveDir, content, variant)

	data, err := os.ReadFile(outputPath)
	if err != nil {
		time.Sleep(200 * time.Millisecond)
		data, err = os.ReadFile(outputPath)
		if err != nil {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "Variant playlist not ready", http.StatusServiceUnavailable)
			return
		}
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("X-Served-By", "go-live")
	w.Write(data)
}

// OnDemandVariantPlaylistDuration handles requests like /go-live/{content}/{duration}/{variant}.m3u8
// Serves virtual 2s/6s segment playlists (no partials).
func (h *Handler) OnDemandVariantPlaylistDuration(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	content := vars["content"]
	duration := vars["duration"]
	variant := vars["variant"]
	prefix := routePrefix(r.URL.Path)
	inputPath := filepath.Join(infiniteOutputDir, content, "master.m3u8")
	worker := ensureHLSWorker(h, content, inputPath, prefix)
	if worker != nil {
		h.ensureTracked(content, "hls-"+duration, "hls-worker-"+content)
	}
	h.trackRequest(r, content, "hls-"+duration)
	outputPath := durationVariantOutputPath(content, duration, variant)
	data, err := os.ReadFile(outputPath)
	if err != nil {
		time.Sleep(200 * time.Millisecond)
		data, err = os.ReadFile(outputPath)
		if err != nil {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "Variant playlist not ready", http.StatusServiceUnavailable)
			return
		}
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("X-Served-By", "go-live")
	w.Write(data)
}

// runContinuousLLHLS runs the continuous LL-HLS playlist generator
// This matches Python generate_main() function in continuous mode
func runContinuousLLHLS(ctx context.Context, inputPath, outputPath, content string) {
	fmt.Fprintf(os.Stderr, "LL-HLS generator started for %s\n", content)
	fmt.Fprintf(os.Stderr, "  Input: %s\n", inputPath)
	fmt.Fprintf(os.Stderr, "  Output: %s\n", outputPath)

	loader := &parser.PlaylistLoader{}
	folder := filepath.Dir(inputPath)
	filename := filepath.Base(inputPath)

	// Load master playlist info
	masterInfo, err := loader.LoadPlaylistInfo(folder, filename)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Failed to load master playlist: %v\n", err)
		return
	}

	if !masterInfo.IsVariant {
		fmt.Fprintf(os.Stderr, "ERROR: Not a variant playlist\n")
		return
	}

	// Get min/max duration across all variants
	minDuration, maxDuration, err := loader.GetVariantsDuration(folder, filename)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Failed to get variant durations: %v\n", err)
		return
	}

	fmt.Fprintf(os.Stderr, "Content duration: min=%.3fs, max=%.3fs\n", minDuration, maxDuration)

	// Write master playlist once (static file)
	masterWritten := false
	iteration := 0
	audioURIs := parseAudioMediaURIs(inputPath)
	var recentTicks []struct {
		at  time.Time
		dur time.Duration
	}

	for {
		select {
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "LL-HLS generator stopped for %s\n", content)
			return
		default:
		}

		startTime := time.Now()
		tickStart := startTime
		iteration++

		// Write master playlist once
		if !masterWritten {
			if err := writeMasterPlaylist(inputPath, outputPath); err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: Failed to write master playlist: %v\n", err)
				time.Sleep(1 * time.Second)
				continue
			}
			masterWritten = true
			fmt.Fprintf(os.Stderr, "Master playlist written: %s\n", outputPath)
		}

		// Generate variant playlists (regenerated every loop)
		// Use Unix timestamp with fractional seconds (like Python's time.time())
		timeNow := float64(startTime.UnixNano()) / 1e9

		// Log timing info every 10 iterations for debugging
		if iteration%10 == 0 {
			timeOffset := math.Mod(timeNow, minDuration)
			fmt.Fprintf(os.Stderr, "[HEARTBEAT] Iteration %d, timeNow=%.3f, timeOffset=%.3f\n",
				iteration, timeNow, timeOffset)
		}

		for _, variant := range masterInfo.MasterPlaylist.Variants {
			if variant == nil {
				continue
			}

			// Load variant playlist info with byteranges
			variantInfo, byteranges, err := loader.LoadPlaylistInfoWithByteranges(folder, variant.URI)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: Failed to load variant %s: %v\n", variant.URI, err)
				continue
			}

			// Determine output filename (remove directory path, keep just filename)
			variantFilename := filepath.Base(variant.URI)
			if strings.Contains(variant.URI, "/") {
				// It's a path like "1080p/index.m3u8", keep the directory structure
				variantFilename = variant.URI
			}

			variantOutputPath := filepath.Join(filepath.Dir(outputPath), variantFilename)
			os.MkdirAll(filepath.Dir(variantOutputPath), 0755)

			// Generate LL-HLS playlist
			llhls := &generator.LLHLSGenerator{}
			playlistContent, err := llhls.GenerateVariantPlaylist(
				variantInfo.MediaPlaylist,
				byteranges,
				variantInfo.RelPath,
				variantInfo.SegmentMap,
				timeNow,
				minDuration,
				maxDuration,
			)

			if err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: Failed to generate variant %s: %v\n", variant.URI, err)
				continue
			}

			// Atomic write
			if err := fileutil.WriteAtomic(variantOutputPath, []byte(playlistContent)); err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: Failed to write variant %s: %v\n", variant.URI, err)
				continue
			}
		}

		// Generate audio playlists referenced by EXT-X-MEDIA tags
		for _, audioURI := range audioURIs {
			variantInfo, byteranges, err := loader.LoadPlaylistInfoWithByteranges(folder, audioURI)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: Failed to load audio %s: %v\n", audioURI, err)
				continue
			}

			audioOutputPath := filepath.Join(filepath.Dir(outputPath), audioURI)
			os.MkdirAll(filepath.Dir(audioOutputPath), 0755)

			llhls := &generator.LLHLSGenerator{}
			playlistContent, err := llhls.GenerateVariantPlaylist(
				variantInfo.MediaPlaylist,
				byteranges,
				variantInfo.RelPath,
				variantInfo.SegmentMap,
				timeNow,
				minDuration,
				maxDuration,
			)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: Failed to generate audio %s: %v\n", audioURI, err)
				continue
			}

			if err := fileutil.WriteAtomic(audioOutputPath, []byte(playlistContent)); err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: Failed to write audio %s: %v\n", audioURI, err)
				continue
			}
		}

		// Tick timing stats (5-minute rolling average)
		tickElapsed := time.Since(tickStart)
		recentTicks = append(recentTicks, struct {
			at  time.Time
			dur time.Duration
		}{at: time.Now(), dur: tickElapsed})
		cutoff := time.Now().Add(-5 * time.Minute)
		total := time.Duration(0)
		count := 0
		pruned := recentTicks[:0]
		for _, sample := range recentTicks {
			if sample.at.After(cutoff) {
				pruned = append(pruned, sample)
				total += sample.dur
				count++
			}
		}
		recentTicks = pruned
		avg := 0.0
		if count > 0 {
			avg = total.Seconds() / float64(count)
		}
		variantsCount := len(masterInfo.MasterPlaylist.Variants)
		audioCount := len(audioURIs)
		logf("[GO-LIVE] LL tick: %.3fs avg_5m=%.3fs variants=%d audio=%d\n",
			tickElapsed.Seconds(), avg, variantsCount, audioCount)
		tickStatsMu.Lock()
		tickStatsByContent[content] = tickStats{
			LastTick:  tickElapsed.Seconds(),
			Avg5m:     avg,
			Variants:  variantsCount,
			Audio:     audioCount,
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		tickStatsMu.Unlock()

		// Cleanup stuck temp files periodically
		if iteration%10 == 0 {
			fileutil.CleanupStuckFiles(filepath.Dir(outputPath), 3*time.Second)
		}

		// Sleep until next update (1 second interval for LL-HLS)
		elapsed := time.Since(startTime)
		sleepDuration := (1 * time.Second) - elapsed
		if sleepDuration > 0 {
			time.Sleep(sleepDuration)
		}
	}
}

func runContinuousHLSRange(ctx context.Context, inputPath, outputPath, content, duration, prefix string) {
	fmt.Fprintf(os.Stderr, "HLS range generator started for %s (%s)\n", content, duration)
	fmt.Fprintf(os.Stderr, "  Input: %s\n", inputPath)
	fmt.Fprintf(os.Stderr, "  Output: %s\n", outputPath)

	segDuration := parseDurationOrFallback(duration, 2*time.Second)
	segSeconds := segDuration.Seconds()

	loader := &parser.PlaylistLoader{}
	folder := filepath.Dir(inputPath)
	filename := filepath.Base(inputPath)

	masterInfo, err := loader.LoadPlaylistInfo(folder, filename)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Failed to load master playlist: %v\n", err)
		return
	}
	if !masterInfo.IsVariant {
		fmt.Fprintf(os.Stderr, "ERROR: Not a variant playlist\n")
		return
	}

	minDuration, maxDuration, err := loader.GetVariantsDuration(folder, filename)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Failed to get variant durations: %v\n", err)
		return
	}

	audioURIs := parseAudioMediaURIs(inputPath)
	variantURIs := make([]string, 0, len(masterInfo.MasterPlaylist.Variants))
	for _, variant := range masterInfo.MasterPlaylist.Variants {
		if variant == nil {
			continue
		}
		variantURIs = append(variantURIs, variant.URI)
	}
	var recentTicks []struct {
		at  time.Time
		dur time.Duration
	}
	masterWritten := false
	lastSegIndex := int64(-1)
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "HLS range generator stopped for %s (%s)\n", content, duration)
			return
		default:
		}

		startTime := time.Now()
		timeNow := float64(startTime.UnixNano()) / 1e9
		timeOffset := math.Mod(timeNow, minDuration)
		currentSegIndex := int64(math.Floor(timeOffset / segSeconds))

		if !masterWritten {
			masterData, err := os.ReadFile(inputPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: Failed to read master playlist: %v\n", err)
				time.Sleep(1 * time.Second)
				continue
			}
			updated := rewriteMasterForDuration(masterData, duration, variantURIs, audioURIs)
			os.MkdirAll(filepath.Dir(outputPath), 0755)
			if err := fileutil.WriteAtomic(outputPath, updated); err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: Failed to write duration master playlist: %v\n", err)
				time.Sleep(1 * time.Second)
				continue
			}
			masterWritten = true
		}

		if currentSegIndex == lastSegIndex {
			sleepDuration := sleepUntilNextSegment(timeOffset, segSeconds, minDuration)
			if sleepDuration < 50*time.Millisecond {
				sleepDuration = 50 * time.Millisecond
			}
			select {
			case <-ctx.Done():
				fmt.Fprintf(os.Stderr, "HLS range generator stopped for %s (%s)\n", content, duration)
				return
			case <-time.After(sleepDuration):
				continue
			}
		}

		lastSegIndex = currentSegIndex

		for _, variant := range masterInfo.MasterPlaylist.Variants {
			if variant == nil {
				continue
			}

			variantInfo, byteranges, err := loader.LoadPlaylistInfoWithByteranges(folder, variant.URI)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: Failed to load variant %s: %v\n", variant.URI, err)
				continue
			}

			variantFilename := filepath.Base(variant.URI)
			if strings.Contains(variant.URI, "/") {
				variantFilename = variant.URI
			}

			variantOutputPath := durationVariantOutputPath(content, duration, variantFilename)
			os.MkdirAll(filepath.Dir(variantOutputPath), 0755)

			rangeGen := &generator.RangeHLSGenerator{}
			playlistContent, err := rangeGen.GenerateVariantPlaylist(
				variantInfo.MediaPlaylist,
				byteranges,
				variantInfo.RelPath,
				variantInfo.SegmentMap,
				timeNow,
				minDuration,
				maxDuration,
				duration,
				prefix,
				content,
				minDuration,
				timeOffset,
			)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: Failed to generate range variant %s: %v\n", variant.URI, err)
				continue
			}

			if err := fileutil.WriteAtomic(variantOutputPath, []byte(playlistContent)); err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: Failed to write range variant %s: %v\n", variant.URI, err)
				continue
			}
		}

		for _, audioURI := range audioURIs {
			variantInfo, byteranges, err := loader.LoadPlaylistInfoWithByteranges(folder, audioURI)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: Failed to load audio %s: %v\n", audioURI, err)
				continue
			}

			audioOutputPath := durationVariantOutputPath(content, duration, audioURI)
			os.MkdirAll(filepath.Dir(audioOutputPath), 0755)

			rangeGen := &generator.RangeHLSGenerator{}
			playlistContent, err := rangeGen.GenerateVariantPlaylist(
				variantInfo.MediaPlaylist,
				byteranges,
				variantInfo.RelPath,
				variantInfo.SegmentMap,
				timeNow,
				minDuration,
				maxDuration,
				duration,
				prefix,
				content,
				minDuration,
				timeOffset,
			)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: Failed to generate range audio %s: %v\n", audioURI, err)
				continue
			}

			if err := fileutil.WriteAtomic(audioOutputPath, []byte(playlistContent)); err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: Failed to write range audio %s: %v\n", audioURI, err)
				continue
			}
		}

		tickElapsed := time.Since(startTime)
		recentTicks = append(recentTicks, struct {
			at  time.Time
			dur time.Duration
		}{at: time.Now(), dur: tickElapsed})
		cutoff := time.Now().Add(-5 * time.Minute)
		total := time.Duration(0)
		count := 0
		pruned := recentTicks[:0]
		for _, sample := range recentTicks {
			if sample.at.After(cutoff) {
				pruned = append(pruned, sample)
				total += sample.dur
				count++
			}
		}
		recentTicks = pruned
		avg := 0.0
		if count > 0 {
			avg = total.Seconds() / float64(count)
		}
		logf("[GO-LIVE] HLS %s tick: %.3fs avg_5m=%.3fs\n",
			duration, tickElapsed.Seconds(), avg)
		rangeTickMu.Lock()
		rangeTickByKey[fmt.Sprintf("%s|%s", content, duration)] = tickStats{
			LastTick:  tickElapsed.Seconds(),
			Avg5m:     avg,
			Variants:  len(masterInfo.MasterPlaylist.Variants),
			Audio:     len(audioURIs),
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		rangeTickMu.Unlock()

		sleepDuration := sleepUntilNextSegment(timeOffset, segSeconds, minDuration)
		if sleepDuration < 50*time.Millisecond {
			sleepDuration = 50 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "HLS range generator stopped for %s (%s)\n", content, duration)
			return
		case <-time.After(sleepDuration):
		}
	}
}

// writeMasterPlaylist writes the master playlist file
func writeMasterPlaylist(sourcePath, outputPath string) error {
	os.MkdirAll(filepath.Dir(outputPath), 0755)

	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return err
	}
	return os.WriteFile(outputPath, data, 0644)
}

// Legacy functions for backwards compatibility (not used in LL-HLS mode)

func runContinuous(ctx context.Context, input, output string) {
	// Legacy continuous mode - not used for LL-HLS
	fmt.Fprintf(os.Stderr, "WARN: Legacy runContinuous called\n")
}

func runOnce(ctx context.Context, input, output string) {
	// Legacy once mode - not used for LL-HLS
	fmt.Fprintf(os.Stderr, "WARN: Legacy runOnce called\n")
}

func (h *Handler) trackRequest(r *http.Request, content, mode string) {
	if h == nil || h.Tracker == nil || r == nil {
		return
	}
	meta := requestMeta(r.RemoteAddr, r.Header.Get("X-Forwarded-For"), r.UserAgent())
	h.Tracker.RecordRequest(content, mode, r.URL.Path, clientKey(&meta), time.Now())
}

func (h *Handler) ensureTracked(content, mode, processID string) {
	if h == nil || h.Tracker == nil {
		return
	}
	h.Tracker.Start(content, mode, processID, time.Now())
}

func (h *Handler) ensureDashTracked(content, variant, genKey string) {
	if h == nil || h.Tracker == nil {
		return
	}
	mode := "dash-" + variant
	h.Tracker.Start(content, mode, "dash-"+genKey, time.Now())
}

func (h *Handler) decorateStreamStats(status StreamStatus) StreamStatus {
	mode := status.Mode
	content := status.Content
	switch {
	case strings.HasPrefix(mode, "dash-"):
		dashTickMu.RLock()
		stats, ok := dashTickByKey[fmt.Sprintf("%s|%s", content, strings.TrimPrefix(mode, "dash-"))]
		dashTickMu.RUnlock()
		if ok {
			status.LastTick = stats.LastTick
			status.Avg5m = stats.Avg5m
		}
	case strings.HasPrefix(mode, "hls-") && mode != "hls-ll":
		rangeTickMu.RLock()
		stats, ok := rangeTickByKey[fmt.Sprintf("%s|%s", content, strings.TrimPrefix(mode, "hls-"))]
		rangeTickMu.RUnlock()
		if ok {
			status.LastTick = stats.LastTick
			status.Avg5m = stats.Avg5m
		}
	case mode == "hls-ll":
		tickStatsMu.RLock()
		stats, ok := tickStatsByContent[content]
		tickStatsMu.RUnlock()
		if ok {
			status.LastTick = stats.LastTick
			status.Avg5m = stats.Avg5m
		}
	}
	return status
}

func (h *Handler) StartIdleReaper() {
	if h == nil || h.Tracker == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now()
			idleDash := h.Tracker.IdleEntries(now)
			for _, entry := range idleDash {
				if entry.ProcessID == "" || !strings.HasPrefix(entry.ProcessID, "dash-") {
					continue
				}
					logf("[GO-LIVE] Stopping idle DASH generator: content=%s mode=%s idle=%s\n",
						entry.Content, entry.Mode, formatSeconds(now.Sub(entry.LastRequest)))
				stopDashGenerator(strings.TrimPrefix(entry.ProcessID, "dash-"))
				h.Tracker.Remove(entry.Content, entry.Mode)
			}

			idleHLS := h.Tracker.IdleContentEntries(now)
			for _, entry := range idleHLS {
				if entry.ProcessID == "" {
					continue
				}
				logf("[GO-LIVE] Stopping idle HLS worker: content=%s idle=%s\n",
					entry.Content, formatSeconds(now.Sub(entry.LastRequest)))
				h.Manager.Stop(entry.ProcessID)
				h.Tracker.RemoveContentModePrefix(entry.Content, "hls-")
				h.Tracker.RemoveContentModePrefix(entry.Content, "dash-")
				hlsWorkerMu.Lock()
				delete(hlsWorkers, entry.Content)
				hlsWorkerMu.Unlock()
			}
		}
	}()
}

func stopDashGenerator(genKey string) {
	dashGenMu.Lock()
	entry := dashGenerators[genKey]
	if entry != nil {
		if entry.cancel != nil {
			entry.cancel()
		}
		delete(dashGenerators, genKey)
	}
	dashGenMu.Unlock()
}
