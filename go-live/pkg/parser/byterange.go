package parser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type ByteRange struct {
	Offset      int  `json:"offset"`
	Length      int  `json:"length"`
	Independent bool `json:"independent"`
}

// ByteRangeFile represents the JSON structure of .byteranges files
type ByteRangeFile struct {
	Fragments []ByteRange `json:"fragments"`
}

// LoadByterangesForPlaylist loads byte-range metadata for all segments in a playlist.
// This matches Python load_byterange_metadata() function.
//
// Sources (in priority order):
// 1. #EXT-X-PART tags in source playlist (preferred - authoritative source)
// 2. .byteranges files (fallback for content without embedded parts)
//
// Returns map: {segment_filename: []ByteRange}
func LoadByterangesForPlaylist(playlistPath string) (map[string][]ByteRange, error) {
	byterangeMap := make(map[string][]ByteRange)

	playlistDir := filepath.Dir(playlistPath)

	content, err := os.ReadFile(playlistPath)
	if err != nil {
		return byterangeMap, err
	}

	// Parse playlist line by line to extract #EXT-X-PART tags
	lines := strings.Split(string(content), "\n")

	// Track current segment being processed
	var currentSegmentParts []ByteRange
	var currentSegmentName string

	// Regex to parse #EXT-X-PART tag
	// Example: #EXT-X-PART:DURATION=1.000000,URI="segment_00001.m4s",BYTERANGE="891279@96",INDEPENDENT=YES
	partRegex := regexp.MustCompile(`#EXT-X-PART:.*URI="([^"]+)".*BYTERANGE="(\d+)@(\d+)"`)
	independentRegex := regexp.MustCompile(`INDEPENDENT=YES`)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Check for #EXT-X-PART tag
		if strings.HasPrefix(line, "#EXT-X-PART:") {
			matches := partRegex.FindStringSubmatch(line)
			if len(matches) == 4 {
				uri := matches[1]
				length, _ := strconv.Atoi(matches[2])
				offset, _ := strconv.Atoi(matches[3])
				independent := independentRegex.MatchString(line)

				// Extract just the filename from URI (might have path like "1080p/segment_00001.m4s")
				segmentFile := filepath.Base(uri)

				// If this is a different segment than we're currently tracking, save the previous one
				if currentSegmentName != "" && currentSegmentName != segmentFile {
					if len(currentSegmentParts) > 0 {
						byterangeMap[currentSegmentName] = currentSegmentParts
						fmt.Fprintf(os.Stderr, "INFO: Loaded %d fragments from #EXT-X-PART tags for %s\n",
							len(currentSegmentParts), currentSegmentName)
					}
					currentSegmentParts = []ByteRange{}
				}

				currentSegmentName = segmentFile
				currentSegmentParts = append(currentSegmentParts, ByteRange{
					Offset:      offset,
					Length:      length,
					Independent: independent,
				})
			}
		} else if !strings.HasPrefix(line, "#") && (strings.HasSuffix(line, ".m4s") || strings.HasSuffix(line, ".ts")) {
			// This is a segment line - save accumulated parts for the current segment
			if currentSegmentName != "" && len(currentSegmentParts) > 0 {
				byterangeMap[currentSegmentName] = currentSegmentParts
				fmt.Fprintf(os.Stderr, "INFO: Loaded %d fragments from #EXT-X-PART tags for %s\n",
					len(currentSegmentParts), currentSegmentName)
			}
			currentSegmentParts = []ByteRange{}
			currentSegmentName = ""
		}
	}

	// Save the last segment's parts if any
	if currentSegmentName != "" && len(currentSegmentParts) > 0 {
		byterangeMap[currentSegmentName] = currentSegmentParts
		fmt.Fprintf(os.Stderr, "INFO: Loaded %d fragments from #EXT-X-PART tags for %s\n",
			len(currentSegmentParts), currentSegmentName)
	}

	// If no #EXT-X-PART tags were found, fall back to .byteranges files
	if len(byterangeMap) == 0 {
		fmt.Fprintf(os.Stderr, "INFO: No #EXT-X-PART tags found, falling back to .byteranges files\n")

		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}

			// Check if it's a segment file
			if strings.HasSuffix(line, ".m4s") || strings.HasSuffix(line, ".ts") {
				segmentPath := filepath.Join(playlistDir, line)
				byterangesPath := segmentPath + ".byteranges"

				if _, err := os.Stat(byterangesPath); err == nil {
					fragments, err := loadByterangesFile(byterangesPath)
					if err != nil {
						fmt.Fprintf(os.Stderr, "WARN: Failed to load byteranges for %s: %v\n", line, err)
					} else {
						byterangeMap[line] = fragments
					}
				}
			}
		}
	}

	return byterangeMap, nil
}

// loadByterangesFile loads a single .byteranges JSON file
func loadByterangesFile(path string) ([]ByteRange, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var data ByteRangeFile
	if err := json.NewDecoder(f).Decode(&data); err != nil {
		return nil, err
	}

	return data.Fragments, nil
}

// LoadByteranges is the legacy function for backwards compatibility
// Deprecated: Use LoadByterangesForPlaylist instead
func LoadByteranges(path string) (map[string][]ByteRange, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var m map[string][]ByteRange
	err = json.NewDecoder(f).Decode(&m)
	if err != nil {
		return nil, err
	}

	return m, nil
}
