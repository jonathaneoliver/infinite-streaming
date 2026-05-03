// Per-request HAR-style log archival. The forwarder polls go-proxy's
// /api/session/<id>/network endpoint for each live session, dedupes
// against an in-memory fingerprint set, and batch-inserts new rows into
// the ClickHouse network_requests table. The session-viewer UI reads
// from /analytics/api/network_requests so the network log fold replays
// even after the proxy has released the session.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// netRow is the JSONEachRow shape for network_requests. Tags match the
// ClickHouse column names exactly so the table accepts the body as-is.
type netRow struct {
	Ts                   string  `json:"ts"`
	SessionID            string  `json:"session_id"`
	PlayID               string  `json:"play_id"`
	Method               string  `json:"method"`
	URL                  string  `json:"url"`
	UpstreamURL          string  `json:"upstream_url"`
	Path                 string  `json:"path"`
	RequestKind          string  `json:"request_kind"`
	Status               uint16  `json:"status"`
	BytesIn              int64   `json:"bytes_in"`
	BytesOut             int64   `json:"bytes_out"`
	ContentType          string  `json:"content_type"`
	RequestRange         string  `json:"request_range"`
	ResponseContentRange string  `json:"response_content_range"`
	DNSMs                float32 `json:"dns_ms"`
	ConnectMs            float32 `json:"connect_ms"`
	TLSMs                float32 `json:"tls_ms"`
	TTFBMs               float32 `json:"ttfb_ms"`
	TransferMs           float32 `json:"transfer_ms"`
	TotalMs              float32 `json:"total_ms"`
	ClientWaitMs         float32 `json:"client_wait_ms"`
	Faulted              uint8   `json:"faulted"`
	FaultType            string  `json:"fault_type"`
	FaultAction          string  `json:"fault_action"`
	FaultCategory        string  `json:"fault_category"`
	RequestHeaders       string  `json:"request_headers"`
	ResponseHeaders      string  `json:"response_headers"`
	QueryString          string  `json:"query_string"`
	EntryFingerprint     uint64  `json:"entry_fingerprint"`
}

// netEntry mirrors go-proxy's NetworkLogEntry. Only the fields we keep
// are listed; unknown JSON keys are tolerated by the decoder.
type netEntry struct {
	Timestamp            time.Time     `json:"timestamp"`
	Method               string        `json:"method"`
	URL                  string        `json:"url"`
	UpstreamURL          string        `json:"upstream_url"`
	Path                 string        `json:"path"`
	RequestKind          string        `json:"request_kind"`
	Status               int           `json:"status"`
	BytesIn              int64         `json:"bytes_in"`
	BytesOut             int64         `json:"bytes_out"`
	ContentType          string        `json:"content_type"`
	PlayID               string        `json:"play_id"`
	RequestHeaders       []nameValue   `json:"request_headers"`
	ResponseHeaders      []nameValue   `json:"response_headers"`
	QueryString          []nameValue   `json:"query_string"`
	DNSMs                float64       `json:"dns_ms"`
	ConnectMs            float64       `json:"connect_ms"`
	TLSMs                float64       `json:"tls_ms"`
	TTFBMs               float64       `json:"ttfb_ms"`
	TransferMs           float64       `json:"transfer_ms"`
	TotalMs              float64       `json:"total_ms"`
	ClientWaitMs         float64       `json:"client_wait_ms"`
	Faulted              bool          `json:"faulted"`
	FaultType            string        `json:"fault_type"`
	FaultAction          string        `json:"fault_action"`
	FaultCategory        string        `json:"fault_category"`
	RequestRange         string        `json:"request_range"`
	ResponseContentRange string        `json:"response_content_range"`
}

type nameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// netSeen tracks fingerprints already inserted, per session. Bounded
// per-session so a chatty session can't OOM the forwarder.
type netSeen struct {
	mu  sync.Mutex
	max int
	m   map[string]map[uint64]struct{}
}

func newNetSeen(maxPerSession int) *netSeen {
	return &netSeen{max: maxPerSession, m: make(map[string]map[uint64]struct{})}
}

func (s *netSeen) check(sessionID string, fp uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	set, ok := s.m[sessionID]
	if !ok {
		set = make(map[uint64]struct{})
		s.m[sessionID] = set
	}
	if _, hit := set[fp]; hit {
		return true
	}
	if len(set) >= s.max {
		// Bound: drop ~25% of old fingerprints. Coarse, but in practice
		// re-polling a long-lived session never hands back rows older
		// than the proxy's ring buffer (which is itself bounded).
		drop := s.max / 4
		i := 0
		for k := range set {
			delete(set, k)
			i++
			if i >= drop {
				break
			}
		}
	}
	set[fp] = struct{}{}
	return false
}

func (s *netSeen) prune(active map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.m {
		if _, ok := active[k]; !ok {
			delete(s.m, k)
		}
	}
}

