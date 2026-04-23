package util

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var sanitizePattern = regexp.MustCompile(`[^\w\-]+`)

func SanitizeName(name string) string {
	trimmed := strings.TrimSpace(name)
	safe := sanitizePattern.ReplaceAllString(trimmed, "_")
	if len(safe) > 50 {
		safe = safe[:50]
	}
	return safe
}

// ResolveUploadFilename returns a sanitized filename for an uploaded file.
// Prefers the clean sanitized name when nothing of the same name exists in dir.
// On collision, appends the first 8 chars of sourceID to disambiguate.
func ResolveUploadFilename(dir, original, sourceID string) string {
	stem := strings.TrimSuffix(filepath.Base(original), filepath.Ext(original))
	ext := strings.ToLower(filepath.Ext(original))
	if ext == "" {
		ext = ".mp4"
	}
	safeStem := SanitizeName(stem)
	clean := safeStem + ext
	if _, err := os.Stat(filepath.Join(dir, clean)); os.IsNotExist(err) {
		return clean
	}
	return safeStem + "_" + sourceID[:8] + ext
}
