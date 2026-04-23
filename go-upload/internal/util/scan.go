package util

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/go-upload/internal/store"
)

var videoExtensions = map[string]struct{}{
	".mp4": {},
	".mkv": {},
	".mov": {},
	".avi": {},
	".m4v": {},
}

type ScanStats struct {
	Scanned  int `json:"scanned"`
	New      int `json:"new"`
	Existing int `json:"existing"`
	Errors   int `json:"errors"`
	Skipped  int `json:"skipped"`
}

func ScanOriginals(st *store.SQLiteStore, root string) ScanStats {
	stats := ScanStats{}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return stats
	}

	existingPaths := map[string]struct{}{}
	sources, err := st.ListSources()
	if err == nil {
		for _, src := range sources {
			existingPaths[normalizePath(src.FilePath)] = struct{}{}
		}
	}

	seen := map[string]struct{}{}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			stats.Errors++
			return nil
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if _, ok := videoExtensions[ext]; !ok {
			stats.Skipped++
			return nil
		}
		stats.Scanned++
		realPath := normalizePath(path)
		if _, ok := seen[realPath]; ok {
			stats.Skipped++
			return nil
		}
		seen[realPath] = struct{}{}
		if _, ok := existingPaths[realPath]; ok {
			stats.Existing++
			return nil
		}
		metadata := GetVideoMetadata(path)
		if len(metadata.Raw) == 0 {
			stats.Skipped++
			return nil
		}
		fileInfo, err := os.Stat(path)
		if err != nil {
			stats.Errors++
			return nil
		}
		sourceID := NewUUID()
		src := store.Source{
			SourceID:         sourceID,
			Name:             SanitizeName(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))),
			OriginalFilename: filepath.Base(path),
			FilePath:         path,
			FileSize:         fileInfo.Size(),
			Duration:         FloatPtr(metadata.Duration),
			Resolution:       StringPtr(metadata.Resolution),
			Codec:            StringPtr(metadata.Codec),
			UploadedAt:       time.Now().UTC().Format(time.RFC3339Nano),
			Metadata:         metadata.Raw,
		}
		if err := st.CreateSource(src); err != nil {
			stats.Errors++
			return nil
		}
		stats.New++
		return nil
	})
	return stats
}

func normalizePath(path string) string {
	if path == "" {
		return path
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return path
}
