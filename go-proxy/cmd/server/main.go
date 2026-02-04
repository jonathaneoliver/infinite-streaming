package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/gorilla/mux"
	"github.com/grafov/m3u8"
)

//go:embed templates/index.html
var indexHTML string

type SessionData map[string]interface{}

type App struct {
	memcache     *memcache.Client
	traffic      *TcTrafficManager
	upstreamHost string
	upstreamPort string
	maxSessions  int
	client       *http.Client
	shapeMu      sync.Mutex
	shapeLoops   map[int]context.CancelFunc
	shapeStates  map[int]NftShapePattern
}

type PlaylistInfo struct {
	URL        string `json:"url"`
	Bandwidth  int    `json:"bandwidth"`
	Resolution string `json:"resolution"`
}

type TcTrafficManager struct {
	interfaceName string
}

type NftShapeStep struct {
	RateMbps        float64 `json:"rate_mbps"`
	DurationSeconds float64 `json:"duration_seconds"`
	Enabled         bool    `json:"enabled"`
}

type NftShapePattern struct {
	Steps          []NftShapeStep `json:"steps"`
	ActiveStep     int            `json:"active_step"`
	ActiveRateMbps float64        `json:"active_rate_mbps"`
	ActiveAt       string         `json:"active_at"`
}

func (s *NftShapeStep) UnmarshalJSON(data []byte) error {
	type alias struct {
		RateMbps        float64 `json:"rate_mbps"`
		DurationSeconds float64 `json:"duration_seconds"`
		Enabled         *bool   `json:"enabled"`
	}
	var raw alias
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.RateMbps = raw.RateMbps
	s.DurationSeconds = raw.DurationSeconds
	s.Enabled = true
	if raw.Enabled != nil {
		s.Enabled = *raw.Enabled
	}
	return nil
}

func NewTcTrafficManager(interfaceName string) *TcTrafficManager {
	return &TcTrafficManager{interfaceName: interfaceName}
}

func (t *TcTrafficManager) IsActive() bool {
	cmd := exec.Command("tc", "qdisc", "show", "dev", t.interfaceName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), "htb")
}

func (t *TcTrafficManager) EnsureRootQdisc() error {
	show := exec.Command("tc", "qdisc", "show", "dev", t.interfaceName)
	if out, err := show.CombinedOutput(); err == nil {
		if strings.Contains(string(out), "qdisc htb 1:") || strings.Contains(string(out), "root htb") {
			return nil
		}
	}
	_ = exec.Command("tc", "qdisc", "del", "dev", t.interfaceName, "root").Run()
	cmd := exec.Command("tc", "qdisc", "add", "dev", t.interfaceName, "root", "handle", "1:", "htb", "default", "999")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tc qdisc add failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (t *TcTrafficManager) EnsureRootClass() error {
	cmd := exec.Command("tc", "class", "show", "dev", t.interfaceName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return err
	}
	if strings.Contains(string(output), "class htb 1:1") {
		return nil
	}
	addCmd := exec.Command(
		"tc", "class", "add", "dev", t.interfaceName, "parent", "1:",
		"classid", "1:1", "htb", "rate", "10000mbit", "ceil", "10000mbit",
	)
	if out, err := addCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tc root class add failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (t *TcTrafficManager) GetPortConfig(port int) (map[string]interface{}, error) {
	cmd := exec.Command("tc", "class", "show", "dev", t.interfaceName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}
	config := map[string]interface{}{
		"port":            port,
		"bandwidth_limit": nil,
		"bandwidth_mbps":  nil,
		"packet_loss":     nil,
		"delay_ms":        nil,
	}
	portSuffix := fmt.Sprintf("%03d", port%1000)
	classid := fmt.Sprintf("1:%s", portSuffix)
	pattern := fmt.Sprintf("class htb %s", classid)
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, pattern) && strings.Contains(line, "rate") {
			fields := strings.Fields(line)
			for i := 0; i < len(fields)-1; i++ {
				if fields[i] == "rate" {
					rateStr := fields[i+1]
					config["bandwidth_limit"] = rateStr
					config["bandwidth_mbps"] = rateToMbps(rateStr)
					break
				}
			}
		}
	}
	if netem, err := t.GetNetemConfig(port); err == nil && netem != nil {
		if val, ok := netem["packet_loss"]; ok {
			config["packet_loss"] = val
		}
		if val, ok := netem["delay_ms"]; ok {
			config["delay_ms"] = val
		}
	}
	return config, nil
}

func (t *TcTrafficManager) UpdateRateLimit(port int, rateMbps float64) error {
	if err := t.EnsureRootQdisc(); err != nil {
		return err
	}
	if err := t.EnsureRootClass(); err != nil {
		return err
	}
	if rateMbps <= 0 {
		log.Printf(
			"NETSHAPE throughput_set ts=%s port=%d rate_mbps=0 action=clear",
			time.Now().UTC().Format(time.RFC3339Nano),
			port,
		)
		_ = t.UpdateNetem(port, 0, 0)
		_ = t.RemoveFilter(port)
		_ = t.RemoveClass(port)
		return nil
	}
	portSuffix := fmt.Sprintf("%03d", port%1000)
	classid := fmt.Sprintf("1:%s", portSuffix)
	changeCmd := exec.Command(
		"tc", "class", "change", "dev", t.interfaceName, "parent", "1:1",
		"classid", classid, "htb", "rate", fmt.Sprintf("%gmbit", rateMbps), "ceil", fmt.Sprintf("%gmbit", rateMbps),
	)
	log.Printf(
		"NETSHAPE throughput_set ts=%s port=%d rate_mbps=%.3f action=apply classid=%s iface=%s",
		time.Now().UTC().Format(time.RFC3339Nano),
		port,
		rateMbps,
		classid,
		t.interfaceName,
	)
	if out, err := changeCmd.CombinedOutput(); err != nil {
		log.Printf("NETSHAPE tc class change failed port=%d: %s", port, strings.TrimSpace(string(out)))
		addCmd := exec.Command(
			"tc", "class", "add", "dev", t.interfaceName, "parent", "1:1",
			"classid", classid, "htb", "rate", fmt.Sprintf("%gmbit", rateMbps), "ceil", fmt.Sprintf("%gmbit", rateMbps),
		)
		if outAdd, errAdd := addCmd.CombinedOutput(); errAdd != nil {
			return fmt.Errorf("tc class change failed: %s; add failed: %s", strings.TrimSpace(string(out)), strings.TrimSpace(string(outAdd)))
		}
	}

	showFilters := exec.Command("tc", "filter", "show", "dev", t.interfaceName)
	filterOut, _ := showFilters.CombinedOutput()
	if !strings.Contains(string(filterOut), fmt.Sprintf("flowid %s", classid)) {
		filterCmd := exec.Command(
			"tc", "filter", "add", "dev", t.interfaceName, "protocol", "ip", "parent", "1:0", "prio", "1", "u32",
			"match", "ip", "sport", fmt.Sprintf("%d", port), "0xffff", "flowid", classid,
		)
		if out, err := filterCmd.CombinedOutput(); err != nil {
			log.Printf("NETSHAPE tc filter add (sport) failed port=%d: %s", port, strings.TrimSpace(string(out)))
			fallbackCmd := exec.Command(
				"tc", "filter", "add", "dev", t.interfaceName, "protocol", "ip", "parent", "1:0", "prio", "1", "u32",
				"match", "ip", "dport", fmt.Sprintf("%d", port), "0xffff", "flowid", classid,
			)
			if out2, err2 := fallbackCmd.CombinedOutput(); err2 != nil {
				return fmt.Errorf("tc filter add failed: %s; fallback failed: %s", strings.TrimSpace(string(out)), strings.TrimSpace(string(out2)))
			}
		}
	}
	verifyCmd := exec.Command("tc", "class", "show", "dev", t.interfaceName)
	verifyOut, _ := verifyCmd.CombinedOutput()
	afterFilterCmd := exec.Command("tc", "filter", "show", "dev", t.interfaceName)
	afterFilterOut, _ := afterFilterCmd.CombinedOutput()
	log.Printf("NETSHAPE tc class show dev %s: %s", t.interfaceName, strings.TrimSpace(string(verifyOut)))
	log.Printf("NETSHAPE tc filter show dev %s: %s", t.interfaceName, strings.TrimSpace(string(afterFilterOut)))
	if !strings.Contains(string(verifyOut), fmt.Sprintf("class htb %s", classid)) {
		return fmt.Errorf("tc class not present after update: %s", strings.TrimSpace(string(verifyOut)))
	}
	return nil
}

func (t *TcTrafficManager) RemoveClass(port int) error {
	portSuffix := fmt.Sprintf("%03d", port%1000)
	classid := fmt.Sprintf("1:%s", portSuffix)
	_ = exec.Command("tc", "class", "del", "dev", t.interfaceName, "classid", classid).Run()
	return nil
}

func (t *TcTrafficManager) RemoveFilter(port int) error {
	cmd := exec.Command(
		"tc", "filter", "del", "dev", t.interfaceName, "protocol", "ip", "parent", "1:0", "prio", "1", "u32",
		"match", "ip", "sport", fmt.Sprintf("%d", port), "0xffff",
	)
	_ = cmd.Run()
	return nil
}

func (t *TcTrafficManager) EnsureClass(port int, rateMbps float64) error {
	cmd := exec.Command("tc", "class", "show", "dev", t.interfaceName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return err
	}
	portSuffix := fmt.Sprintf("%03d", port%1000)
	classid := fmt.Sprintf("1:%s", portSuffix)
	if strings.Contains(string(output), classid) {
		return nil
	}
	if err := t.EnsureRootClass(); err != nil {
		return err
	}
	return t.UpdateRateLimit(port, rateMbps)
}

