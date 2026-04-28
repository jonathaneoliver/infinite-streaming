package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"

	"github.com/jonathaneoliver/infinite-streaming/go-proxy/internal/har"
)

// incidentDir is where HAR snapshot files live on disk.
//
// Layout: {incidentDir}/{YYYY-MM-DD}/{session_id}__{reason}__{ts}.har
//
// Per-player retention (100 files OR 7 days, whichever is stricter) is enforced
// by pruneIncidents on each new write — we never run a background sweeper.
var (
	incidentDirOnce sync.Once
	incidentDirPath string
)

const (
	incidentRetentionMaxFilesPerPlayer = 100
	incidentRetentionMaxAge            = 7 * 24 * time.Hour
)

func resolveIncidentDir() string {
	incidentDirOnce.Do(func() {
		incidentDirPath = os.Getenv("HAR_INCIDENTS_DIR")
		if incidentDirPath == "" {
			incidentDirPath = "/incidents"
		}
	})
	return incidentDirPath
}

// SnapshotRequest is the body for POST /api/session/{id}/har/snapshot.
type SnapshotRequest struct {
	Reason   string                 `json:"reason"`
	Source   string                 `json:"source"` // "dashboard", "rest", "player"
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// IncidentFileInfo describes a stored HAR file in listings.
type IncidentFileInfo struct {
	Filename  string    `json:"filename"`
	Path      string    `json:"path"` // path relative to incident dir, for download endpoint
	SessionID string    `json:"session_id"`
	PlayerID  string    `json:"player_id,omitempty"`
	Reason    string    `json:"reason"`
	Source    string    `json:"source,omitempty"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

// buildHARForSession reads the session's network ring buffer and returns a HAR
// document. Caller is expected to have already validated the session exists.
func (a *App) buildHARForSession(session SessionData, incident *har.Incident) har.HAR {
	sessionID := getString(session, "session_id")

	a.networkLogsMu.RLock()
	rb, exists := a.networkLogs[sessionID]
	a.networkLogsMu.RUnlock()

	var sources []har.Source
	if exists {
		entries := rb.GetAll()
		sources = make([]har.Source, 0, len(entries))
		for _, e := range entries {
			sources = append(sources, har.Source{
				Timestamp:     e.Timestamp,
				Method:        e.Method,
				URL:           e.URL,
				RequestKind:   e.RequestKind,
				Status:        e.Status,
				BytesIn:       e.BytesIn,
				BytesOut:      e.BytesOut,
				ContentType:   e.ContentType,
				DNSMs:         e.DNSMs,
				ConnectMs:     e.ConnectMs,
				TLSMs:         e.TLSMs,
				TTFBMs:        e.TTFBMs,
				TransferMs:    e.TransferMs,
				TotalMs:       e.TotalMs,
				Faulted:       e.Faulted,
				FaultType:     e.FaultType,
				FaultAction:   e.FaultAction,
				FaultCategory: e.FaultCategory,
			})
		}
	}

	opts := har.BuildOptions{
		SessionID: sessionID,
		PlayerID:  getString(session, "player_id"),
		GroupID:   getString(session, "group_id"),
		Incident:  incident,
	}
	if incident != nil {
		if incident.PlayerID == "" {
			incident.PlayerID = opts.PlayerID
		}
		if incident.SessionID == "" {
			incident.SessionID = sessionID
		}
		if incident.GroupID == "" {
			incident.GroupID = opts.GroupID
		}
	}

	return har.Build(sources, opts)
}

// findSessionByID returns the session map matching session_id, or nil.
func (a *App) findSessionByID(sessionID string) SessionData {
	for _, s := range a.getSessionList() {
		if getString(s, "session_id") == sessionID {
			return s
		}
	}
	return nil
}

// handleGetSessionTimelineHAR fetches the live timeline for a player_id as HAR.
// GET /api/sessions/{player_id}/timeline.har
func (a *App) handleGetSessionTimelineHAR(w http.ResponseWriter, r *http.Request) {
	playerID := mux.Vars(r)["player_id"]
	if playerID == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "player_id required"})
		return
	}

	sessions := a.getSessionList()
	session := findSessionByPlayerID(sessions, playerID)
	if session == nil {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]string{"error": "session not found for player_id"})
		return
	}

	doc := a.buildHARForSession(session, nil)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="timeline-%s.har"`, safeFilename(playerID)))
	_ = json.NewEncoder(w).Encode(doc)
}

