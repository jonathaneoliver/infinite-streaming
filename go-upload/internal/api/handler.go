package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/jonathaneoliver/infinite-streaming/go-upload/internal/announce"
	"github.com/jonathaneoliver/infinite-streaming/go-upload/internal/app"
)

type Handler struct {
	App      *app.App
	Announce *announce.Manager
}

func NewHandler(a *app.App) *Handler {
	return &Handler{App: a}
}

func (h *Handler) Health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"service": "go-upload-service",
	})
}

// RendezvousURL exposes the operator's configured pairing rendezvous URL
// (set via INFINITE_STREAM_RENDEZVOUS_URL env var) plus the announce
// label that this server publishes to the rendezvous (so the dashboard
// can show users a friendly name like "test-dev" instead of a bare URL).
// Empty fields when unset. The dashboard reads this to decide whether to
// show the "Pair a TV" form and to label the current server.
func (h *Handler) RendezvousURL(w http.ResponseWriter, _ *http.Request) {
	rzURL := os.Getenv("INFINITE_STREAM_RENDEZVOUS_URL")
	label := strings.TrimSpace(os.Getenv("INFINITE_STREAM_ANNOUNCE_LABEL"))
	if label == "" {
		// Prefer host:port from the announce URL — recognisable across the
		// 4-5 parallel test-deploy containers, where the bare hostname would
		// just be a random docker container ID.
		if announce := strings.TrimSpace(os.Getenv("INFINITE_STREAM_ANNOUNCE_URL")); announce != "" {
			if u, err := url.Parse(announce); err == nil && u.Host != "" {
				label = u.Host
			}
		}
	}
	if label == "" {
		if hn, err := os.Hostname(); err == nil {
			label = hn
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"url":   rzURL,
		"label": label,
	})
}

// AnnounceNow asks the announce loop to fire an extra heartbeat right
// away. Called by the dashboard's Server Info modal so the user can
// re-publish if their boot announce went missing. Always returns 200
// (best-effort; the handler doesn't block on the network call).
func (h *Handler) AnnounceNow(w http.ResponseWriter, _ *http.Request) {
	if h.Announce != nil {
		h.Announce.Trigger()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
