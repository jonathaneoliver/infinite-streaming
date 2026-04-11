package api

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/go-upload/internal/store"
	"github.com/jonathaneoliver/infinite-streaming/go-upload/internal/util"
)

const setupMarkerFile = ".infinite-streaming-initialized"

type DirStatus struct {
	Path     string `json:"path"`
	Exists   bool   `json:"exists"`
	Writable bool   `json:"writable"`
	Error    string `json:"error,omitempty"`
}

type SetupStatus struct {
	Status         string               `json:"status"`
	Root           string               `json:"root"`
	RootExists     bool                 `json:"root_exists"`
	RootWritable   bool                 `json:"root_writable"`
	RootMounted    bool                 `json:"root_mounted"`
	Initialized    bool                 `json:"initialized"`
	Dirs           map[string]DirStatus `json:"dirs"`
	ContentCount   int                  `json:"content_count"`
	SourcesCount   int                  `json:"sources_count"`
	OutputsCount   int                  `json:"outputs_count"`
	ContentEmpty   bool                 `json:"content_empty"`
	Issues         []string             `json:"issues"`
	Recommendations []string            `json:"recommendations"`
}

func (h *Handler) SetupStatus(w http.ResponseWriter, _ *http.Request) {
	status := h.buildSetupStatus()
	writeJSON(w, http.StatusOK, status)
}

func (h *Handler) SetupInitialize(w http.ResponseWriter, _ *http.Request) {
	status := h.buildSetupStatus()
	if err := ensureDirs([]string{
		h.App.Cfg.Root,
		h.App.Cfg.SourcesDir,
		h.App.Cfg.OutputDir,
		h.App.Cfg.UploadsDir,
		filepath.Dir(h.App.Cfg.DatabasePath),
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	if err := touch(filepath.Join(h.App.Cfg.Root, setupMarkerFile)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	status = h.buildSetupStatus()
	writeJSON(w, http.StatusOK, status)
}

func (h *Handler) SetupSeed(w http.ResponseWriter, _ *http.Request) {
	if err := ensureDirs([]string{
		h.App.Cfg.Root,
		h.App.Cfg.SourcesDir,
		h.App.Cfg.OutputDir,
		filepath.Dir(h.App.Cfg.DatabasePath),
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}

	outputName := "sample_clip"
	targetFile := filepath.Join(h.App.Cfg.SourcesDir, outputName+".mp4")
	if _, err := os.Stat(targetFile); err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "exists", "file": targetFile})
		return
	}

	if err := generateSampleVideo(targetFile); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	if !util.ValidateVideo(targetFile) {
		_ = os.Remove(targetFile)
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "Generated sample video is invalid"})
		return
	}

	sourceID := util.NewUUID()
	jobID := util.NewUUID()
	metadata := util.GetVideoMetadata(targetFile)
	fileInfo, _ := os.Stat(targetFile)
	size := int64(0)
	if fileInfo != nil {
		size = fileInfo.Size()
	}

	source := store.Source{
		SourceID:         sourceID,
		Name:             outputName,
		OriginalFilename: filepath.Base(targetFile),
		FilePath:         targetFile,
		FileSize:         size,
		Duration:         util.FloatPtr(metadata.Duration),
		Resolution:       util.StringPtr(metadata.Resolution),
		Codec:            util.StringPtr(metadata.Codec),
		UploadedAt:       time.Now().UTC().Format(time.RFC3339Nano),
		Metadata:         metadata.Raw,
	}
	if err := h.App.Store.CreateSource(source); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}

	config := map[string]interface{}{
		"output_name":     outputName,
		"codec_selection": "both",
		"hls_format":      "fmp4",
		"segment_duration": 6,
		"gop_duration":     1,
		"partial_duration": 200,
	}
	job := store.Job{
		JobID:     jobID,
		Name:      "Seed sample content",
		Status:    "queued",
		Progress:  0,
		Config:    config,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		SourceID:  &sourceID,
	}
	if err := h.App.Store.CreateJob(job); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	h.App.EnqueueJob(jobID)
	_ = touch(filepath.Join(h.App.Cfg.Root, setupMarkerFile))

	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "queued",
		"job_id":   jobID,
		"source_id": sourceID,
		"file":     targetFile,
	})
}

