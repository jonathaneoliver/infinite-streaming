package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	db *sql.DB
}

type JobStatusUpdate struct {
	Status      string
	Progress    *int
	StartedAt   *string
	CompletedAt *string
	ErrorMsg    *string
	OutputPaths interface{}
}

type Job struct {
	JobID       string                 `json:"job_id"`
	Name        string                 `json:"name"`
	Status      string                 `json:"status"`
	Progress    int                    `json:"progress"`
	Config      map[string]interface{} `json:"config"`
	CreatedAt   string                 `json:"created_at"`
	StartedAt   *string                `json:"started_at"`
	CompletedAt *string                `json:"completed_at"`
	ErrorMsg    *string                `json:"error_message"`
	LogPath     *string                `json:"log_path"`
	OutputPaths interface{}            `json:"output_paths"`
	SourceID    *string                `json:"source_id"`
}

type Source struct {
	SourceID         string                 `json:"source_id"`
	Name             string                 `json:"name"`
	OriginalFilename string                 `json:"original_filename"`
	FilePath         string                 `json:"file_path"`
	FileSize         int64                  `json:"file_size"`
	Duration         *float64               `json:"duration"`
	Resolution       *string                `json:"resolution"`
	Codec            *string                `json:"codec"`
	UploadedAt       string                 `json:"uploaded_at"`
	Metadata         map[string]interface{} `json:"metadata"`
}

func DatabasePathFromEnv() string {
	base := os.Getenv("INFINITE_STREAM_DATABASE_DIR")
	if base == "" {
		base = os.Getenv("INFINITE_DATABASE_DIR")
	}
	if base == "" {
		base = os.Getenv("ISM_DATABASE_DIR")
	}
	if base == "" {
		base = "/media/data"
	}
	return filepath.Join(base, "encoding_jobs.db")
}