// netFingerprint computes a stable hash of the entry's identity columns
// so re-polling the same session doesn't re-insert. Includes ts ms,
// path, method, status, play_id, bytes_in.
func netFingerprint(e *netEntry) uint64 {
	h := sha256.New()
	tsBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(tsBytes, uint64(e.Timestamp.UnixMilli()))
	h.Write(tsBytes)
	h.Write([]byte{0})
	h.Write([]byte(e.Path))
	h.Write([]byte{0})
	h.Write([]byte(e.Method))
	h.Write([]byte{0})
	bIn := make([]byte, 8)
	binary.BigEndian.PutUint64(bIn, uint64(e.BytesIn))
	h.Write(bIn)
	h.Write([]byte{0})
	statusBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(statusBytes, uint16(e.Status))
	h.Write(statusBytes)
	h.Write([]byte{0})
	h.Write([]byte(e.PlayID))
	sum := h.Sum(nil)
	return binary.BigEndian.Uint64(sum[:8])
}

// jsonOrEmpty marshals a value to JSON, returning "" on nil/empty/error
// so we don't pollute the column with "null".
func jsonOrEmpty(v interface{}) string {
	switch t := v.(type) {
	case []nameValue:
		if len(t) == 0 {
			return ""
		}
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return ""
	}
	return string(b)
}

func entryToRow(sessionID string, e *netEntry) netRow {
	faulted := uint8(0)
	if e.Faulted {
		faulted = 1
	}
	return netRow{
		Ts:                   e.Timestamp.UTC().Format("2006-01-02 15:04:05.000"),
		SessionID:            sessionID,
		PlayID:               e.PlayID,
		Method:               e.Method,
		URL:                  e.URL,
		UpstreamURL:          e.UpstreamURL,
		Path:                 e.Path,
		RequestKind:          e.RequestKind,
		Status:               uint16(e.Status),
		BytesIn:              e.BytesIn,
		BytesOut:             e.BytesOut,
		ContentType:          e.ContentType,
		RequestRange:         e.RequestRange,
		ResponseContentRange: e.ResponseContentRange,
		DNSMs:                float32(e.DNSMs),
		ConnectMs:            float32(e.ConnectMs),
		TLSMs:                float32(e.TLSMs),
		TTFBMs:               float32(e.TTFBMs),
		TransferMs:           float32(e.TransferMs),
		TotalMs:              float32(e.TotalMs),
		ClientWaitMs:         float32(e.ClientWaitMs),
		Faulted:              faulted,
		FaultType:            e.FaultType,
		FaultAction:          e.FaultAction,
		FaultCategory:        e.FaultCategory,
		RequestHeaders:       jsonOrEmpty(e.RequestHeaders),
		ResponseHeaders:      jsonOrEmpty(e.ResponseHeaders),
		QueryString:          jsonOrEmpty(e.QueryString),
		EntryFingerprint:     netFingerprint(e),
	}
}

