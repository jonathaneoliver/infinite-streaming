package api

import (
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultIdleTimeoutSeconds = 60
	defaultPlayerWindowSecs   = 60
	maxExternalIPList         = 20
)

type StreamEntry struct {
	Content       string
	Mode          string
	ProcessID     string
	StartedAt     time.Time
	LastRequest   time.Time
	LastRequestURI string
	Clients       map[string]time.Time
	TotalRequests int
}

type StreamStatus struct {
	Content         string `json:"content"`
	Mode            string `json:"mode"`
	ProcessID       string `json:"process_id,omitempty"`
	StartedAt       string `json:"started_at,omitempty"`
	LastRequestedAt string `json:"last_requested_at,omitempty"`
	LastRequestURI  string `json:"last_request_uri,omitempty"`
	LastRequestAgo  string `json:"last_request_ago,omitempty"`
	WillShutdownIn  string `json:"will_shutdown_in,omitempty"`
	Players         int    `json:"players"`
	TotalRequests   int    `json:"total_requests"`
	LastTick        float64 `json:"last_tick,omitempty"`
	Avg5m           float64 `json:"avg_5m,omitempty"`
	UniqueClientIPs int      `json:"unique_client_ips,omitempty"`
	ExternalIPCount int      `json:"external_ip_count,omitempty"`
	ExternalIPs     []string `json:"external_ips,omitempty"`
	ExternalIPOverflow int   `json:"external_ip_overflow,omitempty"`
}

type StreamTracker struct {
	mu           sync.RWMutex
	streams      map[string]*StreamEntry
	idleTimeout  time.Duration
	playerWindow time.Duration
}

func NewStreamTracker() *StreamTracker {
	return &StreamTracker{
		streams:      make(map[string]*StreamEntry),
		idleTimeout:  envSeconds("GO_LIVE_IDLE_TIMEOUT", defaultIdleTimeoutSeconds),
		playerWindow: envSeconds("GO_LIVE_PLAYER_WINDOW", defaultPlayerWindowSecs),
	}
}

func (t *StreamTracker) IdleTimeout() time.Duration {
	return t.idleTimeout
}

func (t *StreamTracker) Start(content, mode, processID string, startedAt time.Time) {
	key := streamKey(content, mode)
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.streams[key]
	if !ok {
		entry = &StreamEntry{
			Content:   content,
			Mode:      mode,
			Clients:   make(map[string]time.Time),
			StartedAt: startedAt,
		}
		t.streams[key] = entry
	}
	if entry.StartedAt.IsZero() {
		entry.StartedAt = startedAt
	}
	entry.ProcessID = processID
}

func (t *StreamTracker) RecordRequest(content, mode, uri, clientKey string, now time.Time) {
	key := streamKey(content, mode)
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.streams[key]
	if !ok {
		entry = &StreamEntry{
			Content:   content,
			Mode:      mode,
			Clients:   make(map[string]time.Time),
			StartedAt: now,
		}
		t.streams[key] = entry
	}
	entry.LastRequest = now
	entry.LastRequestURI = uri
	entry.TotalRequests++
	if clientKey != "" {
		entry.Clients[clientKey] = now
	}
	t.pruneClients(entry, now)
}

func (t *StreamTracker) Snapshot(now time.Time) []StreamStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()
	statuses := make([]StreamStatus, 0, len(t.streams))
	for _, entry := range t.streams {
		players := t.countActiveClients(entry, now)
		clientIPs, externalIPs := t.collectActiveClientIPs(entry, now)
		lastAgo := ""
		willShutdown := ""
		if !entry.LastRequest.IsZero() {
			idle := now.Sub(entry.LastRequest)
			lastAgo = formatSeconds(idle)
			remaining := t.idleTimeout - idle
			if remaining < 0 {
				remaining = 0
			}
			willShutdown = formatSeconds(remaining)
		}
		externalList, overflow := externalIPList(externalIPs)
		status := StreamStatus{
			Content:       entry.Content,
			Mode:          entry.Mode,
			ProcessID:     entry.ProcessID,
			Players:       players,
			TotalRequests: entry.TotalRequests,
			LastRequestAgo:  lastAgo,
			WillShutdownIn:  willShutdown,
			UniqueClientIPs: len(clientIPs),
			ExternalIPCount: len(externalIPs),
			ExternalIPs:     externalList,
			ExternalIPOverflow: overflow,
		}
		if !entry.StartedAt.IsZero() {
			status.StartedAt = entry.StartedAt.UTC().Format(time.RFC3339)
		}
		if !entry.LastRequest.IsZero() {
			status.LastRequestedAt = entry.LastRequest.UTC().Format(time.RFC3339)
			status.LastRequestURI = entry.LastRequestURI
		}
		statuses = append(statuses, status)
	}
	return statuses
}

func (t *StreamTracker) IdleEntries(now time.Time) []StreamEntry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var idle []StreamEntry
	for _, entry := range t.streams {
		if entry.ProcessID == "" || entry.LastRequest.IsZero() {
			continue
		}
		if now.Sub(entry.LastRequest) >= t.idleTimeout {
			idle = append(idle, *entry)
		}
	}
	return idle
}

