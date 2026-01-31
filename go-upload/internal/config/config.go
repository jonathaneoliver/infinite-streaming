package config

import (
	"os"
	"path/filepath"
)

type Config struct {
	UploadsDir   string
	SourcesDir   string
	OutputDir    string
	DatabasePath string
	AbrScript    string
}

func Load() Config {
	root := os.Getenv("BOSS_ROOT")
	if root == "" {
		root = "/boss"
	}
	uploads := getenv("BOSS_UPLOADS_DIR", filepath.Join(root, "boss-uploads"))
	sources := getenv("BOSS_SOURCES_DIR", filepath.Join(root, "originals"))
	output := getenv("BOSS_OUTPUT_DIR", filepath.Join(root, "dynamic_content"))
	dbDir := getenv("BOSS_DATABASE_DIR", filepath.Join(root, "boss-data"))

	return Config{
		UploadsDir:   uploads,
		SourcesDir:   sources,
		OutputDir:    output,
		DatabasePath: filepath.Join(dbDir, "encoding_jobs.db"),
		AbrScript:    getenv("ABR_SCRIPT_PATH", "/generate_abr/create_abr_ladder.sh"),
	}
}

func getenv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}
