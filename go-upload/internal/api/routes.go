package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/boss/go-upload/internal/store"
	"github.com/boss/go-upload/internal/util"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

func (h *Handler) ListJobs(w http.ResponseWriter, _ *http.Request) {
	jobs, err := h.App.Store.ListJobs()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

func (h *Handler) GetJob(w http.ResponseWriter, r *http.Request) {
	jobID := mux.Vars(r)["job_id"]
	job, err := h.App.Store.GetJob(jobID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	if job == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"detail": "Job not found"})
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (h *Handler) CancelJob(w http.ResponseWriter, r *http.Request) {
	jobID := mux.Vars(r)["job_id"]
	job, err := h.App.Store.GetJob(jobID)
	if err != nil || job == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"detail": "Job not found"})
		return
	}
	if job.Status != "queued" && job.Status != "uploading" && job.Status != "encoding" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "Job cannot be cancelled"})
		return
	}
	h.App.CancelJob(jobID)
	status := "cancelled"
	_ = h.App.Store.UpdateJobStatus(jobID, store.JobStatusUpdate{Status: status})
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

func (h *Handler) DeleteJob(w http.ResponseWriter, r *http.Request) {
	jobID := mux.Vars(r)["job_id"]
	job, err := h.App.Store.GetJob(jobID)
	if err != nil || job == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"detail": "Job not found"})
		return
	}
	if err := h.App.Store.DeleteJob(jobID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	_ = os.RemoveAll(filepath.Join(h.App.Cfg.UploadsDir, jobID))
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *Handler) ListSources(w http.ResponseWriter, _ *http.Request) {
	sources, err := h.App.Store.ListSources()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": sources})
}

func (h *Handler) GetSource(w http.ResponseWriter, r *http.Request) {
	sourceID := mux.Vars(r)["source_id"]
	src, err := h.App.Store.GetSource(sourceID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	if src == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"detail": "Source not found"})
		return
	}
	writeJSON(w, http.StatusOK, src)
}

func (h *Handler) DeleteSource(w http.ResponseWriter, r *http.Request) {
	sourceID := mux.Vars(r)["source_id"]
	src, err := h.App.Store.GetSource(sourceID)
	if err != nil || src == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"detail": "Source not found"})
		return
	}
	count, err := h.App.Store.CountJobsForSource(sourceID, []string{"queued", "encoding"})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	if count > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "Cannot delete source with active encoding jobs"})
		return
	}
	_ = os.Remove(src.FilePath)
	if err := h.App.Store.DeleteSource(sourceID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *Handler) UploadActive(w http.ResponseWriter, _ *http.Request) {
	count, err := h.App.Store.CountJobsByStatus("uploading")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"active_uploads": count, "queue_length": 0})
}

func (h *Handler) ListContent(w http.ResponseWriter, _ *http.Request) {
	content, err := util.ListContent(h.App.Cfg.OutputDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, content)
}

