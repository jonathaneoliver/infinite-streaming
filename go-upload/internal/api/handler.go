package api

import (
	"encoding/json"
	"net/http"

	"github.com/jonathaneoliver/infinite-streaming/go-upload/internal/app"
)

type Handler struct {
	App *app.App
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

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
