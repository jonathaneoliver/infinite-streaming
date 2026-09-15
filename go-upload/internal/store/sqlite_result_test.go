package store

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
)

func TestJobResultRoundTrip(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "jobs.db"))
	if err := st.CreateJob(Job{JobID: "j1", Name: "n", Status: "queued", Config: map[string]interface{}{"codec_selection": "both"}, CreatedAt: "2026-09-15T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	job, err := st.GetJob("j1")
	if err != nil || job == nil {
		t.Fatalf("GetJob: %v %v", job, err)
	}
	if job.Result != nil {
		t.Fatalf("new job Result = %#v, want nil", job.Result)
	}

	res := map[string]interface{}{
		"encoders":        map[string]interface{}{"h264": "libx264 (software)", "hevc": "libx265 (software)"},
		"padding":         "applied",
		"padding_video_s": 1.234,
	}
	// A result-only update must not clobber other fields.
	if err := st.UpdateJobStatus("j1", JobStatusUpdate{Result: res}); err != nil {
		t.Fatal(err)
	}
	job, _ = st.GetJob("j1")
	if !reflect.DeepEqual(job.Result, res) {
		t.Fatalf("GetJob Result = %#v, want %#v", job.Result, res)
	}
	if job.Status != "queued" || job.Config["codec_selection"] != "both" {
		t.Fatalf("result-only update changed other fields: status=%q config=%#v", job.Status, job.Config)
	}
	jobs, err := st.ListJobs()
	if err != nil || len(jobs) != 1 || !reflect.DeepEqual(jobs[0].Result, res) {
		t.Fatalf("ListJobs = %#v, %v", jobs, err)
	}
}

// A database created before the result column existed must upgrade in place.
func TestJobResultMigratesExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE jobs (job_id TEXT PRIMARY KEY, name TEXT NOT NULL, status TEXT NOT NULL,
		progress INTEGER DEFAULT 0, config TEXT NOT NULL, created_at TEXT NOT NULL, started_at TEXT, completed_at TEXT,
		error_message TEXT, log_path TEXT, output_paths TEXT, source_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO jobs (job_id, name, status, config, created_at) VALUES ('legacy', 'n', 'complete', '{}', '2026-09-15T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	st := openStore(t, path)
	job, err := st.GetJob("legacy")
	if err != nil || job == nil {
		t.Fatalf("GetJob(legacy) = %v, %v", job, err)
	}
	if job.Result != nil {
		t.Fatalf("legacy job Result = %#v, want nil (not recorded)", job.Result)
	}
	if err := st.UpdateJobStatus("legacy", JobStatusUpdate{Result: map[string]interface{}{"padding": "none"}}); err != nil {
		t.Fatalf("writing result to migrated database: %v", err)
	}
	// Reopening an already-migrated database must not fail on the ALTER.
	openStore(t, path)
}

// openStore opens a store the way cmd/server/main.go does: NewSQLiteStore,
// then InitSchema, which creates tables and runs column migrations.
func openStore(t *testing.T, path string) *SQLiteStore {
	t.Helper()
	st, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore(%s): %v", path, err)
	}
	if err := st.InitSchema(); err != nil {
		t.Fatalf("InitSchema(%s): %v", path, err)
	}
	return st
}