func (h *Handler) UploadInit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "invalid form data"})
		return
	}
	filename := r.FormValue("filename")
	if filename == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "filename required"})
		return
	}
	if !strings.HasSuffix(filename, ".mp4") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "Only MP4 files are supported"})
		return
	}
	fileSize := r.FormValue("file_size")
	expectedSize, _ := parseInt(fileSize)

	outputName := r.FormValue("output_name")
	if outputName == "" {
		outputName = strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
	}
	outputName = util.SanitizeName(outputName)

	codecSelection := r.FormValue("codec_selection")
	if codecSelection == "" {
		codecSelection = "both"
	}
	maxResolution := r.FormValue("max_resolution")
	hlsFormat := r.FormValue("hls_format")
	if hlsFormat == "" {
		hlsFormat = "fmp4"
	}
	forceSoftware := r.FormValue("force_software") == "true"
	padding := r.FormValue("padding")
	if padding == "" {
		padding = "none"
	}
	durationLimit, _ := parseInt(r.FormValue("duration_limit"))
	segmentDuration, _ := parseInt(r.FormValue("segment_duration"))
	if segmentDuration == 0 {
		segmentDuration = 6
	}
	gopDuration, _ := parseFloat(r.FormValue("gop_duration"))
	if gopDuration == 0 {
		gopDuration = 1
	}
	partialDurations := r.MultipartForm.Value["partial_durations"]
	if len(partialDurations) == 0 {
		partialDurations = []string{"200"}
	}

	sourceID := util.NewUUID()
	jobID := util.NewUUID()
	hybridFilename := util.HybridFilename(filename, sourceID)

	jobIDs := []string{jobID}
	for i, pdur := range partialDurations {
		config := map[string]interface{}{
			"output_name":      outputName + "_p" + pdur,
			"codec_selection":  codecSelection,
			"max_resolution":   maxResolution,
			"hls_format":       hlsFormat,
			"force_software":   forceSoftware,
			"padding":          padding,
			"duration_limit":   durationLimit,
			"partial_duration": pdur,
			"segment_duration": segmentDuration,
			"gop_duration":     gopDuration,
		}
		id := jobID
		if i > 0 {
			id = util.NewUUID()
			jobIDs = append(jobIDs, id)
		}
		job := store.Job{
			JobID:     id,
			Name:      filename + " (p" + pdur + ")",
			Status:    "uploading",
			Progress:  0,
			Config:    config,
			CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			SourceID:  &sourceID,
		}
		if err := h.App.Store.CreateJob(job); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
			return
		}
	}

	meta := UploadMetadata{
		SourceID:       sourceID,
		JobID:          jobID,
		JobIDs:         jobIDs,
		Filename:       filename,
		HybridFilename: hybridFilename,
		ExpectedSize:   int64(expectedSize),
		ReceivedSize:   0,
		OutputName:     outputName,
	}
	if err := meta.Save(filepath.Join(h.App.Cfg.UploadsDir, jobID+"_metadata.json")); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"job_id":   jobID,
		"job_ids":  jobIDs,
		"source_id": sourceID,
		"status":   "uploading",
	})
}

func (h *Handler) UploadChunk(w http.ResponseWriter, r *http.Request) {
	jobID := mux.Vars(r)["job_id"]
	meta, err := LoadUploadMetadata(filepath.Join(h.App.Cfg.UploadsDir, jobID+"_metadata.json"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"detail": "Upload session not found"})
		return
	}
	if len(meta.JobIDs) == 0 {
		meta.JobIDs = []string{meta.JobID}
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "invalid form data"})
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "file required"})
		return
	}
	defer file.Close()

	targetPath := filepath.Join(h.App.Cfg.SourcesDir, meta.HybridFilename)
	if err := util.AppendToFile(targetPath, file); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	info, _ := os.Stat(targetPath)
	if info != nil {
		meta.ReceivedSize = info.Size()
	}
	progress := 0
	if meta.ExpectedSize > 0 {
		progress = int(float64(meta.ReceivedSize) / float64(meta.ExpectedSize) * 100)
		if progress > 99 {
			progress = 99
		}
	}
	for _, id := range meta.JobIDs {
		val := progress
		_ = h.App.Store.UpdateJobStatus(id, store.JobStatusUpdate{
			Status:   "uploading",
			Progress: &val,
		})
		h.App.LogHub.Broadcast(id, util.TimestampLog("Upload progress: "+strconv.Itoa(progress)+"%"))
	}
	_ = meta.Save(filepath.Join(h.App.Cfg.UploadsDir, jobID+"_metadata.json"))

	writeJSON(w, http.StatusOK, map[string]any{
		"job_id":        jobID,
		"progress":      progress,
		"received_size": meta.ReceivedSize,
		"expected_size": meta.ExpectedSize,
		"status":        "uploading",
	})
}

func (h *Handler) UploadComplete(w http.ResponseWriter, r *http.Request) {
	jobID := mux.Vars(r)["job_id"]
	meta, err := LoadUploadMetadata(filepath.Join(h.App.Cfg.UploadsDir, jobID+"_metadata.json"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"detail": "Upload session not found"})
		return
	}
	if len(meta.JobIDs) == 0 {
		meta.JobIDs = []string{meta.JobID}
	}
	sourcePath := filepath.Join(h.App.Cfg.SourcesDir, meta.HybridFilename)
	if _, err := os.Stat(sourcePath); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"detail": "Uploaded file not found"})
		return
	}
	if !util.ValidateVideo(sourcePath) {
		_ = os.Remove(sourcePath)
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "Invalid video file"})
		return
	}
	metadata := util.GetVideoMetadata(sourcePath)
	fileInfo, _ := os.Stat(sourcePath)
	size := int64(0)
	if fileInfo != nil {
		size = fileInfo.Size()
	}
	source := store.Source{
		SourceID:         meta.SourceID,
		Name:             meta.OutputName,
		OriginalFilename: meta.Filename,
		FilePath:         sourcePath,
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
	for _, id := range meta.JobIDs {
		val := 0
		_ = h.App.Store.UpdateJobStatus(id, store.JobStatusUpdate{Status: "queued", Progress: &val})
		h.App.EnqueueJob(id)
		h.App.LogHub.Broadcast(id, util.TimestampLog("Upload complete! Queued for encoding..."))
	}
	_ = os.Remove(filepath.Join(h.App.Cfg.UploadsDir, jobID+"_metadata.json"))
	writeJSON(w, http.StatusOK, map[string]any{"job_id": jobID, "source_id": meta.SourceID, "status": "queued"})
}

