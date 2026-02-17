package output

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/boss/abrchar/pkg/analysis"
)

// JSONSummary represents the machine-readable JSON output
type JSONSummary struct {
	ExperimentStart string                       `json:"experiment_start"`
	ExperimentEnd   string                       `json:"experiment_end"`
	TotalSwitches   int                          `json:"total_switches"`
	Variants        []VariantInfo                `json:"variants"`
	Boundaries      []BoundaryInfo               `json:"boundaries"`
}

// VariantInfo represents a variant in the summary
type VariantInfo struct {
	Index            int     `json:"index"`
	Bandwidth        float64 `json:"bandwidth_mbps"`
	AverageBandwidth float64 `json:"average_bandwidth_mbps,omitempty"`
	Resolution       string  `json:"resolution,omitempty"`
}

// BoundaryInfo represents metrics for a boundary in the summary
type BoundaryInfo struct {
	LowerVariantIdx    int                  `json:"lower_variant_idx"`
	UpperVariantIdx    int                  `json:"upper_variant_idx"`
	LowerBandwidthMbps float64              `json:"lower_bandwidth_mbps"`
	UpperBandwidthMbps float64              `json:"upper_bandwidth_mbps"`
	Downswitch         ThresholdMetrics     `json:"downswitch"`
	Upswitch           ThresholdMetrics     `json:"upswitch"`
}

// ThresholdMetrics represents statistics for a threshold
type ThresholdMetrics struct {
	Count          int                      `json:"count"`
	Thresholds     analysis.Statistics      `json:"thresholds"`
	SafetyFactors  analysis.Statistics      `json:"safety_factors"`
}

// WriteJSONSummary writes the analysis results as JSON
func WriteJSONSummary(result *analysis.AnalysisResult, outputPath string) error {
	summary := JSONSummary{
		ExperimentStart: result.ExperimentStart.Format("2006-01-02T15:04:05Z07:00"),
		ExperimentEnd:   result.ExperimentEnd.Format("2006-01-02T15:04:05Z07:00"),
		TotalSwitches:   result.TotalSwitches,
		Variants:        []VariantInfo{},
		Boundaries:      []BoundaryInfo{},
	}
	
	// Add variant info
	for i, variant := range result.Ladder.Variants {
		info := VariantInfo{
			Index:      i,
			Bandwidth:  variant.GetBandwidthMbps(),
			Resolution: variant.Resolution,
		}
		if variant.AverageBandwidth > 0 {
			info.AverageBandwidth = variant.GetAverageBandwidthMbps()
		}
		summary.Variants = append(summary.Variants, info)
	}
	
	// Add boundary metrics
	for _, bm := range result.BoundaryMetrics {
		info := BoundaryInfo{
			LowerVariantIdx:    bm.LowerVariantIdx,
			UpperVariantIdx:    bm.UpperVariantIdx,
			LowerBandwidthMbps: bm.LowerBandwidthMbps,
			UpperBandwidthMbps: bm.UpperBandwidthMbps,
			Downswitch: ThresholdMetrics{
				Count:         bm.DownswitchCount,
				Thresholds:    analysis.ComputeStatistics(bm.DownswitchThresholds),
				SafetyFactors: analysis.ComputeStatistics(bm.DownswitchSafetyFactors),
			},
			Upswitch: ThresholdMetrics{
				Count:         bm.UpswitchCount,
				Thresholds:    analysis.ComputeStatistics(bm.UpswitchThresholds),
				SafetyFactors: analysis.ComputeStatistics(bm.UpswitchSafetyFactors),
			},
		}
		summary.Boundaries = append(summary.Boundaries, info)
	}
	
	// Write to file
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}
	
	if err := os.WriteFile(outputPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write JSON file: %w", err)
	}
	
	return nil
}

