package util

import (
	"io"
	"os"
	"path/filepath"
)

func AppendToFile(path string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}

func SaveMultipartFile(path string, r io.Reader, progress func(int)) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	buf := make([]byte, 1024*1024)
	var total int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
			if progress != nil {
				progress(int((total / (1024 * 1024))))
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func ValidateDir(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}

func FindSegments(root string) ([]string, error) {
	var segments []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) == ".m4s" {
			segments = append(segments, path)
		}
		return nil
	})
	return segments, err
}