func (h *Handler) UploadFile(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "invalid form data"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "file required"})
		return
	}
	defer file.Close()
	if !strings.HasSuffix(header.Filename, ".mp4") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "Only MP4 files are supported"})
		return
	}
	sourceID := util.NewUUID()
	jobID := util.NewUUID()
	outputName := util.SanitizeName(r.FormValue("output_name"))
	if outputName == "" {
		outputName = util.SanitizeName(strings.TrimSuffix(header.Filename, filepath.Ext(header.Filename)))
	}
	filename := util.HybridFilename(header.Filename, sourceID)
	targetPath := filepath.Join(h.App.Cfg.SourcesDir, filename)

	uploadJobID := jobID + "_upload"
	uploadJob := store.Job{
		JobID:     uploadJobID,
		Name:      "Uploading " + header.Filename,
		Status:    "uploading",
		Progress:  0,
		Config:    map[string]interface{}{},
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		SourceID:  &sourceID,
	}
	if err := h.App.Store.CreateJob(uploadJob); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}

	size, err := util.SaveMultipartFile(targetPath, file, func(progress int) {
		val := progress
		_ = h.App.Store.UpdateJobStatus(uploadJobID, store.JobStatusUpdate{Status: "uploading", Progress: &val})
	})
	if err != nil {
		_ = os.Remove(targetPath)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
		return
	}
	val := 100
	_ = h.App.Store.UpdateJobStatus(uploadJobID, store.JobStatusUpdate{Status: "complete", Progress: &val})

	if !util.ValidateVideo(targetPath) {
		_ = os.Remove(targetPath)
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "Invalid video file"})
		return
	}
	metadata := util.GetVideoMetadata(targetPath)
	source := store.Source{
		SourceID:         sourceID,
		Name:             outputName,
		OriginalFilename: header.Filename,
		FilePath:         targetPath,
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
		"codec_selection": r.FormValue("codec_selection"),
		"max_resolution":  r.FormValue("max_resolution"),
		"hls_format":      r.FormValue("hls_format"),
		"force_software":  r.FormValue("force_software") == "true",
		"padding":         r.FormValue("padding"),
	}
	if config["codec_selection"] == "" {
		config["codec_selection"] = "both"
	}
	if config["hls_format"] == "" {
		config["hls_format"] = "fmp4"
	}
	job := store.Job{
		JobID:     jobID,
		Name:      header.Filename,
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
	writeJSON(w, http.StatusOK, map[string]any{"job_id": jobID, "source_id": sourceID, "status": "queued"})
}

func (h *Handler) ReencodeSource(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "invalid form data"})
		return
	}
	sourceID := mux.Vars(r)["source_id"]
	source, err := h.App.Store.GetSource(sourceID)
	if err != nil || source == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"detail": "Source not found"})
		return
	}
	outputName := r.FormValue("output_name")
	if outputName == "" {
		outputName = source.Name
	}
	outputName = util.SanitizeName(outputName)
	codecSelection := r.FormValue("codec_selection")
	if codecSelection == "" {
		codecSelection = "both"
	}
	partialDurations := r.MultipartForm.Value["partial_durations"]
	if len(partialDurations) == 0 {
		partialDurations = []string{"200"}
	}
	segmentDuration, _ := parseInt(r.FormValue("segment_duration"))
	if segmentDuration == 0 {
		segmentDuration = 6
	}
	gopDuration, _ := parseFloat(r.FormValue("gop_duration"))
	if gopDuration == 0 {
		gopDuration = 1
	}
	jobIDs := []string{}
	for _, pdur := range partialDurations {
		jobID := util.NewUUID()
		config := map[string]interface{}{
			"output_name":      outputName + "_p" + pdur,
			"codec_selection":  codecSelection,
			"max_resolution":   r.FormValue("max_resolution"),
			"hls_format":       r.FormValue("hls_format"),
			"force_software":   r.FormValue("force_software") == "true",
			"padding":          r.FormValue("padding"),
			"duration_limit":   parseIntDefault(r.FormValue("duration_limit"), 0),
			"partial_duration": pdur,
			"segment_duration": segmentDuration,
			"gop_duration":     gopDuration,
			"keep_mezzanine":   r.FormValue("keep_mezzanine") == "true",
		}
		job := store.Job{
			JobID:     jobID,
			Name:      "Re-encode: " + source.OriginalFilename + " (p" + pdur + ")",
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
		jobIDs = append(jobIDs, jobID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"job_ids": jobIDs, "source_id": sourceID, "status": "queued"})
}

