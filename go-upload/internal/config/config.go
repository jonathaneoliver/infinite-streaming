package config

import (
	"os"
	"path/filepath"
)

type Config struct {
	Root        string
	UploadsDir   string
	SourcesDir   string
	OutputDir    string
	DatabasePath string
	AbrScript    string
}

func Load() Config {
	root := getenvAny([]string{"INFINITE_STREAM_ROOT", "INFINITE_ROOT", "BOSS_ROOT"}, "")
	if root == "" {
		root = "/boss"
	}
	uploads := getenvAny([]string{"INFINITE_STREAM_UPLOADS_DIR", "INFINITE_UPLOADS_DIR", "BOSS_UPLOADS_DIR"}, filepath.Join(root, "boss-uploads"))
	sources := getenvAny([]string{"INFINITE_STREAM_SOURCES_DIR", "INFINITE_SOURCES_DIR", "BOSS_SOURCES_DIR"}, filepath.Join(root, "originals"))
	output := getenvAny([]string{"INFINITE_STREAM_OUTPUT_DIR", "INFINITE_OUTPUT_DIR", "BOSS_OUTPUT_DIR"}, filepath.Join(root, "dynamic_content"))
	dbDir := getenvAny([]string{"INFINITE_STREAM_DATABASE_DIR", "INFINITE_DATABASE_DIR", "BOSS_DATABASE_DIR"}, filepath.Join(root, "boss-data"))

	return Config{
		Root:         root,
		UploadsDir:   uploads,
		SourcesDir:   sources,
		OutputDir:    output,
		DatabasePath: filepath.Join(dbDir, "encoding_jobs.db"),
		AbrScript:    getenvAny([]string{"ABR_SCRIPT_PATH"}, "/generate_abr/create_abr_ladder.sh"),
	}
}

func getenvAny(keys []string, fallback string) string {
	for _, key := range keys {
		if val := os.Getenv(key); val != "" {
			return val
		}
	}
	return fallback
}