func (t *StreamTracker) IdleContentEntries(now time.Time) []StreamEntry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	type agg struct {
		last   time.Time
		entry  *StreamEntry
	}
	byContent := make(map[string]*agg)
	for _, entry := range t.streams {
		if entry.ProcessID == "" || entry.LastRequest.IsZero() {
			continue
		}
		if strings.HasPrefix(entry.Mode, "dash-") {
			continue
		}
		current := byContent[entry.Content]
		if current == nil || entry.LastRequest.After(current.last) {
			byContent[entry.Content] = &agg{
				last:  entry.LastRequest,
				entry: entry,
			}
		}
	}
	var idle []StreamEntry
	for _, item := range byContent {
		if item == nil || item.entry == nil {
			continue
		}
		if now.Sub(item.last) >= t.idleTimeout {
			idle = append(idle, StreamEntry{
				Content:     item.entry.Content,
				Mode:        item.entry.Mode,
				ProcessID:   item.entry.ProcessID,
				LastRequest: item.last,
			})
		}
	}
	return idle
}

func (t *StreamTracker) Remove(content, mode string) {
	key := streamKey(content, mode)
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.streams, key)
}

func (t *StreamTracker) RemoveContentModePrefix(content, prefix string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key, entry := range t.streams {
		if entry == nil {
			continue
		}
		if entry.Content != content {
			continue
		}
		if strings.HasPrefix(entry.Mode, prefix) {
			delete(t.streams, key)
		}
	}
}

func (t *StreamTracker) pruneClients(entry *StreamEntry, now time.Time) {
	if entry == nil {
		return
	}
	cutoff := now.Add(-t.playerWindow)
	for key, last := range entry.Clients {
		if last.Before(cutoff) {
			delete(entry.Clients, key)
		}
	}
}

func (t *StreamTracker) countActiveClients(entry *StreamEntry, now time.Time) int {
	if entry == nil {
		return 0
	}
	cutoff := now.Add(-t.playerWindow)
	count := 0
	for _, last := range entry.Clients {
		if last.After(cutoff) {
			count++
		}
	}
	return count
}

func (t *StreamTracker) collectActiveClientIPs(entry *StreamEntry, now time.Time) (map[string]time.Time, map[string]time.Time) {
	clientIPs := make(map[string]time.Time)
	externalIPs := make(map[string]time.Time)
	if entry == nil {
		return clientIPs, externalIPs
	}
	cutoff := now.Add(-t.playerWindow)
	for key, last := range entry.Clients {
		if last.Before(cutoff) {
			continue
		}
		ip := parseClientIP(key)
		if ip == "" || ip == "unknown" {
			continue
		}
		clientIPs[ip] = last
		if isExternalIP(ip) {
			externalIPs[ip] = last
		}
	}
	return clientIPs, externalIPs
}

func (t *StreamTracker) UniqueExternalIPCount(now time.Time) int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	unique := make(map[string]struct{})
	for _, entry := range t.streams {
		if entry == nil {
			continue
		}
		cutoff := now.Add(-t.playerWindow)
		for key, last := range entry.Clients {
			if last.Before(cutoff) {
				continue
			}
			ip := parseClientIP(key)
			if ip == "" || ip == "unknown" {
				continue
			}
			if isExternalIP(ip) {
				unique[ip] = struct{}{}
			}
		}
	}
	return len(unique)
}

func streamKey(content, mode string) string {
	return content + "|" + mode
}

func clientKey(r *httpRequest) string {
	if r == nil {
		return ""
	}
	ip := r.RemoteIP
	if ip == "" {
		ip = "unknown"
	}
	ua := r.UserAgent
	if ua == "" {
		ua = "unknown"
	}
	return ip + "|" + ua
}

func parseClientIP(key string) string {
	parts := strings.SplitN(key, "|", 2)
	if len(parts) == 0 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

func isExternalIP(ip string) bool {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return false
	}
	if parsed.IsLoopback() || parsed.IsUnspecified() || parsed.IsLinkLocalUnicast() || parsed.IsLinkLocalMulticast() {
		return false
	}
	if parsed.IsPrivate() {
		return false
	}
	return true
}

type httpRequest struct {
	RemoteIP  string
	UserAgent string
}

func requestMeta(remoteAddr, xff, userAgent string) httpRequest {
	clientIP := ""
	if xff != "" {
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			clientIP = strings.TrimSpace(parts[0])
		}
	}
	if clientIP == "" {
		host, _, err := net.SplitHostPort(remoteAddr)
		if err == nil {
			clientIP = host
		} else {
			clientIP = remoteAddr
		}
	}
	return httpRequest{
		RemoteIP:  clientIP,
		UserAgent: userAgent,
	}
}

func envSeconds(name string, fallback int) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return time.Duration(fallback) * time.Second
	}
	secs, err := strconv.Atoi(value)
	if err != nil || secs <= 0 {
		return time.Duration(fallback) * time.Second
	}
	return time.Duration(secs) * time.Second
}

func formatSeconds(d time.Duration) string {
	secs := d.Seconds()
	if secs < 0 {
		secs = 0
	}
	return strconv.FormatFloat(secs, 'f', 1, 64) + "s"
}

func externalIPList(ipTimes map[string]time.Time) ([]string, int) {
	if len(ipTimes) == 0 {
		return nil, 0
	}
	keys := make([]string, 0, len(ipTimes))
	for key := range ipTimes {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return ipTimes[keys[i]].After(ipTimes[keys[j]])
	})
	overflow := 0
	if len(keys) > maxExternalIPList {
		overflow = len(keys) - maxExternalIPList
		keys = keys[:maxExternalIPList]
	}
	return keys, overflow
}
