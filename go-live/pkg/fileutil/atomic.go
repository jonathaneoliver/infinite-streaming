package fileutil

import (
	"os"

	"github.com/google/uuid"
)

// WriteAtomic writes content to a file atomically using temp file + rename
// This matches Python's temp_output + os.rename() pattern
func WriteAtomic(path string, content []byte) error {
	tempPath := path + ".tmp." + uuid.New().String()
	if err := os.WriteFile(tempPath, content, 0644); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}
