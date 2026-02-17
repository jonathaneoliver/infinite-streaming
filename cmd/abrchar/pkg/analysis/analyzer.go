package analysis

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/boss/abrchar/pkg/playlist"
	"github.com/boss/abrchar/pkg/telemetry"
)

// SwitchEvent represents a detected variant switch
type SwitchEvent struct {
	Timestamp        time.Time
	FromVariantIdx   int     // Index in the ladder
	ToVariantIdx     int     // Index in the ladder
	FromBandwidthMbps float64
	ToBandwidthMbps  float64
	BufferDepth      float64 // Buffer depth at switch time (seconds)
	EstThroughputMbps float64 // Estimated throughput at switch time
	Direction        string  // "up" or "down"
}

// BoundaryMetrics represents metrics for switches between two adjacent variants
type BoundaryMetrics struct {
	LowerVariantIdx  int     // Lower bandwidth variant index
	UpperVariantIdx  int     // Upper bandwidth variant index
	LowerBandwidthMbps float64
	UpperBandwidthMbps float64
	
	// Downswitch metrics (upper -> lower)
	DownswitchCount      int
	DownswitchThresholds []float64 // Measured throughput at each downswitch
	DownswitchSafetyFactors []float64 // alpha = variant_bw / throughput
	
	// Upswitch metrics (lower -> upper)
	UpswitchCount      int
	UpswitchThresholds []float64
	UpswitchSafetyFactors []float64
}

// Statistics contains summary statistics for a set of values
type Statistics struct {
	Count  int
	Mean   float64
	Median float64
	StdDev float64
	Min    float64
	Max    float64
	P25    float64 // 25th percentile
	P75    float64 // 75th percentile
}

// AnalysisResult contains the full analysis of an ABR experiment
type AnalysisResult struct {
	Ladder           *playlist.Ladder
	SwitchEvents     []SwitchEvent
	BoundaryMetrics  map[string]*BoundaryMetrics // key: "idx1->idx2"
	ExperimentStart  time.Time
	ExperimentEnd    time.Time
	TotalSwitches    int
}

// Analyzer performs ABR behavior analysis
type Analyzer struct {
	Ladder              *playlist.Ladder
	ThroughputWindowSize int // Number of recent downloads to consider for throughput estimation
}

// NewAnalyzer creates a new analyzer
func NewAnalyzer(ladder *playlist.Ladder, throughputWindowSize int) *Analyzer {
	if throughputWindowSize <= 0 {
		throughputWindowSize = 5 // Default window size
	}
	return &Analyzer{
		Ladder:              ladder,
		ThroughputWindowSize: throughputWindowSize,
	}
}

// Analyze performs full analysis on telemetry data
func (a *Analyzer) Analyze(events []telemetry.Event, downloads []telemetry.SegmentDownload) (*AnalysisResult, error) {
	result := &AnalysisResult{
		Ladder:          a.Ladder,
		BoundaryMetrics: make(map[string]*BoundaryMetrics),
	}
	
	// Detect switch events
	switchEvents := a.detectSwitches(events, downloads)
	result.SwitchEvents = switchEvents
	result.TotalSwitches = len(switchEvents)
	
	// Set experiment time range
	if len(events) > 0 {
		result.ExperimentStart = events[0].Timestamp
		result.ExperimentEnd = events[len(events)-1].Timestamp
	}
	
	// Compute boundary metrics
	result.BoundaryMetrics = a.computeBoundaryMetrics(switchEvents)
	
	return result, nil
}

// detectSwitches detects variant switch events from telemetry
func (a *Analyzer) detectSwitches(events []telemetry.Event, downloads []telemetry.SegmentDownload) []SwitchEvent {
	var switches []SwitchEvent
	
	if len(events) < 2 {
		return switches
	}
	
	// Build throughput time series from downloads
	throughputMap := a.buildThroughputTimeSeries(downloads)
	
	var lastVariantBitrate float64
	var lastVariantIdx int = -1
	
	for _, event := range events {
		if event.VariantBitrate == 0 {
			continue
		}
		
		// Find variant index for this bitrate
		variantIdx := a.findVariantIndexByBitrate(event.VariantBitrate)
		if variantIdx < 0 {
			continue
		}
		
		// Check if variant changed
		if lastVariantIdx >= 0 && variantIdx != lastVariantIdx {
			// Get estimated throughput at switch time
			estThroughput := a.getEstimatedThroughput(throughputMap, event.Timestamp)
			
			direction := "down"
			if variantIdx > lastVariantIdx {
				direction = "up"
			}
			
			switchEvent := SwitchEvent{
				Timestamp:         event.Timestamp,
				FromVariantIdx:    lastVariantIdx,
				ToVariantIdx:      variantIdx,
				FromBandwidthMbps: lastVariantBitrate,
				ToBandwidthMbps:   event.VariantBitrate,
				BufferDepth:       event.BufferDepth,
				EstThroughputMbps: estThroughput,
				Direction:         direction,
			}
			
			switches = append(switches, switchEvent)
		}
		
		lastVariantBitrate = event.VariantBitrate
		lastVariantIdx = variantIdx
	}
	
	return switches
}