// proxyBaseFromSSE derives the proxy's base URL from the configured SSE
// URL so the network poller hits the same origin (e.g. go-server:30081).
func proxyBaseFromSSE(sseURL string) string {
	u, err := url.Parse(sseURL)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// netStreamEvent matches the JSON go-proxy emits on /api/network/stream.
type netStreamEvent struct {
	SessionID string   `json:"session_id"`
	Entry     netEntry `json:"entry"`
}

// streamNetworkSSE subscribes to go-proxy's /api/network/stream endpoint.
// One SSE `data:` line per request as it lands in the proxy's ring
// buffer; we dedup (best-effort, in case of reconnects mid-burst) and
// forward to the batch inserter. On disconnect we reconnect with
// exponential backoff (capped); nothing is replayed by the proxy, so
// a long outage means we miss whatever entries were emitted while down.
func streamNetworkSSE(ctx context.Context, cfg config, seen *netSeen, out chan<- netRow) error {
	base := proxyBaseFromSSE(cfg.sseURL)
	if base == "" {
		return fmt.Errorf("cannot derive proxy base from SSE URL %q", cfg.sseURL)
	}
	endpoint := strings.TrimRight(base, "/") + "/api/network/stream"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("net stream %d", resp.StatusCode)
	}
	br := newSSEReader(resp.Body)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		data, err := br.next()
		if err != nil {
			return err
		}
		if len(data) == 0 {
			continue
		}
		var ev netStreamEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			continue
		}
		if ev.SessionID == "" || ev.Entry.Timestamp.IsZero() {
			continue
		}
		fp := netFingerprint(&ev.Entry)
		if seen.check(ev.SessionID, fp) {
			continue
		}
		select {
		case out <- entryToRow(ev.SessionID, &ev.Entry):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// runNetworkStream drives streamNetworkSSE with reconnect + backoff.
// Replaces the old netPoller. We keep netActiveSet as a no-op for now
// (handlePayload still calls .replace()) so we don't ripple the change
// further — pruning the seen set on SSE pulse is still useful even with
// the new event source.
func runNetworkStream(ctx context.Context, cfg config, seen *netSeen, out chan<- netRow) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := streamNetworkSSE(ctx, cfg, seen, out)
		if ctx.Err() != nil {
			return
		}
		log.Printf("net sse stream ended: %v (reconnecting in %s)", err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// newSSEReader / .next() decode SSE event frames. We only care about the
// `data:` field; comments (lines starting with `:`) and empty heartbeats
// produce empty results that the caller skips.
type sseReader struct {
	r   io.Reader
	buf []byte
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{r: r, buf: make([]byte, 0, 16*1024)}
}

func (s *sseReader) next() ([]byte, error) {
	tmp := make([]byte, 4096)
	for {
		// Look for the end-of-frame marker `\n\n` in the accumulated buffer.
		if idx := indexDoubleNewline(s.buf); idx >= 0 {
			frame := s.buf[:idx]
			s.buf = s.buf[idx+2:]
			return parseSSEData(frame), nil
		}
		n, err := s.r.Read(tmp)
		if n > 0 {
			s.buf = append(s.buf, tmp[:n]...)
		}
		if err != nil {
			return nil, err
		}
	}
}

func indexDoubleNewline(b []byte) int {
	for i := 0; i+1 < len(b); i++ {
		if b[i] == '\n' && b[i+1] == '\n' {
			return i
		}
	}
	return -1
}

func parseSSEData(frame []byte) []byte {
	// Multi-line `data:` fields are joined with `\n` per the SSE spec,
	// but our publisher always emits a single-line `data:` per event.
	for _, line := range bytes.Split(frame, []byte("\n")) {
		if len(line) >= 5 && bytes.Equal(line[:5], []byte("data:")) {
			v := line[5:]
			if len(v) > 0 && v[0] == ' ' {
				v = v[1:]
			}
			return v
		}
	}
	return nil
}

func batchInsertNet(ctx context.Context, cfg config, in <-chan netRow) {
	buf := make([]netRow, 0, cfg.flushBatch)
	tick := time.NewTicker(cfg.flushEvery)
	defer tick.Stop()
	flush := func() {
		if len(buf) == 0 {
			return
		}
		if err := insertNet(ctx, cfg, buf); err != nil {
			log.Printf("net insert failed (%d rows dropped): %v", len(buf), err)
		}
		buf = buf[:0]
	}
	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case r, ok := <-in:
			if !ok {
				flush()
				return
			}
			buf = append(buf, r)
			if len(buf) >= cfg.flushBatch {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

// chQueryBytes runs a ClickHouse query and returns the full response
// body. Used for endpoints that need to massage the result before
// sending to the client (e.g. wrapping JSONEachRow lines in an envelope).
// User-supplied values must be passed via `params` and referenced in the
// SQL with `{name:Type}` placeholders — never interpolated.
func chQueryBytes(ctx context.Context, cfg config, query string, params map[string]string) ([]byte, error) {
	u, err := url.Parse(cfg.clickhouseURL)
	if err != nil {
		return nil, err
	}
	qs := u.Query()
	qs.Set("default_format", "JSONEachRow")
	for k, v := range params {
		qs.Set("param_"+k, v)
	}
	u.RawQuery = qs.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(query))
	if err != nil {
		return nil, err
	}
	if cfg.chUser != "" {
		req.SetBasicAuth(cfg.chUser, cfg.chPassword)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("clickhouse %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// reinflateNetRowJSON converts the JSONEachRow row from network_requests
// into the shape the browser network log code expects. The three
// columns request_headers / response_headers / query_string are stored
// as JSON-encoded strings (so e.g. "[{\"name\":...}]" arrives as a
// String), but the consumer wants actual arrays. We re-parse those
// columns and splice them back in.
func reinflateNetRowJSON(line []byte) []byte {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return line
	}
	for _, key := range [...]string{"request_headers", "response_headers", "query_string"} {
		val, ok := raw[key]
		if !ok {
			continue
		}
		// val is a JSON String containing JSON. Decode the outer string
		// then re-emit the inner JSON as raw.
		var inner string
		if err := json.Unmarshal(val, &inner); err != nil {
			continue
		}
		if inner == "" {
			raw[key] = json.RawMessage("[]")
			continue
		}
		raw[key] = json.RawMessage(inner)
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return line
	}
	return out
}

func insertNet(ctx context.Context, cfg config, rows []netRow) error {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for i := range rows {
		if err := enc.Encode(&rows[i]); err != nil {
			return err
		}
	}
	q := fmt.Sprintf("INSERT INTO %s.network_requests FORMAT JSONEachRow", cfg.chDatabase)
	u, err := url.Parse(cfg.clickhouseURL)
	if err != nil {
		return err
	}
	qs := u.Query()
	qs.Set("query", q)
	u.RawQuery = qs.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), &body)
	if err != nil {
		return err
	}
	if cfg.chUser != "" {
		req.SetBasicAuth(cfg.chUser, cfg.chPassword)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("clickhouse %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}