func (t *TcTrafficManager) UpdateNetem(port int, delayMs int, lossPct float64) error {
	if err := t.EnsureRootQdisc(); err != nil {
		return err
	}
	if err := t.EnsureRootClass(); err != nil {
		return err
	}
	if delayMs > 0 || lossPct > 0 {
		if err := t.EnsureClass(port, 10000); err != nil {
			return err
		}
	}
	portSuffix := fmt.Sprintf("%03d", port%1000)
	classid := fmt.Sprintf("1:%s", portSuffix)
	handle := fmt.Sprintf("%s0:", portSuffix)
	if delayMs <= 0 && lossPct <= 0 {
		_ = exec.Command("tc", "qdisc", "del", "dev", t.interfaceName, "parent", classid, "handle", handle, "netem").Run()
		return nil
	}
	jitter := delayMs / 2
	args := []string{"qdisc", "replace", "dev", t.interfaceName, "parent", classid, "handle", handle, "netem"}
	if delayMs > 0 {
		if jitter > 0 {
			args = append(args, "delay", fmt.Sprintf("%dms", delayMs), fmt.Sprintf("%dms", jitter), "distribution", "normal")
		} else {
			args = append(args, "delay", fmt.Sprintf("%dms", delayMs))
		}
	}
	if lossPct > 0 {
		args = append(args, "loss", fmt.Sprintf("%.2f%%", lossPct))
	}
	cmd := exec.Command("tc", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tc netem failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (t *TcTrafficManager) GetNetemConfig(port int) (map[string]interface{}, error) {
	cmd := exec.Command("tc", "qdisc", "show", "dev", t.interfaceName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}
	portSuffix := fmt.Sprintf("%03d", port%1000)
	parent := fmt.Sprintf("parent 1:%s", portSuffix)
	config := map[string]interface{}{
		"packet_loss": nil,
		"delay_ms":    nil,
	}
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, parent) && strings.Contains(line, "netem") {
			delayMs := parseNetemDelay(line)
			if delayMs > 0 {
				config["delay_ms"] = delayMs
			}
			loss := parseNetemLoss(line)
			if loss > 0 {
				config["packet_loss"] = loss
			}
			return config, nil
		}
	}
	return config, nil
}

func (t *TcTrafficManager) GetPortBytes(port int) (int64, error) {
	cmd := exec.Command("tc", "-s", "class", "show", "dev", t.interfaceName, "parent", "1:1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		cmd = exec.Command("tc", "-s", "class", "show", "dev", t.interfaceName)
		output, err = cmd.CombinedOutput()
		if err != nil {
			return 0, err
		}
	}
	portSuffix := fmt.Sprintf("%03d", port%1000)
	classid := fmt.Sprintf("1:%s", portSuffix)
	lines := strings.Split(string(output), "\n")
	found := false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.Contains(line, classid) && strings.Contains(line, "class htb") {
			found = true
			continue
		}
		if found {
			bytes := parseTcBytes(line)
			if bytes > 0 {
				return bytes, nil
			}
			if strings.HasPrefix(strings.TrimSpace(line), "class") {
				break
			}
		}
	}
	return 0, nil
}

func rateToMbps(rateStr string) interface{} {
	if strings.HasSuffix(rateStr, "Kbit") {
		value := strings.TrimSuffix(rateStr, "Kbit")
		v, _ := strconv.ParseFloat(value, 64)
		return v / 1000
	}
	if strings.HasSuffix(rateStr, "Mbit") {
		value := strings.TrimSuffix(rateStr, "Mbit")
		v, _ := strconv.ParseFloat(value, 64)
		return v
	}
	if strings.HasSuffix(rateStr, "Gbit") {
		value := strings.TrimSuffix(rateStr, "Gbit")
		v, _ := strconv.ParseFloat(value, 64)
		return v * 1000
	}
	return nil
}

func parseNetemDelay(line string) int {
	re := regexp.MustCompile(`delay ([0-9.]+)ms`)
	match := re.FindStringSubmatch(line)
	if len(match) == 2 {
		val, _ := strconv.ParseFloat(match[1], 64)
		return int(math.Round(val))
	}
	return 0
}

func parseNetemLoss(line string) float64 {
	re := regexp.MustCompile(`loss ([0-9.]+)%`)
	match := re.FindStringSubmatch(line)
	if len(match) == 2 {
		val, _ := strconv.ParseFloat(match[1], 64)
		return val
	}
	return 0
}

func parseTcBytes(line string) int64 {
	re := regexp.MustCompile(`Sent (\d+) bytes`)
	match := re.FindStringSubmatch(line)
	if len(match) == 2 {
		val, _ := strconv.ParseInt(match[1], 10, 64)
		return val
	}
	return 0
}

func main() {
	memcachedAddr := getenv("MEMCACHED_ADDR", "memcached:11211")
	upstreamHost := getenvAny([]string{"INFINITE_STREAM_UPSTREAM_HOST", "INFINITE_UPSTREAM_HOST", "BOSS_UPSTREAM_HOST"}, "go-server")
	upstreamPort := getenvAny([]string{"INFINITE_STREAM_UPSTREAM_PORT", "INFINITE_UPSTREAM_PORT", "BOSS_UPSTREAM_PORT"}, "30000")
	maxSessions := getenvIntAny([]string{"INFINITE_STREAM_MAX_SESSIONS", "INFINITE_MAX_SESSIONS", "BOSS_MAX_SESSIONS"}, 8)
	interfaceName := getenvAny([]string{"INFINITE_STREAM_TC_INTERFACE", "INFINITE_TC_INTERFACE", "TC_INTERFACE"}, "eth0")

	mc := memcache.New(memcachedAddr)
	app := &App{
		memcache:     mc,
		traffic:      NewTcTrafficManager(interfaceName),
		upstreamHost: upstreamHost,
		upstreamPort: upstreamPort,
		maxSessions:  maxSessions,
		client: &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{Timeout: 6 * time.Second}).DialContext,
				ResponseHeaderTimeout: 6 * time.Second,
			},
		},
		shapeLoops:  map[int]context.CancelFunc{},
		shapeStates: map[int]NftShapePattern{},
	}

	go app.trackPortThroughput()

	router := mux.NewRouter()
	router.Use(corsMiddleware)
	
	router.HandleFunc("/index.html", app.handleIndex).Methods(http.MethodGet)
	router.HandleFunc("/api/sessions", app.handleGetSessions).Methods(http.MethodGet)
	router.HandleFunc("/api/failure-settings/{id}", app.handleUpdateFailureSettings).Methods(http.MethodPost)
	router.HandleFunc("/api/session/{id}", app.handleSession).Methods(http.MethodGet, http.MethodDelete)
	router.HandleFunc("/api/clear-sessions", app.handleClearSessions).Methods(http.MethodPost)
	router.HandleFunc("/myshows", app.handleMyShows).Methods(http.MethodGet)
	router.HandleFunc("/debug", app.handleDebug).Methods(http.MethodGet)
	router.HandleFunc("/api/nftables/status", app.handleNftStatus).Methods(http.MethodGet)
	router.HandleFunc("/api/nftables/capabilities", app.handleNftCapabilities).Methods(http.MethodGet)
	router.HandleFunc("/api/nftables/port/{port}", app.handleNftPort).Methods(http.MethodGet)
	router.HandleFunc("/api/nftables/bandwidth/{port}", app.handleNftBandwidth).Methods(http.MethodPost)
	router.HandleFunc("/api/nftables/loss/{port}", app.handleNftLoss).Methods(http.MethodPost)
	router.HandleFunc("/api/nftables/shape/{port}", app.handleNftShape).Methods(http.MethodPost)
	router.HandleFunc("/api/nftables/pattern/{port}", app.handleNftPattern).Methods(http.MethodPost)
	router.HandleFunc("/close-socket", app.handleCloseSocket).Methods(http.MethodGet)
	router.HandleFunc("/terminate-worker", app.handleTerminateWorker).Methods(http.MethodGet)
	router.HandleFunc("/force-close", app.handleForceClose).Methods(http.MethodGet)

	router.PathPrefix("/").HandlerFunc(app.handleProxy)

	ports := []int{30081, 30181, 30281, 30381, 30481, 30581, 30681, 30781, 30881}

	errorCh := make(chan error, len(ports))
	for _, port := range ports {
		addr := fmt.Sprintf(":%d", port)
		go func(bind string) {
			log.Printf("go-proxy listening on %s", bind)
			errorCh <- http.ListenAndServe(bind, router)
		}(addr)
	}

	err := <-errorCh
	if err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With, Player-ID, X-Playback-Session-Id, X-Forwarded-Port")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	a.removeInactiveSessions()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

func (a *App) handleGetSessions(w http.ResponseWriter, r *http.Request) {
	a.removeInactiveSessions()
	sessions := a.getSessionList()
	if len(sessions) > 10 {
		sessions = sessions[:10]
	}
	for _, session := range sessions {
		port := getString(session, "x_forwarded_port")
		if port != "" && a.traffic != nil {
			if portNum, err := strconv.Atoi(port); err == nil {
				config, err := a.traffic.GetPortConfig(portNum)
				if err == nil && config != nil {
					if val, ok := config["bandwidth_limit"]; ok {
						if val != nil {
							session["nftables_bandwidth_limit"] = val
						}
					}
					if val, ok := config["packet_loss"]; ok {
						if val != nil {
							session["nftables_packet_loss"] = val
						}
					}
					if val, ok := config["bandwidth_mbps"]; ok {
						if val != nil {
							session["nftables_bandwidth_mbps"] = val
						}
					}
					if val, ok := config["delay_ms"]; ok {
						if val != nil {
							session["nftables_delay_ms"] = val
						}
					}
				}
				if pattern, ok := a.getShapePattern(portNum); ok {
					session["nftables_pattern_enabled"] = len(pattern.Steps) > 0
					session["nftables_pattern_steps"] = pattern.Steps
					if pattern.ActiveAt != "" {
						session["nftables_pattern_step"] = pattern.ActiveStep
						session["nftables_bandwidth_mbps"] = pattern.ActiveRateMbps
						session["nftables_pattern_step_runtime"] = pattern.ActiveStep
						session["nftables_pattern_rate_runtime_mbps"] = pattern.ActiveRateMbps
						session["nftables_pattern_step_runtime_at"] = pattern.ActiveAt
					}
				} else {
					session["nftables_pattern_enabled"] = false
					session["nftables_pattern_steps"] = []NftShapeStep{}
				}
				throughputKey := fmt.Sprintf("throughput_%s", port)
				if item, err := a.memcache.Get(throughputKey); err == nil {
					var throughput map[string]interface{}
					if err := json.Unmarshal(item.Value, &throughput); err == nil {
						session["measured_mbps"] = throughput["mbps"]
						session["measured_bytes"] = throughput["bytes"]
						session["measurement_window"] = throughput["window_seconds"]
					}
				}
			}
		}
	}
	writeJSON(w, sessions)
}

