package main

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/go-live/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/go-live/internal/manager"
	"github.com/gorilla/mux"
)

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		elapsed := time.Since(start)
		if os.Getenv("GO_LIVE_VERBOSE_LOGS") != "" {
			fmt.Fprintf(os.Stderr, "[request] %s %s -> %d (%s)\n", r.Method, r.URL.Path, sw.status, elapsed)
		}
	})
}

func main() {
	fmt.Println("Starting go-live LL-HLS server...")
	mgr := manager.NewProcessManager()
	tracker := api.NewStreamTracker()
	h := &api.Handler{Manager: mgr, Tracker: tracker}

	router := mux.NewRouter()

	// Simple health check - this MUST work
	router.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("health OK"))
	}).Methods(http.MethodGet, http.MethodHead)

	// Go-live API routes
	router.HandleFunc("/go-live/healthz", h.Healthz).Methods(http.MethodGet, http.MethodHead)
	router.HandleFunc("/go-live/api/status", h.Status).Methods(http.MethodGet, http.MethodHead)
	router.HandleFunc("/go-live/api/stop/{id}", h.Stop).Methods("DELETE")
	router.HandleFunc("/go-live/api/tick-stats/{content}", h.TickStats).Methods(http.MethodGet, http.MethodHead)
	router.HandleFunc("/go-live/api/dash-tick-stats/{content}", h.DashTickStats).Methods(http.MethodGet, http.MethodHead)

	// LL-HLS on-demand routes
	// Master playlist: /go-live/{content}/master.m3u8
	router.HandleFunc("/go-live/{content}/master.m3u8", h.OnDemandMasterPlaylist).Methods(http.MethodGet, http.MethodHead)
	// Master playlist with virtual durations
	router.HandleFunc("/go-live/{content}/master_{duration:(?:2s|6s)}.m3u8", h.OnDemandMasterPlaylistDuration).Methods(http.MethodGet, http.MethodHead)
	router.HandleFunc("/go-live/{content}/{duration:(?:2s|6s)}/master.m3u8", h.OnDemandMasterPlaylistDuration).Methods(http.MethodGet, http.MethodHead)

	// Variant playlists with virtual durations
	router.HandleFunc("/go-live/{content}/playlist_{duration:(?:2s|6s)}_{variant:.*}\\.m3u8", h.OnDemandVariantPlaylistDuration).Methods(http.MethodGet, http.MethodHead)
	router.HandleFunc("/go-live/{content}/{duration:(?:2s|6s)}/{variant:.*\\.m3u8}", h.OnDemandVariantPlaylistDuration).Methods(http.MethodGet, http.MethodHead)
	// Variant playlists: /go-live/{content}/{variant}.m3u8
	// This catches paths like /go-live/content/1080p/index.m3u8
	router.HandleFunc("/go-live/{content}/{variant:.*\\.m3u8}", h.OnDemandVariantPlaylist).Methods(http.MethodGet, http.MethodHead)

	// Segment/init files: /go-live/{content}/{path}
	router.HandleFunc("/go-live/{content}/{path:.*\\.(?:ts|m4s|mp4|m4a|cmfv|cmfa|webm|m4v|aac|webvtt)}", h.ServeSegment).Methods(http.MethodGet, http.MethodHead)

	// DASH live manifest generation (Go rewrite of dash_live.py)
	router.HandleFunc("/go-live/{content}/{path:.*\\.mpd}", h.OnDemandDashManifest).Methods(http.MethodGet, http.MethodHead)

	fmt.Println("Routes registered:")
	fmt.Println("  GET  /health - Health check")
	fmt.Println("  GET  /go-live/healthz - LL-HLS service health")
	fmt.Println("  GET  /go-live/api/status - Active processes status")
	fmt.Println("  DEL  /go-live/api/stop/{id} - Stop generator")
	fmt.Println("  GET  /go-live/{content}/master.m3u8 - On-demand master playlist")
	fmt.Println("  GET  /go-live/{content}/{variant}.m3u8 - On-demand variant playlist")
	fmt.Println("  GET  /go-live/{content}/manifest.mpd - On-demand DASH MPD (LL)")
	fmt.Println("  GET  /go-live/{content}/manifest_2s.mpd - On-demand DASH MPD (2s)")
	fmt.Println("  GET  /go-live/{content}/manifest_6s.mpd - On-demand DASH MPD (6s)")
	fmt.Println()
	fmt.Println("Starting server on :8010")
	h.StartIdleReaper()

	http.ListenAndServe(":8010", requestLogger(router))
}
