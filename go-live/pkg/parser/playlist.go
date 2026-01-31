package parser

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bluenviron/gohlslib/pkg/playlist"
)

var (
	logOnceMu     sync.Mutex
	lastLogSecond int64
)

func logOncePerSecond(message string) {
	now := time.Now().Unix()
	logOnceMu.Lock()
	defer logOnceMu.Unlock()
	if lastLogSecond == now {
		return
	}
	lastLogSecond = now
	fmt.Fprintln(os.Stderr, message)
}

type PlaylistLoader struct{}

// PlaylistInfo contains parsed playlist information
type PlaylistInfo struct {
	IsVariant      bool
	MasterPlaylist *playlist.Multivariant
	MediaPlaylist  *playlist.Media
	TotalDuration  float64
	RelPath        string
	SegmentMap     string // URI of the init segment (EXT-X-MAP)
}

// Load loads a media playlist
func (pl *PlaylistLoader) Load(path string) (*playlist.Media, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	mediaPlaylist := &playlist.Media{}
	err = mediaPlaylist.Unmarshal(content)
	if err != nil {
		return nil, err
	}

	return mediaPlaylist, nil
}

// LoadMaster loads a master playlist and returns raw bytes
func (pl *PlaylistLoader) LoadMaster(path string) ([]byte, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// Validate it's a master playlist
	masterPlaylist := &playlist.Multivariant{}
	err = masterPlaylist.Unmarshal(content)
	if err != nil {
		return nil, fmt.Errorf("not a master playlist or failed to parse: %w", err)
	}

	return content, nil
}

// LoadPlaylistInfo loads a playlist (master or media) and returns comprehensive info
// This matches Python load_ll_playlist() function
func (pl *PlaylistLoader) LoadPlaylistInfo(folder, uri string) (*PlaylistInfo, error) {
	path := filepath.Join(folder, filepath.FromSlash(uri))

	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	info := &PlaylistInfo{
		RelPath: filepath.Dir(uri),
	}

	// Try to parse as master playlist first
	masterPlaylist := &playlist.Multivariant{}
	err = masterPlaylist.Unmarshal(content)
	if err == nil && len(masterPlaylist.Variants) > 0 {
		// It's a master playlist
		info.IsVariant = true
		info.MasterPlaylist = masterPlaylist
		return info, nil
	}

	// Try to parse as media playlist
	mediaPlaylist := &playlist.Media{}
	err = mediaPlaylist.Unmarshal(content)
	if err != nil {
		return nil, fmt.Errorf("failed to parse as master or media playlist: %w", err)
	}

	info.IsVariant = false
	info.MediaPlaylist = mediaPlaylist

	// Calculate total duration
	totalDuration := 0.0
	for _, segment := range mediaPlaylist.Segments {
		if segment != nil {
			totalDuration += segment.Duration.Seconds()
		}
	}
	info.TotalDuration = totalDuration

	// Extract EXT-X-MAP (init segment) URI
	if mediaPlaylist.Map != nil {
		info.SegmentMap = mediaPlaylist.Map.URI
	}

	return info, nil
}

// LoadPlaylistInfoWithByteranges loads playlist info and byterange metadata together
// This is a convenience function that matches Python workflow
func (pl *PlaylistLoader) LoadPlaylistInfoWithByteranges(folder, uri string) (*PlaylistInfo, map[string][]ByteRange, error) {
	info, err := pl.LoadPlaylistInfo(folder, uri)
	if err != nil {
		return nil, nil, err
	}

	// Load byterange metadata from the media playlist's parts
	byteranges := make(map[string][]ByteRange)

	if info.MediaPlaylist != nil {
		for _, segment := range info.MediaPlaylist.Segments {
			if segment == nil {
				continue
			}

			// Extract byte ranges from parts
			if len(segment.Parts) > 0 {
				fragments := make([]ByteRange, 0, len(segment.Parts))
				for _, part := range segment.Parts {
					if part == nil {
						continue
					}

					// bluenviron/gohlslib properly parses byte ranges
					var offset, length int
					if part.ByteRangeStart != nil {
						offset = int(*part.ByteRangeStart)
					}
					if part.ByteRangeLength != nil {
						length = int(*part.ByteRangeLength)
					}

					fragments = append(fragments, ByteRange{
						Offset:      offset,
						Length:      length,
						Independent: part.Independent,
					})
				}
				byteranges[segment.URI] = fragments
			}
		}
	}

	// If no parts found in playlist, fall back to .byteranges files
	if len(byteranges) == 0 {
		fmt.Fprintf(os.Stderr, "INFO: No #EXT-X-PART tags found, falling back to .byteranges files\n")
		path := filepath.Join(folder, filepath.FromSlash(uri))
		byteranges, err = LoadByterangesForPlaylist(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARN: Failed to load byteranges for %s: %v\n", uri, err)
		}
	} else {
		logOncePerSecond(fmt.Sprintf("INFO: Loaded #EXT-X-PART tags for %d segments", len(byteranges)))
	}

	return info, byteranges, nil
}

// GetVariantsDuration gets min and max duration across all variants in master playlist
// This matches Python get_variants_duration() function
func (pl *PlaylistLoader) GetVariantsDuration(folder, uri string) (float64, float64, error) {
	masterInfo, err := pl.LoadPlaylistInfo(folder, uri)
	if err != nil {
		return 0, 0, err
	}

	if !masterInfo.IsVariant {
		return 0, 0, fmt.Errorf("not a master playlist")
	}

	minDuration := 0.0
	maxDuration := 0.0
	first := true

	// Check all variants in master
	for _, variant := range masterInfo.MasterPlaylist.Variants {
		if variant == nil {
			continue
		}

		variantInfo, err := pl.LoadPlaylistInfo(folder, variant.URI)
		if err != nil {
			// Log error but don't fail - continue with other variants
			fmt.Fprintf(os.Stderr, "WARN: Failed to load variant %s: %v (continuing...)\n", variant.URI, err)
			continue
		}

		if variantInfo.TotalDuration > 0 {
			if first {
				minDuration = variantInfo.TotalDuration
				maxDuration = variantInfo.TotalDuration
				first = false
			} else {
				if variantInfo.TotalDuration < minDuration {
					minDuration = variantInfo.TotalDuration
				}
				if variantInfo.TotalDuration > maxDuration {
					maxDuration = variantInfo.TotalDuration
				}
			}
		}
	}

	// If no variants loaded successfully, return an error
	if first {
		return 0, 0, fmt.Errorf("failed to load any variant playlists")
	}

	return minDuration, maxDuration, nil
}

// ReadFileBytes is a helper to read file contents
func ReadFileBytes(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return io.ReadAll(f)
}

// SegmentCount returns the number of non-nil segments in a media playlist
func SegmentCount(mediaPlaylist *playlist.Media) int {
	count := 0
	for _, seg := range mediaPlaylist.Segments {
		if seg != nil {
			count++
		}
	}
	return count
}

// GetSegment safely gets a segment by index from a media playlist
func GetSegment(mediaPlaylist *playlist.Media, idx int) *playlist.MediaSegment {
	if idx < 0 || idx >= len(mediaPlaylist.Segments) {
		return nil
	}
	return mediaPlaylist.Segments[idx]
}

// GetSegmentDuration gets the duration of a segment in seconds
func GetSegmentDuration(segment *playlist.MediaSegment) float64 {
	if segment == nil {
		return 0.0
	}
	return segment.Duration.Seconds()
}

// FormatTime formats a time.Duration to ISO8601 format for HLS playlists
func FormatTime(t time.Time) string {
	return t.Format("2006-01-02T15:04:05.000Z")
}