func (a *App) handleUpdateFailureSettings(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	var payload map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, map[string]string{"error": "invalid json"})
		return
	}
	sessions := a.getSessionList()
	for _, session := range sessions {
		if getString(session, "session_id") == id {
			for key, value := range payload {
				session[key] = value
			}
		}
	}
	a.saveSessionList(sessions)
	writeJSON(w, map[string]string{"message": "Settings updated successfully"})
}

func (a *App) handleSession(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if r.Method == http.MethodDelete {
		sessions := a.getSessionList()
		filtered := make([]SessionData, 0, len(sessions))
		removedPorts := map[int]struct{}{}
		for _, session := range sessions {
			if getString(session, "session_id") != id {
				filtered = append(filtered, session)
				continue
			}
			if port, err := strconv.Atoi(getString(session, "x_forwarded_port")); err == nil {
				removedPorts[port] = struct{}{}
			}
		}
		a.saveSessionList(filtered)
		for port := range removedPorts {
			a.disablePatternForPort(port)
		}
		writeJSON(w, map[string]string{"message": "Session deleted successfully"})
		return
	}
	if session := a.getSessionData(id); session != nil {
		writeJSON(w, session)
		return
	}
	w.WriteHeader(http.StatusNotFound)
	writeJSON(w, map[string]string{"error": "Session not found"})
}

func (a *App) handleClearSessions(w http.ResponseWriter, r *http.Request) {
	a.shapeMu.Lock()
	ports := make([]int, 0, len(a.shapeLoops))
	for port := range a.shapeLoops {
		ports = append(ports, port)
	}
	a.shapeMu.Unlock()
	for _, port := range ports {
		a.disablePatternForPort(port)
	}
	_ = a.memcache.FlushAll()
	writeJSON(w, map[string]string{"message": "All sessions cleared successfully"})
}

func (a *App) handleMyShows(w http.ResponseWriter, r *http.Request) {
	url := fmt.Sprintf("http://%s:%s/api/content", a.upstreamHost, a.upstreamPort)
	resp, err := a.client.Get(url)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": "Error fetching content from upstream server"})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": "Error fetching content from upstream server"})
		return
	}
	var items []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": "Error fetching content from upstream server"})
		return
	}
	shows := make([]map[string]string, 0)
	for _, item := range items {
		name := getString(item, "name")
		if name == "" {
			continue
		}
		if !getBool(item, "has_hls") {
			continue
		}
		descriptionParts := []string{fmt.Sprintf("Name: %s", name), fmt.Sprintf("Go-live: /go-live/%s/master.m3u8", name)}
		if segment, ok := item["segment_duration"]; ok {
			descriptionParts = append(descriptionParts, fmt.Sprintf("Segment duration: %v", segment))
		}
		if maxResolution := getString(item, "max_resolution"); maxResolution != "" {
			descriptionParts = append(descriptionParts, fmt.Sprintf("Max resolution: %s", maxResolution))
		}
		if maxHeight := getNumber(item, "max_height"); maxHeight != nil {
			descriptionParts = append(descriptionParts, fmt.Sprintf("Max height: %v", maxHeight))
		}
		shows = append(shows, map[string]string{
			"title":       fmt.Sprintf("/go-live/%s/master.m3u8", name),
			"description": strings.Join(descriptionParts, "\n"),
		})
	}
	writeJSON(w, shows)
}

func (a *App) handleDebug(w http.ResponseWriter, r *http.Request) {
	keys := make([]string, 0, len(r.Header))
	for key := range r.Header {
		keys = append(keys, key)
	}
	writeJSON(w, map[string]interface{}{
		"headers": keys,
		"method":  r.Method,
		"path":    r.URL.Path,
	})
}

func (a *App) handleNftStatus(w http.ResponseWriter, r *http.Request) {
	if runtime.GOOS != "linux" {
		writeJSON(w, map[string]string{"status": "disabled", "message": "Traffic shaping requires Linux (tc/netem)"})
		return
	}
	if a.traffic == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{"status": "disabled", "error": "Manager not initialized"})
		return
	}
	active := a.traffic.IsActive()
	if active {
		writeJSON(w, map[string]string{"status": "active", "message": "TC (traffic control) is running"})
		return
	}
	writeJSON(w, map[string]string{"status": "inactive", "message": "TC is not configured"})
}

func (a *App) handleNftCapabilities(w http.ResponseWriter, r *http.Request) {
	status := "disabled"
	reason := "traffic shaping requires Linux (tc/netem)"
	if runtime.GOOS == "linux" {
		status = "enabled"
		reason = ""
	}
	writeJSON(w, map[string]string{"status": status, "platform": runtime.GOOS, "reason": reason})
}

func (a *App) handleNftPort(w http.ResponseWriter, r *http.Request) {
	if a.traffic == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{"error": "Manager not initialized"})
		return
	}
	port, err := strconv.Atoi(mux.Vars(r)["port"])
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid port"})
		return
	}
	config, err := a.traffic.GetPortConfig(port)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if config == nil {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]string{"error": "Port not found or error reading config"})
		return
	}
	if pattern, ok := a.getShapePattern(port); ok {
		config["pattern_steps"] = pattern.Steps
		config["pattern_enabled"] = len(pattern.Steps) > 0
		config["pattern_step_runtime"] = pattern.ActiveStep
		config["pattern_rate_runtime_mbps"] = pattern.ActiveRateMbps
		config["pattern_runtime_at"] = pattern.ActiveAt
	} else {
		config["pattern_steps"] = []NftShapeStep{}
		config["pattern_enabled"] = false
	}
	writeJSON(w, config)
}

func sanitizeShapeSteps(steps []NftShapeStep) []NftShapeStep {
	out := make([]NftShapeStep, 0, len(steps))
	for _, step := range steps {
		duration := step.DurationSeconds
		if duration <= 0 {
			duration = 1
		}
		rate := step.RateMbps
		if rate < 0 {
			rate = 0
		}
		out = append(out, NftShapeStep{
			RateMbps:        rate,
			DurationSeconds: math.Round(duration*10) / 10,
			Enabled:         step.Enabled,
		})
	}
	return out
}

func (a *App) getShapePattern(port int) (NftShapePattern, bool) {
	a.shapeMu.Lock()
	defer a.shapeMu.Unlock()
	pattern, ok := a.shapeStates[port]
	if !ok {
		return NftShapePattern{}, false
	}
	copied := NftShapePattern{
		Steps:          append([]NftShapeStep(nil), pattern.Steps...),
		ActiveStep:     pattern.ActiveStep,
		ActiveRateMbps: pattern.ActiveRateMbps,
		ActiveAt:       pattern.ActiveAt,
	}
	return copied, true
}

func (a *App) setShapeRuntimeStep(port int, stepIndex int, rateMbps float64) {
	a.shapeMu.Lock()
	defer a.shapeMu.Unlock()
	pattern, ok := a.shapeStates[port]
	if !ok {
		return
	}
	pattern.ActiveStep = stepIndex
	pattern.ActiveRateMbps = rateMbps
	pattern.ActiveAt = time.Now().UTC().Format(time.RFC3339Nano)
	a.shapeStates[port] = pattern
}

func (a *App) stopShapeLoop(port int) {
	a.shapeMu.Lock()
	cancel, ok := a.shapeLoops[port]
	if ok {
		delete(a.shapeLoops, port)
	}
	delete(a.shapeStates, port)
	a.shapeMu.Unlock()
	if ok && cancel != nil {
		cancel()
	}
}

func (a *App) applyShapePattern(port int, steps []NftShapeStep, delayMs int, loss float64) error {
	if a.traffic == nil {
		return fmt.Errorf("traffic manager not initialized")
	}
	cleanSteps := sanitizeShapeSteps(steps)
	if len(cleanSteps) == 0 {
		a.stopShapeLoop(port)
		a.updateSessionsByPort(port, map[string]interface{}{
			"nftables_pattern_enabled": false,
			"nftables_pattern_steps":   []NftShapeStep{},
		})
		return nil
	}
	if err := a.traffic.UpdateNetem(port, delayMs, loss); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	var oldCancel context.CancelFunc
	a.shapeMu.Lock()
	oldCancel = a.shapeLoops[port]
	a.shapeLoops[port] = cancel
	a.shapeStates[port] = NftShapePattern{
		Steps:          append([]NftShapeStep(nil), cleanSteps...),
		ActiveStep:     0,
		ActiveRateMbps: 0,
		ActiveAt:       "",
	}
	a.shapeMu.Unlock()
	if oldCancel != nil {
		oldCancel()
	}
	a.updateSessionsByPort(port, map[string]interface{}{
		"nftables_pattern_enabled": true,
		"nftables_pattern_steps":   cleanSteps,
		"nftables_delay_ms":        delayMs,
		"nftables_packet_loss":     loss,
	})
	go a.runShapePatternLoop(ctx, port, cleanSteps, delayMs, loss)
	return nil
}

