package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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

// incidentPathRe whitelists the exact on-disk shape produced by
// writeIncidentFile: `{YYYY-MM-DD}/{safe-chars}.har`. CodeQL's
// go/path-injection rule recognises this regex match as a sanitiser,
// satisfying static analysis on top of the prefix-validate below.
var incidentPathRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}/[A-Za-z0-9._-]+\.har$`)

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
//
// Players send Device + Stream blocks alongside the freeform Metadata so
// the server can fold them into the HAR's _extensions.context block
// (issue #281).
type SnapshotRequest struct {
	Reason          string                 `json:"reason"`
	Source          string                 `json:"source"` // "dashboard", "rest", "player"
	Force           bool                   `json:"force"`  // bypass per-player debounce
	Metadata        map[string]interface{} `json:"metadata,omitempty"`
	Device          *SnapshotDeviceMeta    `json:"device,omitempty"`
	Stream          *SnapshotStreamMeta    `json:"stream,omitempty"`
	PlayID          string                 `json:"play_id,omitempty"`           // override the auto-detected most-recent play_id
	IncludeAllPlays bool                   `json:"include_all_plays,omitempty"` // forensic mode — no scoping
}

// SnapshotDeviceMeta is the device fingerprint a player sends with a
// snapshot request. Mirrors har.DeviceContext on the wire.
type SnapshotDeviceMeta struct {
	Model       string `json:"model,omitempty"`
	OSVersion   string `json:"os_version,omitempty"`
	AppVersion  string `json:"app_version,omitempty"`
	NetworkType string `json:"network_type,omitempty"`
}

// SnapshotStreamMeta describes what the player is playing.
type SnapshotStreamMeta struct {
	ContentID         string `json:"content_id,omitempty"`
	Protocol          string `json:"protocol,omitempty"`
	Codec             string `json:"codec,omitempty"`
	InitialVariantURL string `json:"initial_variant_url,omitempty"`
}

// snapshotDebounceWindow is the minimum gap between auto-driven captures from
// the same player. Manual (force=true) and dashboard captures bypass this.
const snapshotDebounceWindow = 30 * time.Second

var (
	snapshotLastMu sync.Mutex
	snapshotLastAt = map[string]time.Time{} // player_id -> last accepted capture
)

// snapshotShouldDebounce returns true if we should drop this capture because
// the player triggered another one within the debounce window. Dashboard /
// REST captures (source != "player") never debounce.
func snapshotShouldDebounce(playerID, source string, force bool) bool {
	if force || source != "player" || playerID == "" {
		return false
	}
	snapshotLastMu.Lock()
	defer snapshotLastMu.Unlock()
	now := time.Now()
	last, ok := snapshotLastAt[playerID]
	if ok && now.Sub(last) < snapshotDebounceWindow {
		return true
	}
	snapshotLastAt[playerID] = now
	return false
}

// recoveryChainStore tracks the ordered list of incident reasons a
// player has hit during the current play. Keyed by playerID + ":" +
// playID so a fresh play resets the chain. Issue #281.
var (
	recoveryChainMu    sync.Mutex
	recoveryChainStore = map[string][]string{}
)

func recoveryChainKey(playerID, playID string) string {
	if playerID == "" && playID == "" {
		return ""
	}
	return playerID + ":" + playID
}

// recoveryChainSnapshot returns the current chain for (playerID, playID)
// without modifying it. Used for forensic snapshots that should observe
// without polluting the chain.
func recoveryChainSnapshot(playerID, playID string) []string {
	key := recoveryChainKey(playerID, playID)
	if key == "" {
		return nil
	}
	recoveryChainMu.Lock()
	defer recoveryChainMu.Unlock()
	chain := recoveryChainStore[key]
	if len(chain) == 0 {
		return nil
	}
	out := make([]string, len(chain))
	copy(out, chain)
	return out
}

// recordRecoveryReason appends `reason` to the chain for
// (playerID, playID), capped at 32 entries to keep the HAR reasonable.
func recordRecoveryReason(playerID, playID, reason string) []string {
	key := recoveryChainKey(playerID, playID)
	if key == "" || reason == "" {
		return nil
	}
	recoveryChainMu.Lock()
	defer recoveryChainMu.Unlock()
	chain := append(recoveryChainStore[key], reason)
	const maxChain = 32
	if len(chain) > maxChain {
		chain = chain[len(chain)-maxChain:]
	}
	recoveryChainStore[key] = chain
	out := make([]string, len(chain))
	copy(out, chain)
	return out
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

// HARBuildFilter controls play_id scoping in buildHARForSession.
//
//   - PlayID == "" and IncludeAllPlays == false: filter to the
//     most-recent play_id in the ring buffer (default — matches what
//     the player asks for at incident time).
//   - PlayID != "": filter to that exact play_id.
//   - IncludeAllPlays == true: no filter — every entry across every
//     play is included (forensic mode).
type HARBuildFilter struct {
	PlayID          string
	IncludeAllPlays bool
}

// buildHARForSession reads the session's network ring buffer and returns a HAR
// document. Caller is expected to have already validated the session exists.
// `context` is optional — when non-nil, it lands at log._extensions.context.
func (a *App) buildHARForSession(session SessionData, incident *har.Incident, filter HARBuildFilter, context *har.Context) har.HAR {
	sessionID := getString(session, "session_id")

	a.networkLogsMu.RLock()
	rb, exists := a.networkLogs[sessionID]
	a.networkLogsMu.RUnlock()

	// Resolve the play_id we'll filter on, unless the caller opts out.
	resolvedPlayID := filter.PlayID
	if !filter.IncludeAllPlays && resolvedPlayID == "" && exists {
		resolvedPlayID = mostRecentPlayID(rb.GetAll())
	}

	var sources []har.Source
	var playStartedAt time.Time
	if exists {
		entries := rb.GetAll()
		sources = make([]har.Source, 0, len(entries))
		for _, e := range entries {
			if !filter.IncludeAllPlays && resolvedPlayID != "" && e.PlayID != resolvedPlayID {
				continue
			}
			if playStartedAt.IsZero() || e.Timestamp.Before(playStartedAt) {
				playStartedAt = e.Timestamp
			}
			sources = append(sources, har.Source{
				Timestamp:       e.Timestamp,
				Method:          e.Method,
				URL:             e.URL,
				RequestKind:     e.RequestKind,
				Status:          e.Status,
				BytesIn:         e.BytesIn,
				BytesOut:        e.BytesOut,
				ContentType:     e.ContentType,
				ClientWaitMs:    e.ClientWaitMs,
				TransferMs:      e.TransferMs,
				TotalMs:         e.TotalMs,
				UpstreamURL:     e.UpstreamURL,
				DNSMs:           e.DNSMs,
				ConnectMs:       e.ConnectMs,
				TLSMs:           e.TLSMs,
				TTFBMs:          e.TTFBMs,
				RequestHeaders:  toNameValueSlice(e.RequestHeaders),
				ResponseHeaders: toNameValueSlice(e.ResponseHeaders),
				QueryString:     toNameValueSlice(e.QueryString),
				Faulted:         e.Faulted,
				FaultType:       e.FaultType,
				FaultAction:     e.FaultAction,
				FaultCategory:   e.FaultCategory,
			})
		}
	}

	opts := har.BuildOptions{
		SessionID: sessionID,
		PlayerID:  getString(session, "player_id"),
		GroupID:   getString(session, "group_id"),
		Incident:  incident,
		Context:   context,
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
		// Surface play_id scope in the incident block so HAR consumers
		// know what they're looking at without inspecting individual
		// entries' URLs.
		if incident.Metadata == nil {
			incident.Metadata = map[string]interface{}{}
		}
		if resolvedPlayID != "" {
			incident.Metadata["play_id"] = resolvedPlayID
		}
		if filter.IncludeAllPlays {
			incident.Metadata["include_all_plays"] = true
		}
		if !playStartedAt.IsZero() {
			incident.Metadata["play_started_at"] = playStartedAt.UTC().Format(time.RFC3339Nano)
		}
	}

	return har.Build(sources, opts)
}

// toNameValueSlice converts a []HeaderPair (main package's HTTP capture
// type) to a []har.NameValue without sharing memory. Returns nil for an
// empty slice so the HAR builder can apply its own defaults.
func toNameValueSlice(in []HeaderPair) []har.NameValue {
	if len(in) == 0 {
		return nil
	}
	out := make([]har.NameValue, len(in))
	for i, p := range in {
		out[i] = har.NameValue{Name: p.Name, Value: p.Value}
	}
	return out
}

// mostRecentPlayID walks the ring buffer newest-first looking for the
// last play_id seen. Empty string if no entry carried one (e.g., older
// players that don't yet emit play_id query param).
func mostRecentPlayID(entries []NetworkLogEntry) string {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].PlayID != "" {
			return entries[i].PlayID
		}
	}
	return ""
}

// buildIncidentContext assembles the _extensions.context block for a
// snapshot. Pulls device + stream from the player's request body,
// scenario from the session record, timing from the network log's
// earliest entry for the current play, and the recovery chain from
// the in-memory store. Issue #281.
func (a *App) buildIncidentContext(session SessionData, req *SnapshotRequest, playerID string) *har.Context {
	if req == nil {
		return nil
	}
	ctx := &har.Context{}

	// Device + Stream — copied from request body.
	if req.Device != nil {
		ctx.Device = &har.DeviceContext{
			Model:       req.Device.Model,
			OSVersion:   req.Device.OSVersion,
			AppVersion:  req.Device.AppVersion,
			NetworkType: req.Device.NetworkType,
		}
	}
	if req.Stream != nil {
		ctx.Stream = &har.StreamContext{
			ContentID:         req.Stream.ContentID,
			Protocol:          req.Stream.Protocol,
			Codec:             req.Stream.Codec,
			InitialVariantURL: req.Stream.InitialVariantURL,
		}
	}

	// Scenario — pull a sanitised view of the session's testing
	// configuration. Avoid leaking everything in the session blob;
	// pick the keys analysts actually care about.
	scenario := &har.ScenarioContext{}
	faultKeys := []string{
		"segment_failure_type", "segment_failure_frequency", "segment_consecutive_failures",
		"segment_failure_mode", "segment_failure_units", "segment_failure_urls",
		"manifest_failure_type", "manifest_failure_frequency", "manifest_consecutive_failures",
		"manifest_failure_mode", "manifest_failure_units", "manifest_failure_urls",
		"master_manifest_failure_type", "master_manifest_failure_frequency",
		"master_manifest_consecutive_failures", "master_manifest_failure_mode",
		"transport_failure_type", "transport_consecutive_failures", "transport_failure_frequency",
		"transport_failure_mode",
	}
	for _, k := range faultKeys {
		if v, ok := session[k]; ok && v != nil && v != "" && v != "none" && v != 0 && v != 0.0 {
			if scenario.FaultSettings == nil {
				scenario.FaultSettings = map[string]interface{}{}
			}
			scenario.FaultSettings[k] = v
		}
	}
	shapeKeys := []string{
		"nftables_bandwidth_mbps", "nftables_delay_ms", "nftables_packet_loss",
		"nftables_pattern_enabled", "nftables_pattern_steps",
	}
	for _, k := range shapeKeys {
		v, ok := session[k]
		if !ok || v == nil || v == "" {
			continue
		}
		// Booleans are equal to 0 under interface comparison, so skip
		// the v != 0 guard for bool fields — `false` is a meaningful
		// "off" we want to surface for nftables_pattern_enabled.
		if _, isBool := v.(bool); !isBool {
			if v == 0 || v == 0.0 {
				continue
			}
		}
		if scenario.NftablesShape == nil {
			scenario.NftablesShape = map[string]interface{}{}
		}
		scenario.NftablesShape[k] = v
	}
	if scenario.FaultSettings != nil || scenario.NftablesShape != nil {
		ctx.Scenario = scenario
	}

	// Resolve play_id: prefer body explicit, else most-recent in ring.
	playID := strings.TrimSpace(req.PlayID)
	sessionID := getString(session, "session_id")
	a.networkLogsMu.RLock()
	rb, exists := a.networkLogs[sessionID]
	a.networkLogsMu.RUnlock()
	if playID == "" && exists {
		playID = mostRecentPlayID(rb.GetAll())
	}

	// Timing — derive play_started_at from the earliest ring-buffer
	// entry for the current play_id (preferred). Fall back to a
	// metadata-supplied "play_started_at" string if the player passed
	// one explicitly.
	if playID != "" && exists {
		var playStartedAt time.Time
		for _, e := range rb.GetAll() {
			if e.PlayID != playID {
				continue
			}
			if playStartedAt.IsZero() || e.Timestamp.Before(playStartedAt) {
				playStartedAt = e.Timestamp
			}
		}
		if !playStartedAt.IsZero() {
			ctx.Timing = &har.TimingContext{
				PlayStartedAt:   playStartedAt.UTC().Format(time.RFC3339Nano),
				IncidentOffsetS: time.Since(playStartedAt).Seconds(),
			}
		}
	}
	if ctx.Timing == nil {
		if raw, ok := req.Metadata["play_started_at"].(string); ok && raw != "" {
			if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
				ctx.Timing = &har.TimingContext{
					PlayStartedAt:   t.UTC().Format(time.RFC3339Nano),
					IncidentOffsetS: time.Since(t).Seconds(),
				}
			}
		}
	}

	// Recovery chain — append the current reason and surface the
	// resulting list. Forensic snapshots (source != "player") read
	// without recording so they don't pollute the chain.
	if req.Source == "player" {
		ctx.RecoveryChain = recordRecoveryReason(playerID, playID, req.Reason)
	} else {
		ctx.RecoveryChain = recoveryChainSnapshot(playerID, playID)
	}

	return ctx
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

	// timeline.har accepts ?play_id=X (specific play) and
	// ?include_all_plays=1 (forensic). Default: most-recent play_id.
	filter := HARBuildFilter{
		PlayID:          strings.TrimSpace(r.URL.Query().Get("play_id")),
		IncludeAllPlays: strings.EqualFold(r.URL.Query().Get("include_all_plays"), "1") || strings.EqualFold(r.URL.Query().Get("include_all_plays"), "true"),
	}
	doc := a.buildHARForSession(session, nil, filter, nil)
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

	playerID := getString(session, "player_id")
	if snapshotShouldDebounce(playerID, req.Source, req.Force) {
		w.WriteHeader(http.StatusTooManyRequests)
		writeJSON(w, map[string]interface{}{
			"status":           "debounced",
			"debounce_window":  snapshotDebounceWindow.Seconds(),
			"message":          "another snapshot was captured for this player within the debounce window",
		})
		return
	}

	incident := &har.Incident{
		Reason:    req.Reason,
		Source:    req.Source,
		SessionID: sessionID,
		Timestamp: time.Now().UTC(),
		Metadata:  req.Metadata,
	}

	context := a.buildIncidentContext(session, &req, playerID)
	doc := a.buildHARForSession(session, incident, HARBuildFilter{
		PlayID:          strings.TrimSpace(req.PlayID),
		IncludeAllPlays: req.IncludeAllPlays,
	}, context)
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
	// Strict whitelist match: incident files are always saved as
	// `{YYYY-MM-DD}/{safe-chars}.har`. Anything else is rejected before
	// the path ever reaches filepath.Join. This is the sanitisation
	// barrier CodeQL's go/path-injection rule looks for.
	if !incidentPathRe.MatchString(relPath) {
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

	// Filename pattern: `proxy_<player_id>_<session_id>__<ts>.har`. The
	// `proxy_` prefix signals "go-proxy's view" — future client-side
	// HARs (issue #282) will use `client_`. The trailing __<ts> keeps
	// successive incidents from the same player/session pair from
	// overwriting each other (one play can produce multiple HARs:
	// freeze, then user_retry, then segment_stall).
	ts := now.Format("20060102T150405Z")
	filename := fmt.Sprintf("proxy_%s_%s__%s.har",
		safeFilename(playerID),
		safeFilename(sessionID),
		ts,
	)
	full := filepath.Join(dir, filename)

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return IncidentFileInfo{}, err
	}
	absFull, err := filepath.Abs(full)
	if err != nil {
		return IncidentFileInfo{}, err
	}
	if absFull != absDir && !strings.HasPrefix(absFull, absDir+string(os.PathSeparator)) {
		return IncidentFileInfo{}, fmt.Errorf("invalid incident file path")
	}

	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return IncidentFileInfo{}, err
	}
	if err := os.WriteFile(absFull, body, 0o644); err != nil {
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

	// Filename fallback: `proxy_<playerID>_<sessionID>__<ts>.har`. We
	// only use this when the JSON parse failed and the in-document
	// _extensions.session block isn't available — best effort.
	if info.SessionID == "" || info.PlayerID == "" {
		stem := strings.TrimSuffix(info.Filename, ".har")
		// Trim the optional `proxy_` / `client_` prefix.
		stem = strings.TrimPrefix(stem, "proxy_")
		stem = strings.TrimPrefix(stem, "client_")
		// Drop the `__<ts>` suffix.
		if idx := strings.LastIndex(stem, "__"); idx >= 0 {
			stem = stem[:idx]
		}
		// Remaining: `<playerID>_<sessionID>`. Split on the LAST `_`:
		// everything before is playerID, everything after is sessionID.
		// Correct as long as sessionID has no `_` (it's UUID-shaped in
		// practice, `[a-z0-9-]+`); playerID may contain `_`.
		if idx := strings.LastIndex(stem, "_"); idx >= 0 {
			if info.PlayerID == "" {
				info.PlayerID = stem[:idx]
			}
			if info.SessionID == "" {
				info.SessionID = stem[idx+1:]
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
