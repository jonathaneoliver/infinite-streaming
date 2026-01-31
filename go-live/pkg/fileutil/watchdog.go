package fileutil

import (
	"os"
	"path/filepath"
	"time"
)

func CleanupStuckFiles(dir string, maxAge time.Duration) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.tmp.*"))
	for _, f := range files {
		stat, _ := os.Stat(f)
		if time.Since(stat.ModTime()) > maxAge {
			os.Remove(f)
		}
	}
}