func (a *App) runShapePatternLoop(ctx context.Context, port int, steps []NftShapeStep, delayMs int, loss float64) {
	if len(steps) == 0 {
		return
	}
	hasEnabledStep := false
	for _, step := range steps {
		if step.Enabled {
			hasEnabledStep = true
			break
		}
	}
	stepIndex := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		step := steps[stepIndex]
		ts := time.Now().UTC().Format(time.RFC3339Nano)
		if !step.Enabled {
			log.Printf(
				"NETSHAPE pattern_step ts=%s port=%d step=%d/%d rate_mbps=%.3f duration_s=%.1f enabled=%t status=skipped_disabled",
				ts,
				port,
				stepIndex+1,
				len(steps),
				step.RateMbps,
				step.DurationSeconds,
				step.Enabled,
			)
			if !hasEnabledStep {
				timer := time.NewTimer(250 * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			stepIndex = (stepIndex + 1) % len(steps)
			continue
		}
		if err := a.traffic.UpdateRateLimit(port, step.RateMbps); err != nil {
			log.Printf(
				"NETSHAPE pattern_step ts=%s port=%d step=%d/%d rate_mbps=%.3f duration_s=%.1f enabled=%t status=rate_failed err=%v",
				ts,
				port,
				stepIndex+1,
				len(steps),
				step.RateMbps,
				step.DurationSeconds,
				step.Enabled,
				err,
			)
		} else {
			a.setShapeRuntimeStep(port, stepIndex+1, step.RateMbps)
			log.Printf(
				"NETSHAPE pattern_step ts=%s port=%d step=%d/%d rate_mbps=%.3f duration_s=%.1f enabled=%t status=applied",
				ts,
				port,
				stepIndex+1,
				len(steps),
				step.RateMbps,
				step.DurationSeconds,
				step.Enabled,
			)
		}
		if err := a.traffic.UpdateNetem(port, delayMs, loss); err != nil {
			log.Printf(
				"NETSHAPE pattern_netem ts=%s port=%d delay_ms=%d loss_pct=%.2f status=failed err=%v",
				ts,
				port,
				delayMs,
				loss,
				err,
			)
		}
		a.updateSessionsByPort(port, map[string]interface{}{
			"nftables_bandwidth_mbps":  step.RateMbps,
			"nftables_pattern_enabled": true,
			"nftables_pattern_steps":   steps,
			"nftables_pattern_step":    stepIndex + 1,
		})
		wait := time.Duration(step.DurationSeconds * float64(time.Second))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		stepIndex = (stepIndex + 1) % len(steps)
	}
}

func (a *App) disablePatternForPort(port int) {
	a.stopShapeLoop(port)
	a.updateSessionsByPort(port, map[string]interface{}{
		"nftables_pattern_enabled": false,
		"nftables_pattern_steps":   []NftShapeStep{},
		"nftables_pattern_step":    nil,
	})
}

func (a *App) handleNftPattern(w http.ResponseWriter, r *http.Request) {
	if a.traffic == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{"error": "Manager not initialized"})
		return
	}
	port, err := strconv.Atoi(mux.Vars(r)["port"])
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid port"})
		return
	}
	var payload struct {
		Steps                  []NftShapeStep `json:"steps"`
		DelayMs                int            `json:"delay_ms"`
		LossPct                float64        `json:"loss_pct"`
		SegmentDurationSeconds float64        `json:"segment_duration_seconds"`
		DefaultSegments        float64        `json:"default_segments"`
		DefaultStepSeconds     float64        `json:"default_step_seconds"`
		TemplateMode           string         `json:"template_mode"`
		TemplateMarginPct      float64        `json:"template_margin_pct"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid json"})
		return
	}
	switch payload.TemplateMode {
	case "sliders", "square_wave", "ramp_up", "ramp_down", "pyramid":
	default:
		payload.TemplateMode = "sliders"
	}
	switch payload.TemplateMarginPct {
	case 0, 10, 25, 50:
	default:
		payload.TemplateMarginPct = 0
	}
	if payload.DefaultStepSeconds <= 0 {
		payload.DefaultStepSeconds = payload.DefaultSegments * payload.SegmentDurationSeconds
	}
	if len(payload.Steps) == 0 {
		a.disablePatternForPort(port)
		a.updateSessionsByPort(port, map[string]interface{}{
			"nftables_pattern_segment_duration_seconds": payload.SegmentDurationSeconds,
			"nftables_pattern_default_segments":         payload.DefaultSegments,
			"nftables_pattern_default_step_seconds":     payload.DefaultStepSeconds,
			"nftables_pattern_template_mode":            payload.TemplateMode,
			"nftables_pattern_margin_pct":               payload.TemplateMarginPct,
		})
		writeJSON(w, map[string]interface{}{
			"success":         true,
			"port":            port,
			"pattern_enabled": false,
			"steps":           []NftShapeStep{},
		})
		return
	}
	cleanSteps := sanitizeShapeSteps(payload.Steps)
	if err := a.applyShapePattern(port, cleanSteps, payload.DelayMs, payload.LossPct); err != nil {
		log.Printf("NETSHAPE pattern apply failed port=%d: %v", port, err)
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": "Failed to apply pattern", "details": err.Error()})
		return
	}
	a.updateSessionsByPort(port, map[string]interface{}{
		"nftables_pattern_segment_duration_seconds": payload.SegmentDurationSeconds,
		"nftables_pattern_default_segments":         payload.DefaultSegments,
		"nftables_pattern_default_step_seconds":     payload.DefaultStepSeconds,
		"nftables_pattern_template_mode":            payload.TemplateMode,
		"nftables_pattern_margin_pct":               payload.TemplateMarginPct,
	})
	writeJSON(w, map[string]interface{}{
		"success":         true,
		"port":            port,
		"pattern_enabled": true,
		"steps":           cleanSteps,
		"delay_ms":        payload.DelayMs,
		"loss_pct":        payload.LossPct,
	})
}

func (a *App) handleNftBandwidth(w http.ResponseWriter, r *http.Request) {
	if a.traffic == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{"error": "Manager not initialized"})
		return
	}
	port, err := strconv.Atoi(mux.Vars(r)["port"])
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid port"})
		return
	}
	var payload map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid json"})
		return
	}
	rate := "10"
	if val, ok := payload["rate"]; ok {
		switch v := val.(type) {
		case string:
			rate = v
		case float64:
			rate = fmt.Sprintf("%g", v)
		case int:
			rate = fmt.Sprintf("%d", v)
		}
	}
	rate = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(rate, "mbps", ""), "mbit", ""))
	rate = strings.TrimSpace(rate)
	rateMbps, err := strconv.ParseFloat(rate, 64)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid rate"})
		return
	}
	a.disablePatternForPort(port)
	if err := a.traffic.UpdateRateLimit(port, rateMbps); err != nil {
		log.Printf("NETSHAPE rate limit failed port=%d rate=%g: %v", port, rateMbps, err)
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": "Failed to update rate limit", "details": err.Error()})
		return
	}
	a.updateSessionsByPort(port, map[string]interface{}{
		"nftables_bandwidth_mbps": rateMbps,
	})
	writeJSON(w, map[string]interface{}{"success": true, "port": port, "rate": fmt.Sprintf("%g Mbps", rateMbps)})
}

func (a *App) handleNftLoss(w http.ResponseWriter, r *http.Request) {
	if a.traffic == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{"error": "Manager not initialized"})
		return
	}
	port, err := strconv.Atoi(mux.Vars(r)["port"])
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid port"})
		return
	}
	var payload map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid json"})
		return
	}
	loss := 0.0
	if val, ok := payload["loss_pct"]; ok {
		switch v := val.(type) {
		case float64:
			loss = v
		case int:
			loss = float64(v)
		case string:
			parsed, _ := strconv.ParseFloat(v, 64)
			loss = parsed
		}
	}
	a.disablePatternForPort(port)
	if err := a.traffic.UpdateNetem(port, 0, loss); err != nil {
		log.Printf("NETSHAPE packet loss failed port=%d loss=%.2f: %v", port, loss, err)
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": "Failed to update packet loss", "details": err.Error()})
		return
	}
	a.updateSessionsByPort(port, map[string]interface{}{
		"nftables_packet_loss": loss,
	})
	writeJSON(w, map[string]interface{}{"success": true, "port": port, "loss_pct": loss})
}

func (a *App) handleNftShape(w http.ResponseWriter, r *http.Request) {
	if a.traffic == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{"error": "Manager not initialized"})
		return
	}
	port, err := strconv.Atoi(mux.Vars(r)["port"])
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid port"})
		return
	}
	var payload map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid json"})
		return
	}
	rateMbps := 0.0
	if val, ok := payload["rate_mbps"]; ok {
		switch v := val.(type) {
		case float64:
			rateMbps = v
		case int:
			rateMbps = float64(v)
		case string:
			parsed, _ := strconv.ParseFloat(v, 64)
			rateMbps = parsed
		}
	}
	delayMs := 0
	if val, ok := payload["delay_ms"]; ok {
		switch v := val.(type) {
		case float64:
			delayMs = int(v)
		case int:
			delayMs = v
		case string:
			parsed, _ := strconv.Atoi(v)
			delayMs = parsed
		}
	}
	loss := 0.0
	if val, ok := payload["loss_pct"]; ok {
		switch v := val.(type) {
		case float64:
			loss = v
		case int:
			loss = float64(v)
		case string:
			parsed, _ := strconv.ParseFloat(v, 64)
			loss = parsed
		}
	}
	a.disablePatternForPort(port)
	if err := a.traffic.UpdateRateLimit(port, rateMbps); err != nil {
		log.Printf("NETSHAPE rate limit failed port=%d rate=%g: %v", port, rateMbps, err)
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": "Failed to update rate limit", "details": err.Error()})
		return
	}
	if err := a.traffic.UpdateNetem(port, delayMs, loss); err != nil {
		log.Printf("NETSHAPE netem failed port=%d delay=%d loss=%.2f: %v", port, delayMs, loss, err)
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, map[string]string{"error": "Failed to update delay/loss", "details": err.Error()})
		return
	}
	a.updateSessionsByPort(port, map[string]interface{}{
		"nftables_bandwidth_mbps": rateMbps,
		"nftables_delay_ms":       delayMs,
		"nftables_packet_loss":    loss,
	})
	log.Printf("NETSHAPE applied port=%d rate=%g delay=%d loss=%.2f", port, rateMbps, delayMs, loss)
	writeJSON(w, map[string]interface{}{
		"success":   true,
		"port":      port,
		"rate_mbps": rateMbps,
		"delay_ms":  delayMs,
		"loss_pct":  loss,
	})
}

func (a *App) handleCloseSocket(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("Hijack not supported"))
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("Failed to close socket"))
		return
	}
	_ = conn.Close()
}

func (a *App) handleTerminateWorker(w http.ResponseWriter, r *http.Request) {
	go func() {
		time.Sleep(100 * time.Millisecond)
		os.Exit(0)
	}()
	_, _ = w.Write([]byte("Terminating worker"))
}

func (a *App) handleForceClose(w http.ResponseWriter, r *http.Request) {
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = exec.Command("kill", "-9", fmt.Sprintf("%d", os.Getpid())).Run()
		os.Exit(137)
	}()
	_, _ = w.Write([]byte("Force closing"))
}

func (a *App) handleProxy(w http.ResponseWriter, r *http.Request) {
	filename := strings.TrimPrefix(r.URL.Path, "/")
	if filename == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	externalPort := r.Header.Get("X-Forwarded-Port")
	if externalPort == "" {
		externalPort = hostPortOrDefault(r.Host, "30181")
	}
	log.Printf("Original URL: %s", r.URL.String())
	log.Printf("Original Host: %s", r.Host)
	log.Printf("X-Forwarded-Port: %s", r.Header.Get("X-Forwarded-Port"))

	sessionList := a.getSessionList()
	sessionNumber := thirdFromLastDigit(externalPort)
	playerID := r.URL.Query().Get("player_id")
	playerHeader := r.Header.Get("player_id")
	playerHeaderAlt := r.Header.Get("Player-ID")
	playbackSessionHeader := r.Header.Get("X-Playback-Session-Id")

	if playerID != "" && sessionNumber == "0" {
		if existing := findSessionByPlayerID(sessionList, playerID, playerHeader, playerHeaderAlt, playbackSessionHeader); existing != nil {
			assigned := getString(existing, "session_number")
			if assigned == "" {
				assigned = getString(existing, "session_id")
			}
			if assigned != "" {
				assignedNum, _ := strconv.Atoi(assigned)
				newPort := replaceThirdFromLastDigit(externalPort, assignedNum)
				host := hostWithoutPort(r.Host)
				newURL := fmt.Sprintf("http://%s:%s/%s", host, newPort, filename)
				if r.URL.RawQuery != "" {
					newURL = newURL + "?" + r.URL.RawQuery
				}
				log.Printf("Redirecting to existing session URL: %s %s -> %s", newURL, externalPort, newPort)
				http.Redirect(w, r, newURL, http.StatusFound)
				return
			}
		}
		if len(sessionList) >= a.maxSessions {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		allocated := allocateSessionNumber(sessionList, a.maxSessions)
		sessionData := SessionData{
			"session_number":              fmt.Sprintf("%d", allocated),
			"sid":                         fmt.Sprintf("%d", allocated),
			"session_id":                  fmt.Sprintf("%d", allocated),
			"player_id":                   playerID,
			"headers_player_id":           playerHeader,
			"headers_player-ID":           playerHeaderAlt,
			"headers_x_playback_session_id": playbackSessionHeader,
			"playlists_count":             0,
			"segments_count":              0,
			"manifests_count":             0,
			"last_request":                nowISO(),
			"first_request_time":          nowISO(),
			"segment_failure_type":        "none",
			"segment_failure_frequency":   0,
			"segment_consecutive_failures": 0,
			"segment_failure_units":       "requests",
			"manifest_failure_type":       "none",
			"manifest_failure_frequency":  0,
			"manifest_failure_units":      "requests",
			"manifest_consecutive_failures": 0,
			"playlist_failure_type":       "none",
			"playlist_failure_frequency":  0,
			"playlist_failure_units":      "requests",
			"playlist_consecutive_failures": 0,
			"current_failures":            0,
			"consecutive_failures_count":  0,
			"player_ip":                   "",
			"user_agent":                  "",
			"playlist_failure_at":         nil,
			"playlist_failure_recover_at": nil,
			"playlist_failure_urls":       []string{},
			"segment_failure_urls":        []string{},
			"segment_failure_at":          nil,
			"segment_failure_recover_at":  nil,
			"manifest_failure_at":         nil,
			"manifest_failure_recover_at": nil,
		}
		sessionList = append(sessionList, sessionData)
		a.saveSessionList(sessionList)
		newPort := replaceThirdFromLastDigit(externalPort, allocated)
		host := hostWithoutPort(r.Host)
		newURL := fmt.Sprintf("http://%s:%s/%s", host, newPort, filename)
		if r.URL.RawQuery != "" {
			newURL = newURL + "?" + r.URL.RawQuery
		}
		log.Printf("Redirecting to new URL with port: %s %s -> %s", newURL, externalPort, newPort)
		http.Redirect(w, r, newURL, http.StatusFound)
		return
	}

	if sessionNumber == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	index := -1
	for i, session := range sessionList {
		if getString(session, "session_id") == sessionNumber {
			index = i
			break
		}
	}
	if index == -1 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	sessionData := sessionList[index]
	sessionData["last_request"] = nowISO()
	sessionData["last_request_url"] = filename
	sessionData["user_agent"] = r.UserAgent()
	sessionData["player_ip"] = remoteIP(r.RemoteAddr)
	sessionData["x_forwarded_port"] = externalPort
	requestBytes := int64(0)
	if r.ContentLength > 0 {
		requestBytes = r.ContentLength
	}

	portNum, _ := strconv.Atoi(externalPort)
	if portNum > 0 {
		a.applySessionShaping(sessionData, portNum)
	}

	if _, ok := sessionData["session_start_time"]; !ok {
		sessionData["session_start_time"] = nowISO()
	}
	if startStr, ok := sessionData["session_start_time"].(string); ok {
		if startTime, err := time.Parse("2006-01-02T15:04:05.000", startStr); err == nil {
			sessionData["session_duration"] = math.Round(time.Since(startTime).Seconds()*1000) / 1000
		}
	}

	upstreamURL := fmt.Sprintf("http://%s:%s/%s", a.upstreamHost, a.upstreamPort, filename)
	contentType, isManifest, isPlaylist, isSegment, playlistInfo := a.getContentType(upstreamURL)

	if isPlaylist {
		playlistUrls := getPlaylistInfos(sessionData)
		base := pathBase(filename)
		for _, playlist := range playlistUrls {
			if strings.Contains(playlist.URL, base) {
				sessionData["last_playlist_url"] = filename
				break
			}
		}
	}
	if isManifest {
		sessionData["manifest_url"] = filename
	}
	if playlistInfo != nil {
		sessionData["playlist_urls"] = playlistInfo
	}

	handler := NewRequestHandler(isSegment, isManifest, isPlaylist, sessionData)
	failureType := handler.HandleRequest(filename)

	sessionList[index] = sessionData
	a.saveSessionList(sessionList)
	if playerID := getString(sessionData, "player_id"); playerID != "" {
		a.saveSession(playerID, sessionData)
	}

	if failureType != "none" {
		log.Printf("FAILURE! Identifier: %s, %s, %s", sessionNumber, upstreamURL, failureType)
		if failureType == "corrupted" && isSegment {
			proxyReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
			if err != nil {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			resp, err := a.client.Do(proxyReq)
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					w.WriteHeader(http.StatusGatewayTimeout)
					return
				}
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				w.WriteHeader(resp.StatusCode)
				return
			}
			if contentType != "" {
				w.Header().Set("Content-Type", contentType)
			}
			w.Header().Set("X-Session-ID", getString(sessionData, "session_number"))
			w.WriteHeader(http.StatusOK)
			writer := bufio.NewWriter(w)
			buf := make([]byte, 32*1024)
			var bytesOut int64
			for {
				n, err := resp.Body.Read(buf)
				if n > 0 {
					for i := 0; i < n; i++ {
						buf[i] = 0
					}
					_, _ = writer.Write(buf[:n])
					bytesOut += int64(n)
				}
				if err != nil {
					break
				}
			}
			_ = writer.Flush()
			updateSessionTraffic(sessionData, requestBytes, bytesOut)
			sessionList[index] = sessionData
			a.saveSessionList(sessionList)
			return
		}
		updateSessionTraffic(sessionData, requestBytes, 0)
		sessionList[index] = sessionData
		a.saveSessionList(sessionList)
		switch failureType {
		case "404":
			w.WriteHeader(http.StatusNotFound)
		case "403":
			w.WriteHeader(http.StatusForbidden)
		case "500":
			w.WriteHeader(http.StatusInternalServerError)
		case "timeout":
			w.WriteHeader(http.StatusGatewayTimeout)
		case "connection_refused":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "dns_failure":
			w.WriteHeader(http.StatusBadGateway)
		case "rate_limiting":
			w.WriteHeader(http.StatusTooManyRequests)
		case "hung":
			log.Printf("hanging response to request: %s", upstreamURL)
			time.Sleep(5 * time.Minute)
			return
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
		return
	}

	proxyReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstreamURL, nil)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	resp, err := a.client.Do(proxyReq)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		w.WriteHeader(resp.StatusCode)
		return
	}
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("X-Session-ID", getString(sessionData, "session_number"))
	w.WriteHeader(http.StatusOK)
	writer := bufio.NewWriter(w)
	bytesOut, _ := io.Copy(writer, resp.Body)
	_ = writer.Flush()
	updateSessionTraffic(sessionData, requestBytes, bytesOut)
	sessionList[index] = sessionData
	a.saveSessionList(sessionList)
}

func (a *App) applySessionShaping(session SessionData, port int) {
	if a.traffic == nil || runtime.GOOS != "linux" {
		return
	}
	if getBool(session, "nftables_pattern_enabled") || sessionHasPatternSteps(session) {
		// Pattern loop owns the rate while enabled; avoid per-request overrides.
		return
	}
	rate := getFloat(session, "nftables_bandwidth_mbps")
	delay := getInt(session, "nftables_delay_ms")
	loss := getFloat(session, "nftables_packet_loss")
	if rate == 0 && delay == 0 && loss == 0 {
		return
	}
	if err := a.traffic.UpdateRateLimit(port, rate); err != nil {
		log.Printf("NETSHAPE apply rate failed port=%d rate=%g: %v", port, rate, err)
		return
	}
	if err := a.traffic.UpdateNetem(port, delay, loss); err != nil {
		log.Printf("NETSHAPE apply netem failed port=%d delay=%d loss=%.2f: %v", port, delay, loss, err)
		return
	}
}

func (a *App) getContentType(target string) (string, bool, bool, bool, []PlaylistInfo) {
	parsed, err := url.Parse(target)
	if err != nil {
		return "", false, false, false, nil
	}
	if parsed.Hostname() != "" {
		parsed.Host = fmt.Sprintf("%s:%s", parsed.Hostname(), a.upstreamPort)
	}
	headReq, err := http.NewRequest(http.MethodHead, parsed.String(), nil)
	if err != nil {
		return "", false, false, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	headReq = headReq.WithContext(ctx)
	resp, err := a.client.Do(headReq)
	if err != nil {
		return "", false, false, false, nil
	}
	contentType := resp.Header.Get("Content-Type")
	resp.Body.Close()

	if resp.StatusCode == http.StatusMethodNotAllowed {
		contentType = ""
	}
	if strings.HasSuffix(strings.ToLower(parsed.Path), ".m3u8") && contentType == "" {
		contentType = "application/vnd.apple.mpegurl"
	}

	if strings.Contains(strings.ToLower(contentType), "mpegurl") {
		getReq, _ := http.NewRequest(http.MethodGet, parsed.String(), nil)
		ctxGet, cancelGet := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelGet()
		getReq = getReq.WithContext(ctxGet)
		getResp, err := a.client.Do(getReq)
		if err != nil {
			return contentType, false, false, false, nil
		}
		defer getResp.Body.Close()
		if getResp.StatusCode >= 400 {
			return contentType, false, false, false, nil
		}
		contentType = getResp.Header.Get("Content-Type")
		body, _ := io.ReadAll(getResp.Body)
		if len(body) > 0 {
			playlist, listType, err := m3u8.DecodeFrom(bufio.NewReader(bytes.NewReader(body)), true)
			if err == nil {
				switch listType {
				case m3u8.MASTER:
					master := playlist.(*m3u8.MasterPlaylist)
					infos := make([]PlaylistInfo, 0)
					for _, variant := range master.Variants {
						resolution := "unknown"
						if variant.Resolution != "" {
							resolution = variant.Resolution
						}
						infos = append(infos, PlaylistInfo{
							URL:        variant.URI,
							Bandwidth:  int(variant.Bandwidth),
							Resolution: resolution,
						})
					}
					return contentType, true, false, false, infos
				case m3u8.MEDIA:
					return contentType, false, true, false, nil
				}
			}
		}
		return contentType, false, true, false, nil
	}
	return contentType, false, false, true, nil
}

func (a *App) trackPortThroughput() {
	cache := map[int]struct {
		bytes     int64
		timestamp time.Time
	}{}
	for {
		sessions := a.getSessionList()
		activePorts := map[int]struct{}{}
		for _, session := range sessions {
			portStr := getString(session, "x_forwarded_port")
			if portStr == "" {
				continue
			}
			if port, err := strconv.Atoi(portStr); err == nil {
				activePorts[port] = struct{}{}
			}
		}
		if len(activePorts) == 0 {
			time.Sleep(5 * time.Second)
			continue
		}
		now := time.Now()
		if a.traffic != nil && runtime.GOOS == "linux" {
			for port := range activePorts {
				bytesValue, err := a.traffic.GetPortBytes(port)
				if err != nil || bytesValue <= 0 {
					continue
				}
				if prev, ok := cache[port]; ok {
					deltaBytes := bytesValue - prev.bytes
					deltaTime := now.Sub(prev.timestamp).Seconds()
					if deltaTime > 0 {
						mbps := (float64(deltaBytes) * 8) / (deltaTime * 1024 * 1024)
						payload := map[string]interface{}{
							"mbps":           math.Round(mbps*100) / 100,
							"bytes":          deltaBytes,
							"window_seconds": math.Round(deltaTime*10) / 10,
							"timestamp":      now.Unix(),
						}
						if bytes, err := json.Marshal(payload); err == nil {
							_ = a.memcache.Set(&memcache.Item{Key: fmt.Sprintf("throughput_%d", port), Value: bytes, Expiration: 30})
						}
					}
				}
				cache[port] = struct {
					bytes     int64
					timestamp time.Time
				}{bytes: bytesValue, timestamp: now}
			}
			time.Sleep(5 * time.Second)
			continue
		}
		cmd := exec.Command("nft", "list", "chain", "inet", "throttle", "output")
		output, err := cmd.CombinedOutput()
		if err == nil {
			bytesValue := parseNftBytes(string(output))
			if bytesValue > 0 {
				for port := range activePorts {
					if prev, ok := cache[port]; ok {
						deltaBytes := bytesValue - prev.bytes
						deltaTime := now.Sub(prev.timestamp).Seconds()
						if deltaTime > 0 {
							mbps := (float64(deltaBytes) * 8) / (deltaTime * 1024 * 1024)
							payload := map[string]interface{}{
								"mbps":           math.Round(mbps*100) / 100,
								"bytes":          deltaBytes,
								"window_seconds": math.Round(deltaTime*10) / 10,
								"timestamp":      now.Unix(),
							}
							if bytes, err := json.Marshal(payload); err == nil {
								_ = a.memcache.Set(&memcache.Item{Key: fmt.Sprintf("throughput_%d", port), Value: bytes, Expiration: 30})
							}
						}
					}
					cache[port] = struct {
						bytes     int64
						timestamp time.Time
					}{bytes: bytesValue, timestamp: now}
				}
			}
		}
		time.Sleep(5 * time.Second)
	}
}

func parseNftBytes(output string) int64 {
	re := regexp.MustCompile(`counter packets (\d+) bytes (\d+)`)
	match := re.FindStringSubmatch(output)
	if len(match) == 3 {
		val, _ := strconv.ParseInt(match[2], 10, 64)
		return val
	}
	return 0
}

func (a *App) getSessionData(identifier string) SessionData {
	if identifier == "" {
		return nil
	}
	if item, err := a.memcache.Get(identifier); err == nil {
		var session SessionData
		if err := json.Unmarshal(item.Value, &session); err == nil {
			return session
		}
	}
	sessions := a.getSessionList()
	for _, session := range sessions {
		if getString(session, "session_id") == identifier {
			return session
		}
	}
	return nil
}

func (a *App) updateSessionsByPort(port int, updates map[string]interface{}) {
	sessions := a.getSessionList()
	changed := false
	for _, session := range sessions {
		portStr := getString(session, "x_forwarded_port")
		if portStr == "" {
			continue
		}
		if portNum, err := strconv.Atoi(portStr); err == nil && portNum == port {
			for key, value := range updates {
				session[key] = value
			}
			changed = true
		}
	}
	if changed {
		a.saveSessionList(sessions)
	}
}

func (a *App) getSessionList() []SessionData {
	item, err := a.memcache.Get("session_list")
	if err != nil {
		return []SessionData{}
	}
	var sessions []SessionData
	if err := json.Unmarshal(item.Value, &sessions); err != nil {
		return []SessionData{}
	}
	return sessions
}

func (a *App) saveSessionList(sessions []SessionData) {
	if data, err := json.Marshal(sessions); err == nil {
		_ = a.memcache.Set(&memcache.Item{Key: "session_list", Value: data})
	}
}

func (a *App) saveSession(identifier string, session SessionData) {
	if identifier == "" {
		return
	}
	if data, err := json.Marshal(session); err == nil {
		_ = a.memcache.Set(&memcache.Item{Key: identifier, Value: data})
	}
}

func (a *App) removeInactiveSessions() {
	sessions := a.getSessionList()
	if len(sessions) == 0 {
		return
	}
	active := make([]SessionData, 0, len(sessions))
	now := time.Now()
	for _, session := range sessions {
		lastRequest := getString(session, "last_request")
		if lastRequest == "" {
			continue
		}
		lastTime, err := time.Parse("2006-01-02T15:04:05.000", lastRequest)
		if err != nil {
			continue
		}
		if now.Sub(lastTime) < 60*time.Second {
			active = append(active, session)
		} else {
			_ = a.memcache.Delete(getString(session, "session_id"))
		}
	}
	a.saveSessionList(active)
}

func writeJSON(w http.ResponseWriter, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	enc := json.NewEncoder(w)
	_ = enc.Encode(payload)
}

func getString(m map[string]interface{}, key string) string {
	if val, ok := m[key]; ok {
		switch v := val.(type) {
		case string:
			return v
		case []byte:
			return string(v)
		case float64:
			if v == math.Trunc(v) {
				return fmt.Sprintf("%d", int(v))
			}
			return fmt.Sprintf("%g", v)
		case int:
			return fmt.Sprintf("%d", v)
		}
	}
	return ""
}

func getBool(m map[string]interface{}, key string) bool {
	if val, ok := m[key]; ok {
		switch v := val.(type) {
		case bool:
			return v
		case string:
			return v == "true"
		case float64:
			return v != 0
		}
	}
	return false
}

func sessionHasPatternSteps(m map[string]interface{}) bool {
	val, ok := m["nftables_pattern_steps"]
	if !ok || val == nil {
		return false
	}
	switch v := val.(type) {
	case []NftShapeStep:
		return len(v) > 0
	case []interface{}:
		return len(v) > 0
	}
	return false
}

func getNumber(m map[string]interface{}, key string) interface{} {
	if val, ok := m[key]; ok {
		switch val.(type) {
		case float64, int, int64:
			return val
		}
	}
	return nil
}

func getFloat(m map[string]interface{}, key string) float64 {
	if val, ok := m[key]; ok {
		switch v := val.(type) {
		case float64:
			return v
		case int:
			return float64(v)
		case int64:
			return float64(v)
		case string:
			f, _ := strconv.ParseFloat(v, 64)
			return f
		}
	}
	return 0
}

func getInt(m map[string]interface{}, key string) int {
	if val, ok := m[key]; ok {
		switch v := val.(type) {
		case int:
			return v
		case int64:
			return int(v)
		case float64:
			return int(v)
		case string:
			i, _ := strconv.Atoi(v)
			return i
		}
	}
	return 0
}

func getStringSlice(m map[string]interface{}, key string) []string {
	val, ok := m[key]
	if !ok || val == nil {
		return nil
	}
	if slice, ok := val.([]string); ok {
		return slice
	}
	if raw, ok := val.([]interface{}); ok {
		list := make([]string, 0, len(raw))
		for _, item := range raw {
			if s, ok := item.(string); ok {
				list = append(list, s)
			}
		}
		return list
	}
	return nil
}

func getPlaylistInfos(session SessionData) []PlaylistInfo {
	val, ok := session["playlist_urls"]
	if !ok || val == nil {
		return nil
	}
	bytes, err := json.Marshal(val)
	if err != nil {
		return nil
	}
	var infos []PlaylistInfo
	if err := json.Unmarshal(bytes, &infos); err != nil {
		return nil
	}
	return infos
}

func nowISO() string {
	return time.Now().Format("2006-01-02T15:04:05.000")
}

func thirdFromLastDigit(port string) string {
	if len(port) < 3 {
		return ""
	}
	return string(port[len(port)-3])
}

func replaceThirdFromLastDigit(port string, replacement int) string {
	if len(port) < 3 {
		return port
	}
	chars := []rune(port)
	chars[len(chars)-3] = rune('0' + replacement)
	return string(chars)
}

func allocateSessionNumber(sessions []SessionData, max int) int {
	used := map[int]bool{}
	for _, session := range sessions {
		id := getString(session, "session_id")
		if len(id) > 0 {
			last := id[len(id)-1]
			if last >= '0' && last <= '9' {
				used[int(last-'0')] = true
			}
		}
	}
	for i := 1; i <= max; i++ {
		if !used[i] {
			return i
		}
	}
	return 1
}

func findSessionByPlayerID(sessions []SessionData, ids ...string) SessionData {
	for _, session := range sessions {
		player := getString(session, "player_id")
		headerID := getString(session, "headers_player_id")
		headerAlt := getString(session, "headers_player-ID")
		playbackID := getString(session, "headers_x_playback_session_id")
		for _, id := range ids {
			if id == "" {
				continue
			}
			if player == id || headerID == id || headerAlt == id || playbackID == id {
				return session
			}
		}
	}
	return nil
}

func hostPortOrDefault(hostport, fallback string) string {
	_, port, err := net.SplitHostPort(hostport)
	if err != nil {
		if strings.Contains(hostport, ":") {
			parts := strings.Split(hostport, ":")
			return parts[len(parts)-1]
		}
		return fallback
	}
	return port
}

func hostWithoutPort(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		if strings.Contains(hostport, ":") {
			parts := strings.Split(hostport, ":")
			return parts[0]
		}
		return hostport
	}
	return host
}

func remoteIP(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

func pathBase(path string) string {
	parts := strings.Split(path, "/")
	return parts[len(parts)-1]
}

func pathParent(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2]
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getenvAny(keys []string, fallback string) string {
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func getenvIntAny(keys []string, fallback int) int {
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			parsed, err := strconv.Atoi(value)
			if err == nil {
				return parsed
			}
		}
	}
	return fallback
}

type FailureHandler struct {
	failureType       string
	failureUnits      string
	consecutiveUnits  string
	frequencyUnits    string
	failureFrequency  int
	consecutive       int
	failureAt         interface{}
	failureRecoverAt  interface{}
	resetFailureType  interface{}
}

func NewFailureHandler(prefix string, session SessionData) *FailureHandler {
	failureUnits := getString(session, prefix+"_failure_units")
	consecutiveUnits := getString(session, prefix+"_consecutive_units")
	frequencyUnits := getString(session, prefix+"_frequency_units")
	if consecutiveUnits == "" {
		consecutiveUnits = failureUnits
	}
	if frequencyUnits == "" {
		frequencyUnits = failureUnits
	}
	if consecutiveUnits == "" {
		consecutiveUnits = "requests"
	}
	if frequencyUnits == "" {
		frequencyUnits = "requests"
	}
	return &FailureHandler{
		failureType:      getString(session, prefix+"_failure_type"),
		failureUnits:     failureUnits,
		consecutiveUnits: consecutiveUnits,
		frequencyUnits:   frequencyUnits,
		failureFrequency: getInt(session, prefix+"_failure_frequency"),
		consecutive:      getInt(session, prefix+"_consecutive_failures"),
		failureAt:        session[prefix+"_failure_at"],
		failureRecoverAt: session[prefix+"_failure_recover_at"],
		resetFailureType: session[prefix+"_reset_failure_type"],
	}
}

func (h *FailureHandler) HandleFailure(count int, now time.Time) string {
	if h.failureType == "" {
		h.failureType = "none"
	}
	if h.failureType == "none" {
		return "none"
	}
	if h.frequencyUnits == "seconds" {
		h.handleFailureTime(count, now)
	} else {
		h.handleFailureCount(count, now)
	}
	return h.failureType
}

func (h *FailureHandler) handleFailureCount(count int, now time.Time) {
	if h.consecutive <= 0 {
		return
	}
	if h.failureAt == nil {
		h.failureAt = count
	}
	failureAt := intFromInterface(h.failureAt)
	if count < failureAt {
		h.failureType = "none"
		return
	}
	if h.consecutiveUnits == "seconds" {
		if h.failureRecoverAt == nil {
			h.failureRecoverAt = now.Add(time.Duration(h.consecutive) * time.Second).Format("2006-01-02T15:04:05.000")
			return
		}
		failureRecover := timeFromInterface(h.failureRecoverAt)
		if now.Before(failureRecover) {
			return
		}
		if h.failureFrequency > 0 {
			h.failureAt = count + h.failureFrequency
			h.failureType = "none"
			h.failureRecoverAt = nil
			return
		}
		h.failureType = "none"
		h.resetFailureType = "none"
		h.failureRecoverAt = nil
		h.failureAt = nil
		return
	}
	if h.failureRecoverAt == nil {
		h.failureRecoverAt = count + h.consecutive
		return
	}
	failureRecover := intFromInterface(h.failureRecoverAt)
	if count < failureRecover {
		return
	}
	if h.failureFrequency > 0 {
		h.failureAt = count + h.failureFrequency
		h.failureType = "none"
		h.failureRecoverAt = nil
		return
	}
	h.failureType = "none"
	h.resetFailureType = "none"
	h.failureRecoverAt = nil
	h.failureAt = nil
}

func (h *FailureHandler) handleFailureTime(count int, now time.Time) {
	if h.consecutive <= 0 {
		return
	}
	if h.failureAt == nil {
		h.failureAt = nowISO()
	}
	failureAt := timeFromInterface(h.failureAt)
	if now.Before(failureAt) {
		h.failureType = "none"
		return
	}
	if h.consecutiveUnits == "seconds" {
		if h.failureRecoverAt == nil {
			h.failureRecoverAt = now.Add(time.Duration(h.consecutive) * time.Second).Format("2006-01-02T15:04:05.000")
			return
		}
		failureRecover := timeFromInterface(h.failureRecoverAt)
		if now.Before(failureRecover) {
			return
		}
		if h.failureFrequency > 0 {
			h.failureAt = now.Add(time.Duration(h.failureFrequency) * time.Second).Format("2006-01-02T15:04:05.000")
			h.failureType = "none"
			h.failureRecoverAt = nil
			return
		}
		h.failureType = "none"
		h.resetFailureType = "none"
		h.failureRecoverAt = nil
		h.failureAt = nil
		return
	}
	if h.failureRecoverAt == nil {
		h.failureRecoverAt = count + h.consecutive
		return
	}
	failureRecover := intFromInterface(h.failureRecoverAt)
	if count < failureRecover {
		return
	}
	if h.failureFrequency > 0 {
		h.failureAt = now.Add(time.Duration(h.failureFrequency) * time.Second).Format("2006-01-02T15:04:05.000")
		h.failureType = "none"
		h.failureRecoverAt = nil
		return
	}
	h.failureType = "none"
	h.resetFailureType = "none"
	h.failureRecoverAt = nil
	h.failureAt = nil
}

func intFromInterface(val interface{}) int {
	switch v := val.(type) {
	case int:
		return v
	case float64:
		return int(v)
	case string:
		i, _ := strconv.Atoi(v)
		return i
	}
	return 0
}

func int64FromInterface(val interface{}) int64 {
	switch v := val.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	case string:
		i, _ := strconv.ParseInt(v, 10, 64)
		return i
	}
	return 0
}

func floatFromInterface(val interface{}) float64 {
	switch v := val.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	case json.Number:
		f, _ := v.Float64()
		return f
	}
	return 0
}

func timeFromInterface(val interface{}) time.Time {
	switch v := val.(type) {
	case string:
		parsed, _ := time.Parse("2006-01-02T15:04:05.000", v)
		return parsed
	}
	return time.Time{}
}

type RequestHandler struct {
	mode       string
	session    SessionData
	failureKey string
}

func NewRequestHandler(isSegment, isManifest, isPlaylist bool, session SessionData) *RequestHandler {
	if isSegment {
		return &RequestHandler{mode: "segment", session: session}
	}
	if isManifest {
		return &RequestHandler{mode: "manifest", session: session}
	}
	if isPlaylist {
		return &RequestHandler{mode: "playlist", session: session}
	}
	return &RequestHandler{mode: "unknown", session: session}
}

func (h *RequestHandler) HandleRequest(filename string) string {
	switch h.mode {
	case "segment":
		return h.handleSegmentFailure(filename)
	case "manifest":
		return h.handleFailure("manifest", "manifests_count")
	case "playlist":
		return h.handlePlaylistFailure(filename)
	default:
		return "none"
	}
}

func (h *RequestHandler) handleFailure(prefix, countKey string) string {
	count := getInt(h.session, countKey) + 1
	h.session[countKey] = count
	failure := NewFailureHandler(prefix, h.session)
	failureType := failure.HandleFailure(count, time.Now())
	if prefix == "segment" {
		log.Printf(
			"SEGMENT FAILURE DEBUG count=%d type=%s units=%s consecutiveUnits=%s frequencyUnits=%s freq=%d consecutive=%d failureAt=%v recoverAt=%v resetType=%v",
			count,
			failure.failureType,
			failure.failureUnits,
			failure.consecutiveUnits,
			failure.frequencyUnits,
			failure.failureFrequency,
			failure.consecutive,
			failure.failureAt,
			failure.failureRecoverAt,
			failure.resetFailureType,
		)
	}
	h.session[prefix+"_failure_at"] = failure.failureAt
	h.session[prefix+"_failure_recover_at"] = failure.failureRecoverAt
	if failure.resetFailureType != nil {
		h.session[prefix+"_failure_type"] = failure.resetFailureType
		h.session[prefix+"_reset_failure_type"] = nil
	}
	return failureType
}

func (h *RequestHandler) handlePlaylistFailure(filename string) string {
	h.session["playlists_count"] = getInt(h.session, "playlists_count") + 1
	playlistURLs := getStringSlice(h.session, "playlist_failure_urls")
	match := shouldApplyFailure(playlistURLs, filename, pathParent(filename))
	if !match {
		return "none"
	}
	failure := NewFailureHandler("playlist", h.session)
	failureType := failure.HandleFailure(getInt(h.session, "playlists_count"), time.Now())
	h.session["playlist_failure_at"] = failure.failureAt
	h.session["playlist_failure_recover_at"] = failure.failureRecoverAt
	if failure.resetFailureType != nil {
		h.session["playlist_failure_type"] = failure.resetFailureType
		h.session["playlist_reset_failure_type"] = nil
	}
	return failureType
}

func (h *RequestHandler) handleSegmentFailure(filename string) string {
	h.session["segments_count"] = getInt(h.session, "segments_count") + 1
	segmentURLs := getStringSlice(h.session, "segment_failure_urls")
	match := shouldApplyFailure(segmentURLs, filename, pathParent(filename))
	if !match {
		return "none"
	}
	failure := NewFailureHandler("segment", h.session)
	failureType := failure.HandleFailure(getInt(h.session, "segments_count"), time.Now())
	log.Printf(
		"SEGMENT FAILURE DEBUG count=%d type=%s units=%s consecutiveUnits=%s frequencyUnits=%s freq=%d consecutive=%d failureAt=%v recoverAt=%v resetType=%v",
		getInt(h.session, "segments_count"),
		failure.failureType,
		failure.failureUnits,
		failure.consecutiveUnits,
		failure.frequencyUnits,
		failure.failureFrequency,
		failure.consecutive,
		failure.failureAt,
		failure.failureRecoverAt,
		failure.resetFailureType,
	)
	h.session["segment_failure_at"] = failure.failureAt
	h.session["segment_failure_recover_at"] = failure.failureRecoverAt
	if failure.resetFailureType != nil {
		h.session["segment_failure_type"] = failure.resetFailureType
		h.session["segment_reset_failure_type"] = nil
	}
	return failureType
}

func shouldApplyFailure(entries []string, filename, variant string) bool {
	if len(entries) == 0 {
		return true
	}
	base := pathBase(filename)
	for _, entry := range entries {
		if entry == "" {
			continue
		}
		if entry == "All" {
			return true
		}
		if entry == variant {
			return true
		}
		if entry == base {
			return true
		}
		if strings.Contains(filename, entry) {
			return true
		}
	}
	return false
}

func updateSessionTraffic(session SessionData, bytesIn, bytesOut int64) {
	now := time.Now()
	totalIn := int64FromInterface(session["bytes_in_total"]) + bytesIn
	totalOut := int64FromInterface(session["bytes_out_total"]) + bytesOut
	lastIn := int64FromInterface(session["bytes_in_last"])
	lastOut := int64FromInterface(session["bytes_out_last"])
	lastTs := int64FromInterface(session["bytes_last_ts"])
	if lastTs > 0 {
		deltaTime := now.Sub(time.Unix(lastTs, 0)).Seconds()
		if deltaTime > 0 {
			mbpsIn := (float64(totalIn-lastIn) * 8) / (deltaTime * 1024 * 1024)
			mbpsOut := (float64(totalOut-lastOut) * 8) / (deltaTime * 1024 * 1024)
			session["mbps_in"] = math.Round(mbpsIn*100) / 100
			session["mbps_out"] = math.Round(mbpsOut*100) / 100
			session["measurement_window_io"] = math.Round(deltaTime*10) / 10
		}
	}
	session["bytes_in_total"] = totalIn
	session["bytes_out_total"] = totalOut
	session["bytes_in_last"] = totalIn
	session["bytes_out_last"] = totalOut
	session["bytes_last_ts"] = now.Unix()
	updateSessionTrafficAverages(session, totalIn, totalOut, now)
	log.Printf("SESSIONNET bytes_in=%d bytes_out=%d mbps_in=%v mbps_out=%v window=%v",
		bytesIn,
		bytesOut,
		session["mbps_in"],
		session["mbps_out"],
		session["measurement_window_io"],
	)
}

func updateSessionTrafficAverages(session SessionData, totalIn, totalOut int64, now time.Time) {
	const windowSeconds = 18
	const shortWindowSeconds = 1
	cutoff := now.Add(-time.Duration(windowSeconds) * time.Second).Unix()
	shortCutoff := now.Add(-time.Duration(shortWindowSeconds) * time.Second).Unix()
	samples := make([]map[string]interface{}, 0)
	if raw, ok := session["io_samples"]; ok && raw != nil {
		switch v := raw.(type) {
		case []map[string]interface{}:
			samples = v
		case []interface{}:
			for _, item := range v {
				if m, ok := item.(map[string]interface{}); ok {
					samples = append(samples, m)
				}
			}
		}
	}
	var prevSample map[string]interface{}
	if len(samples) > 0 {
		prevSample = samples[len(samples)-1]
	}
	samples = append(samples, map[string]interface{}{
		"ts":  now.Unix(),
		"in":  totalIn,
		"out": totalOut,
	})
	activeSamples := make([]map[string]interface{}, 0)
	if raw, ok := session["active_io_samples"]; ok && raw != nil {
		switch v := raw.(type) {
		case []map[string]interface{}:
			activeSamples = v
		case []interface{}:
			for _, item := range v {
				if m, ok := item.(map[string]interface{}); ok {
					activeSamples = append(activeSamples, m)
				}
			}
		}
	}
	if prevSample != nil {
		prevTs := int64FromInterface(prevSample["ts"])
		nowTs := now.Unix()
		if nowTs > prevTs {
			deltaTime := float64(nowTs - prevTs)
			deltaIn := totalIn - int64FromInterface(prevSample["in"])
			deltaOut := totalOut - int64FromInterface(prevSample["out"])
			if deltaIn > 0 || deltaOut > 0 {
				activeSamples = append(activeSamples, map[string]interface{}{
					"ts":  nowTs,
					"dt":  deltaTime,
					"in":  deltaIn,
					"out": deltaOut,
				})
			}
		}
	}
	filtered := make([]map[string]interface{}, 0, len(samples))
	filteredShort := make([]map[string]interface{}, 0, len(samples))
	for _, sample := range samples {
		ts := int64FromInterface(sample["ts"])
		if ts >= cutoff {
			filtered = append(filtered, sample)
		}
		if ts >= shortCutoff {
			filteredShort = append(filteredShort, sample)
		}
	}
	if len(filtered) > 120 {
		filtered = filtered[len(filtered)-120:]
	}
	session["io_samples"] = filtered

	filteredActive := make([]map[string]interface{}, 0, len(activeSamples))
	for _, sample := range activeSamples {
		ts := int64FromInterface(sample["ts"])
		if ts >= cutoff {
			filteredActive = append(filteredActive, sample)
		}
	}
	if len(filteredActive) > 120 {
		filteredActive = filteredActive[len(filteredActive)-120:]
	}
	session["active_io_samples"] = filteredActive

	if len(filtered) >= 2 {
		oldest := filtered[0]
		newest := filtered[len(filtered)-1]
		oldTs := int64FromInterface(oldest["ts"])
		newTs := int64FromInterface(newest["ts"])
		if newTs > oldTs {
			deltaTime := float64(newTs - oldTs)
			deltaIn := int64FromInterface(newest["in"]) - int64FromInterface(oldest["in"])
			deltaOut := int64FromInterface(newest["out"]) - int64FromInterface(oldest["out"])
			session["measurement_window_io"] = math.Round(deltaTime*10) / 10
			if deltaTime >= 12 {
				mbpsInAvg := (float64(deltaIn) * 8) / (deltaTime * 1024 * 1024)
				mbpsOutAvg := (float64(deltaOut) * 8) / (deltaTime * 1024 * 1024)
				session["mbps_in_avg"] = math.Round(mbpsInAvg*100) / 100
				session["mbps_out_avg"] = math.Round(mbpsOutAvg*100) / 100
			} else {
				session["mbps_in_avg"] = nil
				session["mbps_out_avg"] = nil
			}
		}
	}

	if len(filteredShort) >= 2 {
		oldest := filteredShort[0]
		newest := filteredShort[len(filteredShort)-1]
		oldTs := int64FromInterface(oldest["ts"])
		newTs := int64FromInterface(newest["ts"])
		if newTs > oldTs {
			deltaTime := float64(newTs - oldTs)
			deltaIn := int64FromInterface(newest["in"]) - int64FromInterface(oldest["in"])
			deltaOut := int64FromInterface(newest["out"]) - int64FromInterface(oldest["out"])
			session["measurement_window_io_1s"] = math.Round(deltaTime*100) / 100
			if deltaTime >= 1 {
				mbpsInAvg := (float64(deltaIn) * 8) / (deltaTime * 1024 * 1024)
				mbpsOutAvg := (float64(deltaOut) * 8) / (deltaTime * 1024 * 1024)
				session["mbps_in_1s"] = math.Round(mbpsInAvg*100) / 100
				session["mbps_out_1s"] = math.Round(mbpsOutAvg*100) / 100
			} else {
				session["mbps_in_1s"] = nil
				session["mbps_out_1s"] = nil
			}
		}
	}

	if len(filteredActive) >= 2 {
		var sumDt float64
		var sumIn int64
		var sumOut int64
		for _, sample := range filteredActive {
			sumDt += floatFromInterface(sample["dt"])
			sumIn += int64FromInterface(sample["in"])
			sumOut += int64FromInterface(sample["out"])
		}
		session["measurement_window_active"] = math.Round(sumDt*10) / 10
		if sumDt >= 12 {
			mbpsInActive := (float64(sumIn) * 8) / (sumDt * 1024 * 1024)
			mbpsOutActive := (float64(sumOut) * 8) / (sumDt * 1024 * 1024)
			session["mbps_in_active"] = math.Round(mbpsInActive*100) / 100
			session["mbps_out_active"] = math.Round(mbpsOutActive*100) / 100
		} else {
			session["mbps_in_active"] = nil
			session["mbps_out_active"] = nil
		}
	}
}