func (h *Handler) BatchReencodeJSON(w http.ResponseWriter, r *http.Request) {
	var req BatchReencodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "invalid request body"})
		return
	}
	if len(req.SourceIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "source_ids required"})
		return
	}
	if len(req.PartialDurations) == 0 {
		req.PartialDurations = []int{200}
	}
	if req.CodecSelection == "" {
		req.CodecSelection = "both"
	}
	if req.HlsFormat == "" {
		req.HlsFormat = "fmp4"
	}
	if req.SegmentDuration == 0 {
		req.SegmentDuration = 6
	}
	if req.GopDuration == 0 {
		req.GopDuration = 1
	}
	jobs := []map[string]string{}
	for _, sourceID := range req.SourceIDs {
		source, err := h.App.Store.GetSource(sourceID)
		if err != nil || source == nil {
			continue
		}
		for _, pdur := range req.PartialDurations {
			jobID := util.NewUUID()
			maxRes := ""
			if req.MaxResolution != nil {
				maxRes = *req.MaxResolution
			}
			durationLimit := 0
			if req.DurationLimit != nil {
				durationLimit = *req.DurationLimit
			}
			config := map[string]interface{}{
				"output_name":      source.Name + "_p" + strconv.Itoa(pdur),
				"codec_selection":  req.CodecSelection,
				"max_resolution":   maxRes,
				"hls_format":       req.HlsFormat,
				"force_software":   req.ForceSoftware,
				"padding":          req.Padding,
				"duration_limit":   durationLimit,
				"partial_duration": pdur,
				"segment_duration": req.SegmentDuration,
				"gop_duration":     req.GopDuration,
				"keep_mezzanine":   req.KeepMezzanine,
			}
			job := store.Job{
				JobID:     jobID,
				Name:      "Batch re-encode: " + source.OriginalFilename + " (p" + strconv.Itoa(pdur) + ")",
				Status:    "queued",
				Progress:  0,
				Config:    config,
				CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
				SourceID:  &sourceID,
			}
			if err := h.App.Store.CreateJob(job); err != nil {
				continue
			}
			h.App.EnqueueJob(jobID)
			jobs = append(jobs, map[string]string{"job_id": jobID, "source_id": sourceID})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs, "total": len(jobs)})
}

func (h *Handler) GenerateByteranges(w http.ResponseWriter, r *http.Request) {
	content := mux.Vars(r)["content_name"]
	contentPath := filepath.Join(h.App.Cfg.OutputDir, content)
	ok, err := util.ValidateDir(contentPath)
	if err != nil || !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "Content directory not found"})
		return
	}
	segments, err := util.FindSegments(contentPath)
	if err != nil || len(segments) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "No segments found"})
		return
	}
	result := util.GenerateByteranges(segments, contentPath, content)
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) ScanOriginals(w http.ResponseWriter, _ *http.Request) {
	stats := util.ScanOriginals(h.App.Store, h.App.Cfg.SourcesDir)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Scan complete",
		"stats":   stats,
	})
}

func (h *Handler) StreamLogs(w http.ResponseWriter, r *http.Request) {
	jobID := mux.Vars(r)["job_id"]
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	h.App.LogHub.Add(jobID, conn)
	defer func() {
		h.App.LogHub.Remove(jobID, conn)
		_ = conn.Close()
	}()

	logPath := filepath.Join(h.App.Cfg.UploadsDir, jobID, "encoding.log")
	if _, err := os.Stat(logPath); err == nil {
		if data, err := os.ReadFile(logPath); err == nil {
			_ = conn.WriteMessage(websocket.TextMessage, data)
		}
	}
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}