// handlePostHARSnapshot persists the current session timeline to disk.
// POST /api/session/{id}/har/snapshot
func (a *App) handlePostHARSnapshot(w http.ResponseWriter, r *http.Request) {
	sessionID := mux.Vars(r)["id"]
	if sessionID == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "session id required"})
		return
	}

	var req SnapshotRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]string{"error": "invalid json"})
			return
		}
	}
	if req.Reason == "" {
		req.Reason = "manual"
	}
	if req.Source == "" {
		req.Source = "rest"
	}

	session := a.findSessionByID(sessionID)
	if session == nil {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]string{"error": "session not found"})
		return
	}

	incident := &har.Incident{
		Reason:    req.Reason,
		Source:    req.Source,
		SessionID: sessionID,
		Timestamp: time.Now().UTC(),
		Metadata:  req.Metadata,
	}

	doc := a.buildHARForSession(session, incident)
	playerID := getString(session, "player_id")
	info, err := writeIncidentFile(sessionID, playerID, req.Reason, req.Source, doc)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": fmt.Sprintf("write failed: %v", err)})
		return
	}

	go pruneIncidents(playerID)

	writeJSON(w, map[string]interface{}{
		"status":   "ok",
		"incident": info,
	})
}

// handleListIncidents lists saved HAR files. Optional ?player_id=X filter.
// GET /api/incidents
func (a *App) handleListIncidents(w http.ResponseWriter, r *http.Request) {
	playerFilter := strings.TrimSpace(r.URL.Query().Get("player_id"))
	files, err := listIncidentFiles()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if playerFilter != "" {
		filtered := files[:0]
		for _, f := range files {
			if f.PlayerID == playerFilter {
				filtered = append(filtered, f)
			}
		}
		files = filtered
	}
	writeJSON(w, map[string]interface{}{
		"count":     len(files),
		"incidents": files,
	})
}

// handleGetIncidentFile streams a saved HAR file.
// GET /api/incidents/{path:.*}
func (a *App) handleGetIncidentFile(w http.ResponseWriter, r *http.Request) {
	relPath := mux.Vars(r)["path"]
	if relPath == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "path required"})
		return
	}
	// Reject path traversal — only forward-slash-joined components, no `..`.
	if strings.Contains(relPath, "..") || strings.HasPrefix(relPath, "/") {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid path"})
		return
	}
	// Belt-and-suspenders: resolve absolute paths and ensure the file lives
	// under the incidents dir even if mux's URL decoding produced something
	// unexpected.
	root, err := filepath.Abs(resolveIncidentDir())
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": "incidents dir resolve failed"})
		return
	}
	full := filepath.Clean(filepath.Join(root, filepath.FromSlash(relPath)))
	if !strings.HasPrefix(full, root+string(filepath.Separator)) && full != root {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid path"})
		return
	}
	f, err := os.Open(full)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]string{"error": "not found"})
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s"`, filepath.Base(full)))
	_, _ = io.Copy(w, f)
}

// writeIncidentFile persists doc under {incidentDir}/{date}/{filename}.
func writeIncidentFile(sessionID, playerID, reason, source string, doc har.HAR) (IncidentFileInfo, error) {
	now := time.Now().UTC()
	dateDir := now.Format("2006-01-02")
	root := resolveIncidentDir()
	dir := filepath.Join(root, dateDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return IncidentFileInfo{}, err
	}

	ts := now.Format("20060102T150405Z")
	filename := fmt.Sprintf("%s__%s__%s.har",
		safeFilename(sessionID),
		safeFilename(reason),
		ts,
	)
	full := filepath.Join(dir, filename)

	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return IncidentFileInfo{}, err
	}
	if err := os.WriteFile(full, body, 0o644); err != nil {
		return IncidentFileInfo{}, err
	}

	info := IncidentFileInfo{
		Filename:  filename,
		Path:      filepath.ToSlash(filepath.Join(dateDir, filename)),
		SessionID: sessionID,
		PlayerID:  playerID,
		Reason:    reason,
		Source:    source,
		SizeBytes: int64(len(body)),
		CreatedAt: now,
	}
	return info, nil
}