// WriteMarkdownReport writes the analysis results as a Markdown report
func WriteMarkdownReport(result *analysis.AnalysisResult, outputPath string) error {
	var sb strings.Builder
	
	sb.WriteString("# ABR Characterization Report\n\n")
	
	// Experiment summary
	sb.WriteString("## Experiment Summary\n\n")
	sb.WriteString(fmt.Sprintf("- **Start:** %s\n", result.ExperimentStart.Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("- **End:** %s\n", result.ExperimentEnd.Format("2006-01-02 15:04:05")))
	duration := result.ExperimentEnd.Sub(result.ExperimentStart)
	sb.WriteString(fmt.Sprintf("- **Duration:** %.0f seconds\n", duration.Seconds()))
	sb.WriteString(fmt.Sprintf("- **Total Switches:** %d\n\n", result.TotalSwitches))
	
	// Bitrate ladder
	sb.WriteString("## Bitrate Ladder\n\n")
	sb.WriteString("| Index | Bandwidth | Avg Bandwidth | Resolution |\n")
	sb.WriteString("|-------|-----------|---------------|------------|\n")
	for i, variant := range result.Ladder.Variants {
		avgBw := "—"
		if variant.AverageBandwidth > 0 {
			avgBw = fmt.Sprintf("%.2f Mbps", variant.GetAverageBandwidthMbps())
		}
		res := variant.Resolution
		if res == "" {
			res = "—"
		}
		sb.WriteString(fmt.Sprintf("| %d | %.2f Mbps | %s | %s |\n",
			i, variant.GetBandwidthMbps(), avgBw, res))
	}
	sb.WriteString("\n")
	
	// Boundary metrics
	sb.WriteString("## Boundary Metrics\n\n")
	
	for _, bm := range result.BoundaryMetrics {
		sb.WriteString(fmt.Sprintf("### Boundary: Variant %d ↔ Variant %d\n\n",
			bm.LowerVariantIdx, bm.UpperVariantIdx))
		sb.WriteString(fmt.Sprintf("**Bitrates:** %.2f Mbps ↔ %.2f Mbps\n\n",
			bm.LowerBandwidthMbps, bm.UpperBandwidthMbps))
		
		// Downswitch metrics
		if bm.DownswitchCount > 0 {
			sb.WriteString("#### Downswitch (High → Low)\n\n")
			sb.WriteString(fmt.Sprintf("**Count:** %d\n\n", bm.DownswitchCount))
			
			threshStats := analysis.ComputeStatistics(bm.DownswitchThresholds)
			sb.WriteString("**Throughput Thresholds:**\n\n")
			sb.WriteString(fmt.Sprintf("- Mean: %.2f Mbps\n", threshStats.Mean))
			sb.WriteString(fmt.Sprintf("- Median: %.2f Mbps\n", threshStats.Median))
			sb.WriteString(fmt.Sprintf("- StdDev: %.2f Mbps\n", threshStats.StdDev))
			sb.WriteString(fmt.Sprintf("- Range: %.2f - %.2f Mbps\n\n", threshStats.Min, threshStats.Max))
			
			safetyStats := analysis.ComputeStatistics(bm.DownswitchSafetyFactors)
			sb.WriteString("**Safety Factors (α = variant_bw / throughput):**\n\n")
			sb.WriteString(fmt.Sprintf("- Mean: %.2f\n", safetyStats.Mean))
			sb.WriteString(fmt.Sprintf("- Median: %.2f\n", safetyStats.Median))
			sb.WriteString(fmt.Sprintf("- StdDev: %.2f\n", safetyStats.StdDev))
			sb.WriteString(fmt.Sprintf("- Range: %.2f - %.2f\n\n", safetyStats.Min, safetyStats.Max))
		}
		
		// Upswitch metrics
		if bm.UpswitchCount > 0 {
			sb.WriteString("#### Upswitch (Low → High)\n\n")
			sb.WriteString(fmt.Sprintf("**Count:** %d\n\n", bm.UpswitchCount))
			
			threshStats := analysis.ComputeStatistics(bm.UpswitchThresholds)
			sb.WriteString("**Throughput Thresholds:**\n\n")
			sb.WriteString(fmt.Sprintf("- Mean: %.2f Mbps\n", threshStats.Mean))
			sb.WriteString(fmt.Sprintf("- Median: %.2f Mbps\n", threshStats.Median))
			sb.WriteString(fmt.Sprintf("- StdDev: %.2f Mbps\n", threshStats.StdDev))
			sb.WriteString(fmt.Sprintf("- Range: %.2f - %.2f Mbps\n\n", threshStats.Min, threshStats.Max))
			
			safetyStats := analysis.ComputeStatistics(bm.UpswitchSafetyFactors)
			sb.WriteString("**Safety Factors (α = variant_bw / throughput):**\n\n")
			sb.WriteString(fmt.Sprintf("- Mean: %.2f\n", safetyStats.Mean))
			sb.WriteString(fmt.Sprintf("- Median: %.2f\n", safetyStats.Median))
			sb.WriteString(fmt.Sprintf("- StdDev: %.2f\n", safetyStats.StdDev))
			sb.WriteString(fmt.Sprintf("- Range: %.2f - %.2f\n\n", safetyStats.Min, safetyStats.Max))
		}
		
		// Hysteresis analysis
		if bm.DownswitchCount > 0 && bm.UpswitchCount > 0 {
			downStats := analysis.ComputeStatistics(bm.DownswitchThresholds)
			upStats := analysis.ComputeStatistics(bm.UpswitchThresholds)
			sb.WriteString("#### Hysteresis\n\n")
			sb.WriteString(fmt.Sprintf("- Downswitch median: %.2f Mbps\n", downStats.Median))
			sb.WriteString(fmt.Sprintf("- Upswitch median: %.2f Mbps\n", upStats.Median))
			hysteresis := upStats.Median - downStats.Median
			sb.WriteString(fmt.Sprintf("- **Hysteresis:** %.2f Mbps\n\n", hysteresis))
		}
		
		sb.WriteString("---\n\n")
	}
	
	// Key conclusions
	sb.WriteString("## Key Conclusions\n\n")
	
	if len(result.BoundaryMetrics) > 0 {
		sb.WriteString("### Safety Factor Summary\n\n")
		sb.WriteString("| Boundary | Direction | Median α | Mean α | StdDev α |\n")
		sb.WriteString("|----------|-----------|----------|--------|----------|\n")
		
		for _, bm := range result.BoundaryMetrics {
			if bm.DownswitchCount > 0 {
				stats := analysis.ComputeStatistics(bm.DownswitchSafetyFactors)
				sb.WriteString(fmt.Sprintf("| %d→%d | Down | %.2f | %.2f | %.2f |\n",
					bm.LowerVariantIdx, bm.UpperVariantIdx, stats.Median, stats.Mean, stats.StdDev))
			}
			if bm.UpswitchCount > 0 {
				stats := analysis.ComputeStatistics(bm.UpswitchSafetyFactors)
				sb.WriteString(fmt.Sprintf("| %d→%d | Up | %.2f | %.2f | %.2f |\n",
					bm.LowerVariantIdx, bm.UpperVariantIdx, stats.Median, stats.Mean, stats.StdDev))
			}
		}
		sb.WriteString("\n")
	}
	
	// Write to file
	if err := os.WriteFile(outputPath, []byte(sb.String()), 0644); err != nil {
		return fmt.Errorf("failed to write markdown file: %w", err)
	}
	
	return nil
}
