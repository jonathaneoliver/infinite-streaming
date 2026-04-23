package app

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/go-upload/internal/config"
	"github.com/jonathaneoliver/infinite-streaming/go-upload/internal/store"
	"github.com/jonathaneoliver/infinite-streaming/go-upload/internal/util"
)

type App struct {
	Store       *store.SQLiteStore
	Cfg         config.Config
	Queue       chan string
	ActiveProcs map[string]*exec.Cmd
	ActiveMu    sync.Mutex
	LogHub      *LogHub
	Progress    *ProgressTracker
}

func New(st *store.SQLiteStore, cfg config.Config) *App {
	return &App{
		Store:       st,
		Cfg:         cfg,
		Queue:       make(chan string, 100),
		ActiveProcs: make(map[string]*exec.Cmd),
		LogHub:      NewLogHub(),
		Progress:    NewProgressTracker(),
	}
}

func (a *App) StartWorker(ctx context.Context) {
	go func() {
		log.Printf("Background worker started")
		for {
			select {
			case <-ctx.Done():
				return
			case jobID := <-a.Queue:
				if err := a.ExecuteEncodingJob(ctx, jobID); err != nil {
					log.Printf("job %s failed: %v", jobID, err)
				}
			}
		}
	}()
}

func (a *App) EnqueueJob(jobID string) {
	select {
	case a.Queue <- jobID:
	default:
		// Avoid blocking if queue is full; enqueue in background.
		go func() { a.Queue <- jobID }()
	}
}

func (a *App) RequeueInterruptedJobs() {
	jobs, err := a.Store.ListJobs()
	if err != nil {
		log.Printf("failed to list jobs for requeue: %v", err)
		return
	}
	for _, job := range jobs {
		if job.Status == "queued" || job.Status == "uploading" || job.Status == "encoding" {
			zero := 0
			if err := a.Store.UpdateJobStatus(job.JobID, store.JobStatusUpdate{
				Status:   "queued",
				Progress: &zero,
			}); err != nil {
				log.Printf("failed to reset job %s: %v", job.JobID, err)
			}
			a.EnqueueJob(job.JobID)
		}
	}
}

func (a *App) ExecuteEncodingJob(ctx context.Context, jobID string) error {
	job, err := a.Store.GetJob(jobID)
	if err != nil || job == nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	zero := 0
	if err := a.Store.UpdateJobStatus(jobID, store.JobStatusUpdate{
		Status:    "encoding",
		Progress:  &zero,
		StartedAt: &now,
	}); err != nil {
		return err
	}

	a.LogHub.Broadcast(jobID, util.TimestampLog("Starting encoding job: "+job.Name))

	cfg := job.Config
	inputFile, logFile, err := a.resolveJobInput(job)
	if err != nil {
		failMsg := err.Error()
		return a.failJob(jobID, failMsg)
	}

	cmdArgs, err := buildAbrCommand(a.Cfg.AbrScript, a.Cfg.OutputDir, inputFile, cfg)
	if err != nil {
		return a.failJob(jobID, err.Error())
	}

	a.LogHub.Broadcast(jobID, util.TimestampLog("Command: "+util.JoinArgs(cmdArgs)))
	a.LogHub.Broadcast(jobID, util.TimestampLog("---"))

	cmd := exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...)
	cmd.Dir = filepath.Dir(a.Cfg.AbrScript)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return a.failJob(jobID, err.Error())
	}
	cmd.Stderr = cmd.Stdout

	a.ActiveMu.Lock()
	a.ActiveProcs[jobID] = cmd
	a.ActiveMu.Unlock()

	if err := cmd.Start(); err != nil {
		return a.failJob(jobID, err.Error())
	}

	var humanLines []string
	_, err = util.StreamLines(stdout, func(line string) {
		// Always feed the parser so `-progress pipe:1` lines (out_time=...) drive
		// the meter; only broadcast/persist the human-readable lines.
		if !isFFmpegProgressLine(line) {
			a.LogHub.Broadcast(jobID, line)
			humanLines = append(humanLines, line+"\n")
		}
		if progress := a.Progress.Parse(jobID, line, cfg); progress != nil {
			_ = a.Store.UpdateJobStatus(jobID, store.JobStatusUpdate{
				Status:   "encoding",
				Progress: progress,
			})
			if msg := a.Progress.Message(jobID); msg != "" {
				a.LogHub.Broadcast(jobID, util.TimestampLog("📊 "+msg))
			}
		}
	})
	if err != nil {
		a.LogHub.Broadcast(jobID, util.TimestampLog("⚠️ "+err.Error()))
	}

	waitErr := cmd.Wait()
	util.WriteLines(logFile, humanLines)

	a.ActiveMu.Lock()
	delete(a.ActiveProcs, jobID)
	a.ActiveMu.Unlock()

	if waitErr != nil {
		return a.failJob(jobID, "Encoding failed: "+waitErr.Error())
	}

	outputPaths := findOutputDirectories(cfg, a.Cfg.OutputDir)
	progress := 100
	doneAt := time.Now().UTC().Format(time.RFC3339Nano)
	if err := a.Store.UpdateJobStatus(jobID, store.JobStatusUpdate{
		Status:      "complete",
		Progress:    &progress,
		CompletedAt: &doneAt,
		OutputPaths: outputPaths,
	}); err != nil {
		return err
	}
	a.LogHub.Broadcast(jobID, util.TimestampLog("✅ Encoding complete"))
	if len(outputPaths) > 0 {
		a.LogHub.Broadcast(jobID, util.TimestampLog("Output: "+util.JoinArgs(outputPaths)))
	}
	return nil
}

func (a *App) failJob(jobID, message string) error {
	errMsg := message
	doneAt := time.Now().UTC().Format(time.RFC3339Nano)
	if err := a.Store.UpdateJobStatus(jobID, store.JobStatusUpdate{
		Status:      "failed",
		ErrorMsg:    &errMsg,
		CompletedAt: &doneAt,
	}); err != nil {
		return err
	}
	a.LogHub.Broadcast(jobID, util.TimestampLog("❌ "+message))
	return nil
}

func (a *App) CancelJob(jobID string) {
	a.ActiveMu.Lock()
	cmd := a.ActiveProcs[jobID]
	a.ActiveMu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func (a *App) resolveJobInput(job *store.Job) (string, string, error) {
	if job.SourceID != nil {
		src, err := a.Store.GetSource(*job.SourceID)
		if err != nil || src == nil {
			return "", "", util.Errf("Source not found: %s", *job.SourceID)
		}
		// Recompute the path against the current SourcesDir so legacy stored
		// paths (e.g. /boss/originals/...) resolve correctly without migration.
		filePath := filepath.Join(a.Cfg.SourcesDir, filepath.Base(src.FilePath))
		return filePath, filepath.Join(a.Cfg.UploadsDir, job.JobID, "encoding.log"), nil
	}
	jobDir := filepath.Join(a.Cfg.UploadsDir, job.JobID)
	return filepath.Join(jobDir, "input.mp4"), filepath.Join(jobDir, "encoding.log"), nil
}

func EnsureDirs(cfg config.Config) error {
	paths := []string{cfg.UploadsDir, cfg.SourcesDir, cfg.OutputDir, filepath.Dir(cfg.DatabasePath)}
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return err
		}
	}
	return nil
}