// listIncidentFiles walks the incident dir and returns metadata sorted newest first.
func listIncidentFiles() ([]IncidentFileInfo, error) {
	root := resolveIncidentDir()
	out := []IncidentFileInfo{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".har") {
			return nil
		}
		info, ferr := readIncidentMeta(path)
		if ferr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		info.Path = filepath.ToSlash(rel)
		out = append(out, info)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// readIncidentMeta parses a HAR file just enough to extract session/player/reason.
// Falls back to filename parsing if the JSON is malformed.
func readIncidentMeta(path string) (IncidentFileInfo, error) {
	st, err := os.Stat(path)
	if err != nil {
		return IncidentFileInfo{}, err
	}
	info := IncidentFileInfo{
		Filename:  filepath.Base(path),
		SizeBytes: st.Size(),
		CreatedAt: st.ModTime().UTC(),
	}

	// Try to read the _extensions block for richer metadata.
	body, err := os.ReadFile(path)
	if err == nil {
		var doc har.HAR
		if jerr := json.Unmarshal(body, &doc); jerr == nil {
			if ext := doc.Log.Extensions; ext != nil {
				if sess, ok := ext["session"].(map[string]interface{}); ok {
					if v, ok := sess["session_id"].(string); ok {
						info.SessionID = v
					}
					if v, ok := sess["player_id"].(string); ok {
						info.PlayerID = v
					}
				}
				if inc, ok := ext["incident"].(map[string]interface{}); ok {
					if v, ok := inc["reason"].(string); ok {
						info.Reason = v
					}
					if v, ok := inc["source"].(string); ok {
						info.Source = v
					}
					if info.PlayerID == "" {
						if v, ok := inc["player_id"].(string); ok {
							info.PlayerID = v
						}
					}
				}
			}
		}
	}

	// Filename fallback: {sessionID}__{reason}__{ts}.har
	if info.SessionID == "" || info.Reason == "" {
		parts := strings.SplitN(strings.TrimSuffix(info.Filename, ".har"), "__", 3)
		if len(parts) >= 2 {
			if info.SessionID == "" {
				info.SessionID = parts[0]
			}
			if info.Reason == "" {
				info.Reason = parts[1]
			}
		}
	}
	return info, nil
}

// pruneIncidents enforces per-player retention (max files OR max age).
//
// Called as a fire-and-forget goroutine after each write. Errors are logged
// but never block the snapshot response.
func pruneIncidents(playerID string) {
	files, err := listIncidentFiles()
	if err != nil {
		return
	}

	// Drop files older than retention window (any player).
	cutoff := time.Now().UTC().Add(-incidentRetentionMaxAge)
	root := resolveIncidentDir()
	for _, f := range files {
		if f.CreatedAt.Before(cutoff) {
			_ = os.Remove(filepath.Join(root, filepath.FromSlash(f.Path)))
		}
	}

	// If we know the player, enforce per-player file count.
	if playerID == "" {
		return
	}
	var mine []IncidentFileInfo
	for _, f := range files {
		if f.PlayerID == playerID && f.CreatedAt.After(cutoff) {
			mine = append(mine, f)
		}
	}
	if len(mine) <= incidentRetentionMaxFilesPerPlayer {
		return
	}
	// Already sorted newest first; remove the tail beyond the cap.
	for _, f := range mine[incidentRetentionMaxFilesPerPlayer:] {
		_ = os.Remove(filepath.Join(root, filepath.FromSlash(f.Path)))
	}
}

// safeFilename strips characters that misbehave on common filesystems.
func safeFilename(s string) string {
	if s == "" {
		return "unknown"
	}
	repl := func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '-' || r == '_' || r == '.':
			return r
		}
		return '_'
	}
	out := strings.Map(repl, s)
	if len(out) > 80 {
		out = out[:80]
	}
	return out
}
