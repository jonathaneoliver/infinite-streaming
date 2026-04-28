package util

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var sanitizePattern = regexp.MustCompile(`[^\w\-]+`)

// Maximum sanitised name length. Sized so the resulting catalogue
// directory (`<safe>_p200_<codec>_<YYYYMMDD>_<HHMMSS>`, ~25 chars of
// suffix) stays well under modern filesystems' 255-byte filename
// limit (ext4 / btrfs / APFS / NTFS). Older versions used 50, which
// truncated descriptive titles like
// "samsung_4k_demo_samsung_and_redbull_see_the_unexpected_hdr_uhd_4k_full_hd_2160p"
// to "samsung_4k_demo_samsung_and_redbull_see_the_unexpe" — losing the
// distinguishing tail.
const maxSanitizedNameLen = 200

func SanitizeName(name string) string {
	trimmed := strings.TrimSpace(name)
	safe := sanitizePattern.ReplaceAllString(trimmed, "_")
	if len(safe) > maxSanitizedNameLen {
		safe = safe[:maxSanitizedNameLen]
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