func (h *Handler) buildSetupStatus() SetupStatus {
	root := h.App.Cfg.Root
	rootExists, rootWritable := pathExists(root), canWrite(root)
	rootMounted := isMountPoint(root)
	initialized := pathExists(filepath.Join(root, setupMarkerFile))

	dirs := map[string]DirStatus{
		"root":    dirStatus(root),
		"sources": dirStatus(h.App.Cfg.SourcesDir),
		"outputs": dirStatus(h.App.Cfg.OutputDir),
		"uploads": dirStatus(h.App.Cfg.UploadsDir),
		"db":      dirStatus(filepath.Dir(h.App.Cfg.DatabasePath)),
	}

	contentCount := 0
	content, err := util.ListContent(h.App.Cfg.OutputDir)
	if err == nil {
		contentCount = len(content)
	}

	sourcesCount := countFiles(h.App.Cfg.SourcesDir, ".mp4")
	outputsCount := countDirs(h.App.Cfg.OutputDir)
	contentEmpty := contentCount == 0 && sourcesCount == 0

	issues := []string{}
	recs := []string{}
	if !rootExists {
		issues = append(issues, "Storage root not found")
		recs = append(recs, fmt.Sprintf("Mount a host volume to %s", root))
	}
	if rootExists && !rootWritable {
		issues = append(issues, "Storage root is not writable")
		recs = append(recs, "Check filesystem permissions for the mounted volume")
	}
	if rootExists && rootWritable && !rootMounted {
		issues = append(issues, "Storage volume may not be mounted (running on container filesystem)")
		recs = append(recs, fmt.Sprintf("Bind-mount your host folder to %s", root))
	}
	if contentEmpty {
		issues = append(issues, "No content available")
		recs = append(recs, "Upload a video or seed sample content")
	}
	if !initialized {
		recs = append(recs, "Complete first-run setup")
	}

	status := "ok"
	if len(issues) > 0 {
		status = "warning"
	}

	return SetupStatus{
		Status:         status,
		Root:           root,
		RootExists:     rootExists,
		RootWritable:   rootWritable,
		RootMounted:    rootMounted,
		Initialized:    initialized,
		Dirs:           dirs,
		ContentCount:   contentCount,
		SourcesCount:   sourcesCount,
		OutputsCount:   outputsCount,
		ContentEmpty:   contentEmpty,
		Issues:         issues,
		Recommendations: recs,
	}
}

func dirStatus(path string) DirStatus {
	status := DirStatus{Path: path}
	info, err := os.Stat(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			status.Error = err.Error()
		}
		return status
	}
	if !info.IsDir() {
		status.Error = "not a directory"
		return status
	}
	status.Exists = true
	status.Writable = canWrite(path)
	if !status.Writable {
		status.Error = "not writable"
	}
	return status
}

func canWrite(path string) bool {
	if !pathExists(path) {
		return false
	}
	testFile := filepath.Join(path, ".write-test")
	if err := os.WriteFile(testFile, []byte("ok"), 0644); err != nil {
		return false
	}
	_ = os.Remove(testFile)
	return true
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func ensureDirs(paths []string) error {
	for _, path := range paths {
		if path == "" {
			continue
		}
		if err := os.MkdirAll(path, 0755); err != nil {
			return err
		}
	}
	return nil
}

func touch(path string) error {
	now := time.Now()
	if pathExists(path) {
		return os.Chtimes(path, now, now)
	}
	return os.WriteFile(path, []byte("initialized"), 0644)
}

func isMountPoint(path string) bool {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	lines := strings.Split(string(data), "\n")
	clean := filepath.Clean(path)
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 5 && fields[4] == clean {
			return true
		}
	}
	return false
}

func countFiles(dir, suffix string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if suffix == "" || strings.HasSuffix(entry.Name(), suffix) {
			count++
		}
	}
	return count
}

func countDirs(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			count++
		}
	}
	return count
}

func generateSampleVideo(target string) error {
	args := []string{
		"-y",
		"-f", "lavfi",
		"-i", "testsrc=size=1280x720:rate=30",
		"-f", "lavfi",
		"-i", "sine=frequency=1000:sample_rate=44100",
		"-t", "10",
		"-c:v", "libx264",
		"-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-shortest",
		target,
	}
	cmd := exec.Command("ffmpeg", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg failed: %s", string(output))
	}
	return nil
}