// buildThroughputTimeSeries builds a time series of throughput estimates
func (a *Analyzer) buildThroughputTimeSeries(downloads []telemetry.SegmentDownload) map[time.Time]float64 {
	throughputMap := make(map[time.Time]float64)
	
	// Use sliding window EWMA
	window := make([]float64, 0, a.ThroughputWindowSize)
	
	for _, dl := range downloads {
		if dl.ThroughputMbps > 0 {
			window = append(window, dl.ThroughputMbps)
			if len(window) > a.ThroughputWindowSize {
				window = window[1:]
			}
			
			// Calculate median of window
			ewma := calculateMedian(window)
			throughputMap[dl.EndTime] = ewma
		}
	}
	
	return throughputMap
}

// getEstimatedThroughput gets the estimated throughput at a given time
func (a *Analyzer) getEstimatedThroughput(throughputMap map[time.Time]float64, t time.Time) float64 {
	// Find the closest measurement before or at time t
	var closestTime time.Time
	var closestValue float64
	minDiff := time.Hour * 24 // Large initial value
	
	for ts, value := range throughputMap {
		diff := t.Sub(ts)
		if diff >= 0 && diff < minDiff {
			minDiff = diff
			closestTime = ts
			closestValue = value
		}
	}
	
	if closestTime.IsZero() {
		return 0
	}
	
	return closestValue
}

// findVariantIndexByBitrate finds the variant index that matches the given bitrate
func (a *Analyzer) findVariantIndexByBitrate(bitrateMbps float64) int {
	tolerance := 0.1 // 0.1 Mbps tolerance
	
	for i, variant := range a.Ladder.Variants {
		vBitrate := variant.GetAverageBandwidthMbps()
		if math.Abs(vBitrate-bitrateMbps) < tolerance {
			return i
		}
		
		// Also check regular bandwidth
		vBitrate = variant.GetBandwidthMbps()
		if math.Abs(vBitrate-bitrateMbps) < tolerance {
			return i
		}
	}
	
	return -1
}

// computeBoundaryMetrics computes metrics for each boundary
func (a *Analyzer) computeBoundaryMetrics(switches []SwitchEvent) map[string]*BoundaryMetrics {
	boundaries := make(map[string]*BoundaryMetrics)
	
	for _, sw := range switches {
		// Determine boundary key (always lower -> upper)
		lowerIdx := sw.FromVariantIdx
		upperIdx := sw.ToVariantIdx
		if sw.Direction == "up" {
			lowerIdx = sw.FromVariantIdx
			upperIdx = sw.ToVariantIdx
		} else {
			lowerIdx = sw.ToVariantIdx
			upperIdx = sw.FromVariantIdx
		}
		
		key := fmt.Sprintf("%d->%d", lowerIdx, upperIdx)
		
		if _, exists := boundaries[key]; !exists {
			boundaries[key] = &BoundaryMetrics{
				LowerVariantIdx:    lowerIdx,
				UpperVariantIdx:    upperIdx,
				LowerBandwidthMbps: a.Ladder.Variants[lowerIdx].GetAverageBandwidthMbps(),
				UpperBandwidthMbps: a.Ladder.Variants[upperIdx].GetAverageBandwidthMbps(),
			}
		}
		
		bm := boundaries[key]
		
		if sw.Direction == "down" {
			bm.DownswitchCount++
			bm.DownswitchThresholds = append(bm.DownswitchThresholds, sw.EstThroughputMbps)
			
			// Calculate safety factor: alpha = variant_bandwidth / throughput
			if sw.EstThroughputMbps > 0 {
				alpha := bm.UpperBandwidthMbps / sw.EstThroughputMbps
				bm.DownswitchSafetyFactors = append(bm.DownswitchSafetyFactors, alpha)
			}
		} else {
			bm.UpswitchCount++
			bm.UpswitchThresholds = append(bm.UpswitchThresholds, sw.EstThroughputMbps)
			
			if sw.EstThroughputMbps > 0 {
				alpha := bm.UpperBandwidthMbps / sw.EstThroughputMbps
				bm.UpswitchSafetyFactors = append(bm.UpswitchSafetyFactors, alpha)
			}
		}
	}
	
	return boundaries
}

// ComputeStatistics computes statistics for a set of values
func ComputeStatistics(values []float64) Statistics {
	if len(values) == 0 {
		return Statistics{}
	}
	
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)
	
	// Mean
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	mean := sum / float64(len(values))
	
	// Median
	median := sorted[len(sorted)/2]
	if len(sorted)%2 == 0 {
		median = (sorted[len(sorted)/2-1] + sorted[len(sorted)/2]) / 2
	}
	
	// Standard deviation
	variance := 0.0
	for _, v := range values {
		diff := v - mean
		variance += diff * diff
	}
	stdDev := math.Sqrt(variance / float64(len(values)))
	
	// Percentiles
	p25 := sorted[len(sorted)/4]
	p75 := sorted[len(sorted)*3/4]
	
	return Statistics{
		Count:  len(values),
		Mean:   mean,
		Median: median,
		StdDev: stdDev,
		Min:    sorted[0],
		Max:    sorted[len(sorted)-1],
		P25:    p25,
		P75:    p75,
	}
}

// calculateMedian calculates the median of a slice
func calculateMedian(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)
	
	mid := len(sorted) / 2
	if len(sorted)%2 == 0 {
		return (sorted[mid-1] + sorted[mid]) / 2
	}
	return sorted[mid]
}
