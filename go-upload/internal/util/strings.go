package util

import (
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

func HybridFilename(original, sourceID string) string {
	stem := strings.TrimSuffix(filepath.Base(original), filepath.Ext(original))
	ext := strings.ToLower(filepath.Ext(original))
	if ext == "" {
		ext = ".mp4"
	}
	safe := SanitizeName(stem)
	return safe + "_" + sourceID[:8] + ext
}