func NewSQLiteStore(dbPath string) (*SQLiteStore, error) {
	// WAL mode allows concurrent reads alongside a single writer.
	// busy_timeout makes writers wait briefly on contention instead of failing
	// immediately with SQLITE_BUSY.
	dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}
	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) InitSchema() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS jobs (
			job_id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			status TEXT NOT NULL,
			progress INTEGER DEFAULT 0,
			config TEXT NOT NULL,
			created_at TEXT NOT NULL,
			started_at TEXT,
			completed_at TEXT,
			error_message TEXT,
			log_path TEXT,
			output_paths TEXT,
			source_id TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS sources (
			source_id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			original_filename TEXT NOT NULL,
			file_path TEXT NOT NULL,
			file_size INTEGER NOT NULL,
			duration REAL,
			resolution TEXT,
			codec TEXT,
			uploaded_at TEXT NOT NULL,
			metadata TEXT
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	if _, err := s.db.Exec(`ALTER TABLE jobs ADD COLUMN source_id TEXT`); err != nil {
		// Ignore if already exists.
		if !isDuplicateColumnError(err) {
			return err
		}
	}

	// One-shot migration: rewrite legacy /boss/ paths to /media/ for source
	// records created before the BOSS→ISM rename. Idempotent.
	if _, err := s.db.Exec(`UPDATE sources SET file_path = REPLACE(file_path, '/boss/', '/media/') WHERE file_path LIKE '/boss/%'`); err != nil {
		return err
	}

	return nil
}

func (s *SQLiteStore) ListJobs() ([]Job, error) {
	// Hide completed/failed jobs older than 48h. Always keep active jobs
	// (queued/uploading/encoding) regardless of age so a long-running encode
	// never disappears from the list mid-run.
	cutoff := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano)
	rows, err := s.db.Query(`SELECT job_id, name, status, progress, config, created_at, started_at, completed_at, error_message, log_path, output_paths, source_id
		FROM jobs
		WHERE created_at >= ? OR status IN ('queued','uploading','encoding')
		ORDER BY created_at DESC`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) GetJob(jobID string) (*Job, error) {
	row := s.db.QueryRow(`SELECT job_id, name, status, progress, config, created_at, started_at, completed_at, error_message, log_path, output_paths, source_id FROM jobs WHERE job_id = ?`, jobID)
	job, err := scanJob(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &job, nil
}

func (s *SQLiteStore) CreateJob(job Job) error {
	configJSON, err := json.Marshal(job.Config)
	if err != nil {
		return err
	}
	var outputJSON *string
	if job.OutputPaths != nil {
		b, err := json.Marshal(job.OutputPaths)
		if err != nil {
			return err
		}
		str := string(b)
		outputJSON = &str
	}
	_, err = s.db.Exec(
		`INSERT INTO jobs (job_id, name, status, progress, config, created_at, started_at, completed_at, error_message, log_path, output_paths, source_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.JobID,
		job.Name,
		job.Status,
		job.Progress,
		string(configJSON),
		job.CreatedAt,
		nullString(job.StartedAt),
		nullString(job.CompletedAt),
		nullString(job.ErrorMsg),
		nullString(job.LogPath),
		outputJSON,
		nullString(job.SourceID),
	)
	return err
}

func (s *SQLiteStore) UpdateJobStatus(jobID string, update JobStatusUpdate) error {
	fields := []string{}
	args := []interface{}{}

	if update.Status != "" {
		fields = append(fields, "status = ?")
		args = append(args, update.Status)
	}
	if update.Progress != nil {
		fields = append(fields, "progress = ?")
		args = append(args, *update.Progress)
	}
	if update.StartedAt != nil {
		fields = append(fields, "started_at = ?")
		args = append(args, *update.StartedAt)
	}
	if update.CompletedAt != nil {
		fields = append(fields, "completed_at = ?")
		args = append(args, *update.CompletedAt)
	}
	if update.ErrorMsg != nil {
		fields = append(fields, "error_message = ?")
		args = append(args, *update.ErrorMsg)
	}
	if update.OutputPaths != nil {
		b, err := json.Marshal(update.OutputPaths)
		if err != nil {
			return err
		}
		fields = append(fields, "output_paths = ?")
		args = append(args, string(b))
	}
	if len(fields) == 0 {
		return nil
	}
	args = append(args, jobID)
	query := fmt.Sprintf("UPDATE jobs SET %s WHERE job_id = ?", joinFields(fields))
	_, err := s.db.Exec(query, args...)
	return err
}

func (s *SQLiteStore) DeleteJob(jobID string) error {
	_, err := s.db.Exec(`DELETE FROM jobs WHERE job_id = ?`, jobID)
	return err
}

func (s *SQLiteStore) ListSources() ([]Source, error) {
	rows, err := s.db.Query(`SELECT source_id, name, original_filename, file_path, file_size, duration, resolution, codec, uploaded_at, metadata FROM sources ORDER BY uploaded_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) GetSource(sourceID string) (*Source, error) {
	row := s.db.QueryRow(`SELECT source_id, name, original_filename, file_path, file_size, duration, resolution, codec, uploaded_at, metadata FROM sources WHERE source_id = ?`, sourceID)
	src, err := scanSource(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &src, nil
}

func (s *SQLiteStore) CreateSource(src Source) error {
	metaJSON, err := json.Marshal(src.Metadata)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO sources (source_id, name, original_filename, file_path, file_size, duration, resolution, codec, uploaded_at, metadata)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		src.SourceID,
		src.Name,
		src.OriginalFilename,
		src.FilePath,
		src.FileSize,
		src.Duration,
		src.Resolution,
		src.Codec,
		src.UploadedAt,
		string(metaJSON),
	)
	return err
}

func (s *SQLiteStore) DeleteSource(sourceID string) error {
	_, err := s.db.Exec(`DELETE FROM sources WHERE source_id = ?`, sourceID)
	return err
}

func (s *SQLiteStore) CountJobsByStatus(status string) (int, error) {
	row := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status = ?`, status)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *SQLiteStore) CountJobsForSource(sourceID string, statuses []string) (int, error) {
	if len(statuses) == 0 {
		return 0, nil
	}
	placeholders := make([]string, 0, len(statuses))
	args := []interface{}{sourceID}
	for _, status := range statuses {
		placeholders = append(placeholders, "?")
		args = append(args, status)
	}
	query := fmt.Sprintf(`SELECT COUNT(*) FROM jobs WHERE source_id = ? AND status IN (%s)`, joinFields(placeholders))
	row := s.db.QueryRow(query, args...)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

type jobScanner interface {
	Scan(dest ...any) error
}

type sourceScanner interface {
	Scan(dest ...any) error
}

func scanJob(row jobScanner) (Job, error) {
	var job Job
	var configStr string
	var outputStr sql.NullString
	var startedAt sql.NullString
	var completedAt sql.NullString
	var errorMsg sql.NullString
	var logPath sql.NullString
	var sourceID sql.NullString

	err := row.Scan(
		&job.JobID,
		&job.Name,
		&job.Status,
		&job.Progress,
		&configStr,
		&job.CreatedAt,
		&startedAt,
		&completedAt,
		&errorMsg,
		&logPath,
		&outputStr,
		&sourceID,
	)
	if err != nil {
		return job, err
	}

	job.Config = map[string]interface{}{}
	if configStr != "" {
		if err := json.Unmarshal([]byte(configStr), &job.Config); err != nil {
			return job, fmt.Errorf("decode job.config: %w", err)
		}
	}
	job.OutputPaths = nil
	if outputStr.Valid && outputStr.String != "" {
		var outputAny interface{}
		if err := json.Unmarshal([]byte(outputStr.String), &outputAny); err != nil {
			return job, fmt.Errorf("decode job.output_paths: %w", err)
		}
		job.OutputPaths = outputAny
	}
	if startedAt.Valid {
		job.StartedAt = &startedAt.String
	}
	if completedAt.Valid {
		job.CompletedAt = &completedAt.String
	}
	if errorMsg.Valid {
		job.ErrorMsg = &errorMsg.String
	}
	if logPath.Valid {
		job.LogPath = &logPath.String
	}
	if sourceID.Valid {
		job.SourceID = &sourceID.String
	}

	return job, nil
}

func scanSource(row sourceScanner) (Source, error) {
	var src Source
	var duration sql.NullFloat64
	var resolution sql.NullString
	var codec sql.NullString
	var metadata sql.NullString

	err := row.Scan(
		&src.SourceID,
		&src.Name,
		&src.OriginalFilename,
		&src.FilePath,
		&src.FileSize,
		&duration,
		&resolution,
		&codec,
		&src.UploadedAt,
		&metadata,
	)
	if err != nil {
		return src, err
	}

	if duration.Valid {
		src.Duration = &duration.Float64
	}
	if resolution.Valid {
		src.Resolution = &resolution.String
	}
	if codec.Valid {
		src.Codec = &codec.String
	}
	src.Metadata = map[string]interface{}{}
	if metadata.Valid && metadata.String != "" {
		if err := json.Unmarshal([]byte(metadata.String), &src.Metadata); err != nil {
			return src, fmt.Errorf("decode source.metadata: %w", err)
		}
	}

	return src, nil
}

func nullString(v *string) interface{} {
	if v == nil {
		return nil
	}
	return *v
}

func joinFields(fields []string) string {
	if len(fields) == 0 {
		return ""
	}
	out := fields[0]
	for i := 1; i < len(fields); i++ {
		out += ", " + fields[i]
	}
	return out
}

func isDuplicateColumnError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "duplicate column") || strings.Contains(err.Error(), "already exists")
}
