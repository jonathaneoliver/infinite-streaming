package util

import (
	"bufio"
	"encoding/xml"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type ContentInfo struct {
	Name string `json:"name"`
	// Logical clip identifier — the lowercased name with the
	// `_p200_<codec>` suffix stripped. The same clip encoded as h264,
	// hevc, and av1 share a clip_id, so clients can dedupe browse rows
	// by an exact key instead of fuzzy substring matching. Timestamp
	// suffixes are preserved (different upload sessions of the same
	// title remain distinct).
	ClipID string `json:"clip_id"`
	// Codec stripped from the name: "h264", "hevc", "av1", or "" if
	// the name doesn't carry one.
	Codec             string  `json:"codec"`
	HasDash           bool    `json:"has_dash"`
	HasHls            bool    `json:"has_hls"`
	HasThumbnail      bool    `json:"has_thumbnail"`
	// 640 px wide — the default tile size on most clients.
	ThumbnailURL      string  `json:"thumbnail_url,omitempty"`
	// 320 px wide — list rows / mobile.
	ThumbnailURLSmall string  `json:"thumbnail_url_small,omitempty"`
	// 1280 px wide — Continue Watching hero / large surfaces.
	ThumbnailURLLarge string  `json:"thumbnail_url_large,omitempty"`
	SegmentDuration   *int    `json:"segment_duration"`
	MaxResolution     *string `json:"max_resolution"`
	MaxHeight         *int    `json:"max_height"`
}

// Strips `_p200_<codec>` from a content name, preserving anything before
// (the clip stem) and anything after (e.g. a re-encode timestamp). Returns
// the lowercased stem + the matched codec (empty if the pattern doesn't
// match).
var clipIDPattern = regexp.MustCompile(`(?i)_p200_(h264|hevc|h265|av1)(_|$)`)

func splitClipIDAndCodec(name string) (clipID, codec string) {
	m := clipIDPattern.FindStringSubmatchIndex(name)
	if m == nil {
		return strings.ToLower(name), ""
	}
	codec = strings.ToLower(name[m[2]:m[3]])
	stripped := name[:m[0]] + name[m[4]:]
	stripped = strings.TrimSuffix(stripped, "_")
	return strings.ToLower(stripped), codec
}

func ListContent(contentDir string) ([]ContentInfo, error) {
	entries, err := os.ReadDir(contentDir)
	if err != nil {
		return nil, err
	}

	var contentList []ContentInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		itemPath := filepath.Join(contentDir, name)
		hasDash := fileExists(filepath.Join(itemPath, "manifest.mpd"))
		hasHls := fileExists(filepath.Join(itemPath, "master.m3u8"))
		if !hasDash && !hasHls {
			continue
		}
		// Thumbnail discovery — generate_abr/create_abr_ladder.sh emits
		// three jpegs per output dir (320/640/1280 px wide). Clients pick
		// the size that fits their surface so we don't pay client-side
		// rescaling cost. We only stat the 640 default; the script writes
		// all three in a single ffmpeg pass so any one of them implies
		// the others.
		hasThumbnail := fileExists(filepath.Join(itemPath, "thumbnail.jpg"))
		thumbnailURL, thumbnailURLSmall, thumbnailURLLarge := "", "", ""
		if hasThumbnail {
			base := "/go-live/" + name
			thumbnailURL = base + "/thumbnail.jpg"
			thumbnailURLSmall = base + "/thumbnail-small.jpg"
			thumbnailURLLarge = base + "/thumbnail-large.jpg"
		}
		segmentDuration := detectSegmentDuration(itemPath)
		maxResolution, maxHeight := detectMaxResolution(itemPath)
		clipID, codec := splitClipIDAndCodec(name)
		contentList = append(contentList, ContentInfo{
			Name:              name,
			ClipID:            clipID,
			Codec:             codec,
			HasDash:           hasDash,
			HasHls:            hasHls,
			HasThumbnail:      hasThumbnail,
			ThumbnailURL:      thumbnailURL,
			ThumbnailURLSmall: thumbnailURLSmall,
			ThumbnailURLLarge: thumbnailURLLarge,
			SegmentDuration:   segmentDuration,
			MaxResolution:     maxResolution,
			MaxHeight:         maxHeight,
		})
	}

	sort.Slice(contentList, func(i, j int) bool {
		return contentList[i].Name < contentList[j].Name
	})

	return contentList, nil
}

