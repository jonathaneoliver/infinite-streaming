package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gorilla/mux"
	"github.com/jonathaneoliver/infinite-streaming/go-upload/internal/announce"
	"github.com/jonathaneoliver/infinite-streaming/go-upload/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/go-upload/internal/app"
	"github.com/jonathaneoliver/infinite-streaming/go-upload/internal/config"
	"github.com/jonathaneoliver/infinite-streaming/go-upload/internal/store"
)

func main() {
	addr := ":8003"
	if v := os.Getenv("GO_UPLOAD_ADDR"); v != "" {
		addr = v
	}

	cfg := config.Load()
	if err := app.EnsureDirs(cfg); err != nil {
		log.Fatalf("failed to ensure directories: %v", err)
	}
	st, err := store.NewSQLiteStore(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("failed to open sqlite db: %v", err)
	}
	if err := st.InitSchema(); err != nil {
		log.Fatalf("failed to init db schema: %v", err)
	}

	r := mux.NewRouter()
	application := app.New(st, cfg)
	application.StartWorker(context.Background())
	application.RequeueInterruptedJobs()
	h := api.NewHandler(application)

	r.HandleFunc("/", h.Health).Methods(http.MethodGet)
	r.HandleFunc("/health", h.Health).Methods(http.MethodGet)
	r.HandleFunc("/api/rendezvous", h.RendezvousURL).Methods(http.MethodGet)
	r.HandleFunc("/api/announce-now", h.AnnounceNow).Methods(http.MethodPost)
	// Jobs
	r.HandleFunc("/api/jobs", h.ListJobs).Methods(http.MethodGet)
	r.HandleFunc("/api/jobs/{job_id}", h.GetJob).Methods(http.MethodGet)
	r.HandleFunc("/api/jobs/{job_id}/cancel", h.CancelJob).Methods(http.MethodPost)
	r.HandleFunc("/api/jobs/{job_id}", h.DeleteJob).Methods(http.MethodDelete)
	r.HandleFunc("/api/jobs/{job_id}/stream", h.StreamLogs)
	// Sources
	r.HandleFunc("/api/sources", h.ListSources).Methods(http.MethodGet)
	r.HandleFunc("/api/sources/{source_id}", h.GetSource).Methods(http.MethodGet)
	r.HandleFunc("/api/sources/{source_id}", h.DeleteSource).Methods(http.MethodDelete)
	r.HandleFunc("/api/sources/{source_id}/reencode", h.ReencodeSource).Methods(http.MethodPost)
	r.HandleFunc("/api/sources/batch-reencode-json", h.BatchReencodeJSON).Methods(http.MethodPost)
	r.HandleFunc("/api/sources/scan-originals", h.ScanOriginals).Methods(http.MethodPost)
	// Uploads
	r.HandleFunc("/api/upload/active", h.UploadActive).Methods(http.MethodGet)
	r.HandleFunc("/api/upload", h.UploadFile).Methods(http.MethodPost)
	r.HandleFunc("/api/upload/init", h.UploadInit).Methods(http.MethodPost)
	r.HandleFunc("/api/upload/chunk/{job_id}", h.UploadChunk).Methods(http.MethodPost)
	r.HandleFunc("/api/upload/complete/{job_id}", h.UploadComplete).Methods(http.MethodPost)
	// Utilities
	r.HandleFunc("/api/content/{content_name}/generate-byteranges", h.GenerateByteranges).Methods(http.MethodPost)
	r.HandleFunc("/api/content", h.ListContent).Methods(http.MethodGet)
	// Setup
	r.HandleFunc("/api/setup", h.SetupStatus).Methods(http.MethodGet)
	r.HandleFunc("/api/setup/initialize", h.SetupInitialize).Methods(http.MethodPost)
	r.HandleFunc("/api/setup/seed", h.SetupSeed).Methods(http.MethodPost)

	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Optional: announce this server's URL to the pairing rendezvous so the
	// standalone /pair page lists us. Opt-in via INFINITE_STREAM_ANNOUNCE_URL.
	// (Replaces an earlier mDNS/Bonjour attempt that didn't work through
	// Docker bridge networking.)
	announceMgr := announce.New(filepath.Dir(cfg.DatabasePath))
	go announceMgr.Run(context.Background())
	h.Announce = announceMgr

	log.Printf("go-upload listening on %s (db=%s)", addr, cfg.DatabasePath)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}

