package playlist

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Variant represents a single HLS variant stream
type Variant struct {
	URI              string  // Relative or absolute URI to the variant playlist
	Bandwidth        int64   // BANDWIDTH attribute (required, bits per second)
	AverageBandwidth int64   // AVERAGE-BANDWIDTH attribute (optional, bits per second)
	Resolution       string  // RESOLUTION attribute (e.g., "1920x1080")
	Codecs           string  // CODECS attribute
	FrameRate        float64 // FRAME-RATE attribute
	Index            int     // Index in the original playlist order
}

// Ladder represents a parsed HLS multivariant playlist with ordered variants
type Ladder struct {
	Variants []Variant // Variants ordered by bandwidth (ascending)
	BaseURL  string    // Base URL for resolving relative URIs
}

var (
	bandwidthRegex    = regexp.MustCompile(`BANDWIDTH=(\d+)`)
	avgBandwidthRegex = regexp.MustCompile(`AVERAGE-BANDWIDTH=(\d+)`)
	resolutionRegex   = regexp.MustCompile(`RESOLUTION=(\d+x\d+)`)
	codecsRegex       = regexp.MustCompile(`CODECS="([^"]+)"`)
	frameRateRegex    = regexp.MustCompile(`FRAME-RATE=([\d.]+)`)
)

// ParseHLSMaster parses an HLS multivariant (master) playlist from a URL or file path
func ParseHLSMaster(urlOrPath string) (*Ladder, error) {
	// Check if it's a local file path
	if strings.HasPrefix(urlOrPath, "file://") || !strings.Contains(urlOrPath, "://") {
		// Local file path
		filePath := strings.TrimPrefix(urlOrPath, "file://")
		file, err := os.Open(filePath)
		if err != nil {
			return nil, fmt.Errorf("failed to open file: %w", err)
		}
		defer file.Close()
		
		return ParseHLSMasterFromReader(file, urlOrPath)
	}
	
	// HTTP(S) URL
	resp, err := http.Get(urlOrPath)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch master playlist: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP error fetching master playlist: %d", resp.StatusCode)
	}

	return ParseHLSMasterFromReader(resp.Body, urlOrPath)
}

// ParseHLSMasterFromReader parses an HLS multivariant playlist from an io.Reader
func ParseHLSMasterFromReader(r io.Reader, baseURL string) (*Ladder, error) {
	ladder := &Ladder{
		BaseURL:  baseURL,
		Variants: []Variant{},
	}

	scanner := bufio.NewScanner(r)
	variantIndex := 0
	var currentStreamInf string

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and comments that aren't EXT-X-STREAM-INF
		if line == "" {
			continue
		}

		// Check for EXT-X-STREAM-INF
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			currentStreamInf = line
			continue
		}

		// If we have a pending STREAM-INF and this is a URI line
		if currentStreamInf != "" && !strings.HasPrefix(line, "#") {
			variant := parseVariant(currentStreamInf, line, variantIndex)
			ladder.Variants = append(ladder.Variants, variant)
			variantIndex++
			currentStreamInf = ""
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading master playlist: %w", err)
	}

	if len(ladder.Variants) == 0 {
		return nil, fmt.Errorf("no variants found in master playlist")
	}

	// Sort variants by bandwidth (ascending)
	sort.Slice(ladder.Variants, func(i, j int) bool {
		return ladder.Variants[i].Bandwidth < ladder.Variants[j].Bandwidth
	})

	return ladder, nil
}

// parseVariant parses a single variant from EXT-X-STREAM-INF line
func parseVariant(streamInf, uri string, index int) Variant {
	variant := Variant{
		URI:   uri,
		Index: index,
	}

	// Extract BANDWIDTH (required)
	if matches := bandwidthRegex.FindStringSubmatch(streamInf); len(matches) > 1 {
		if bw, err := strconv.ParseInt(matches[1], 10, 64); err == nil {
			variant.Bandwidth = bw
		}
	}

	// Extract AVERAGE-BANDWIDTH (optional)
	if matches := avgBandwidthRegex.FindStringSubmatch(streamInf); len(matches) > 1 {
		if avgBw, err := strconv.ParseInt(matches[1], 10, 64); err == nil {
			variant.AverageBandwidth = avgBw
		}
	}

	// Extract RESOLUTION (optional)
	if matches := resolutionRegex.FindStringSubmatch(streamInf); len(matches) > 1 {
		variant.Resolution = matches[1]
	}

	// Extract CODECS (optional)
	if matches := codecsRegex.FindStringSubmatch(streamInf); len(matches) > 1 {
		variant.Codecs = matches[1]
	}

	// Extract FRAME-RATE (optional)
	if matches := frameRateRegex.FindStringSubmatch(streamInf); len(matches) > 1 {
		if fr, err := strconv.ParseFloat(matches[1], 64); err == nil {
			variant.FrameRate = fr
		}
	}

	return variant
}

// GetBandwidthMbps returns the bandwidth in Mbps
func (v *Variant) GetBandwidthMbps() float64 {
	return float64(v.Bandwidth) / 1_000_000.0
}

// GetAverageBandwidthMbps returns the average bandwidth in Mbps (or bandwidth if not set)
func (v *Variant) GetAverageBandwidthMbps() float64 {
	if v.AverageBandwidth > 0 {
		return float64(v.AverageBandwidth) / 1_000_000.0
	}
	return v.GetBandwidthMbps()
}

// GetEffectiveBandwidth returns AVERAGE-BANDWIDTH if available, otherwise BANDWIDTH
func (v *Variant) GetEffectiveBandwidth() int64 {
	if v.AverageBandwidth > 0 {
		return v.AverageBandwidth
	}
	return v.Bandwidth
}

// String returns a human-readable representation of the variant
func (v *Variant) String() string {
	parts := []string{
		fmt.Sprintf("%.2f Mbps", v.GetBandwidthMbps()),
	}
	if v.Resolution != "" {
		parts = append(parts, v.Resolution)
	}
	if v.AverageBandwidth > 0 {
		parts = append(parts, fmt.Sprintf("avg=%.2f Mbps", v.GetAverageBandwidthMbps()))
	}
	return strings.Join(parts, ", ")
}

// FindVariantByBandwidth finds the variant closest to the given bandwidth (in Mbps)
func (l *Ladder) FindVariantByBandwidth(bandwidthMbps float64) *Variant {
	if len(l.Variants) == 0 {
		return nil
	}

	targetBps := int64(bandwidthMbps * 1_000_000)
	closestIdx := 0
	minDiff := abs(l.Variants[0].GetEffectiveBandwidth() - targetBps)

	for i := 1; i < len(l.Variants); i++ {
		diff := abs(l.Variants[i].GetEffectiveBandwidth() - targetBps)
		if diff < minDiff {
			minDiff = diff
			closestIdx = i
		}
	}

	return &l.Variants[closestIdx]
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