func detectSegmentDuration(contentPath string) *int {
	hlsDirs := []string{"720p", "540p", "360p", "1080p"}
	for _, dir := range hlsDirs {
		playlistPath := filepath.Join(contentPath, dir, "playlist.m3u8")
		if !fileExists(playlistPath) {
			continue
		}
		if dur := parseHlsPlaylistDuration(playlistPath); dur != nil {
			return dur
		}
	}

	// Fallback to DASH manifest
	manifestPath := filepath.Join(contentPath, "manifest.mpd")
	if fileExists(manifestPath) {
		if dur := parseDashSegmentDuration(manifestPath); dur != nil {
			return dur
		}
	}

	return nil
}

func parseHlsPlaylistDuration(path string) *int {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()

	var durations []float64
	var targetDuration *int
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#EXTINF:") {
			val := strings.TrimPrefix(line, "#EXTINF:")
			val = strings.TrimSuffix(val, ",")
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				durations = append(durations, f)
				if len(durations) >= 10 {
					break
				}
			}
		} else if strings.HasPrefix(line, "#EXT-X-TARGETDURATION:") && targetDuration == nil {
			val := strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:")
			if v, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
				targetDuration = &v
			}
		}
	}

	if len(durations) > 0 {
		var sum float64
		for _, v := range durations {
			sum += v
		}
		avg := sum / float64(len(durations))
		rounded := int(avg + 0.5)
		return &rounded
	}

	return targetDuration
}

func parseDashSegmentDuration(path string) *int {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	type segmentList struct {
		Timescale string `xml:"timescale,attr"`
		Timeline  struct {
			S struct {
				D string `xml:"d,attr"`
			} `xml:"S"`
		} `xml:"SegmentTimeline"`
	}

	decoder := xml.NewDecoder(strings.NewReader(string(data)))
	for {
		tok, err := decoder.Token()
		if err != nil {
			return nil
		}
		switch elem := tok.(type) {
		case xml.StartElement:
			if elem.Name.Local != "SegmentList" {
				continue
			}
			var list segmentList
			if err := decoder.DecodeElement(&list, &elem); err != nil {
				continue
			}
			timescale, _ := strconv.ParseFloat(list.Timescale, 64)
			if timescale == 0 {
				timescale = 1
			}
			if list.Timeline.S.D != "" {
				if d, err := strconv.ParseFloat(list.Timeline.S.D, 64); err == nil {
					segSeconds := d / timescale
					rounded := int(segSeconds + 0.5)
					return &rounded
				}
			}
		}
	}
}

func detectMaxResolution(contentPath string) (*string, *int) {
	var heights []int
	entries, err := os.ReadDir(contentPath)
	if err == nil {
		re := regexp.MustCompile(`^(\d{3,4})p$`)
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			match := re.FindStringSubmatch(entry.Name())
			if match != nil {
				if val, err := strconv.Atoi(match[1]); err == nil {
					heights = append(heights, val)
				}
			}
		}
	}

	if len(heights) == 0 {
		// Fallback: parse master.m3u8 for RESOLUTION tags
		masterPath := filepath.Join(contentPath, "master.m3u8")
		if fileExists(masterPath) {
			file, err := os.Open(masterPath)
			if err == nil {
				defer file.Close()
				re := regexp.MustCompile(`RESOLUTION=(\d+)x(\d+)`)
				scanner := bufio.NewScanner(file)
				for scanner.Scan() {
					line := scanner.Text()
					match := re.FindStringSubmatch(line)
					if match != nil {
						if val, err := strconv.Atoi(match[2]); err == nil {
							heights = append(heights, val)
						}
					}
				}
			}
		}
	}

	if len(heights) == 0 {
		return nil, nil
	}

	maxHeight := heights[0]
	for _, h := range heights[1:] {
		if h > maxHeight {
			maxHeight = h
		}
	}
	res := strconv.Itoa(maxHeight) + "p"
	return &res, &maxHeight
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
