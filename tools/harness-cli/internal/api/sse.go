package api

// Minimal SSE consumer for /api/v2/timeseries. We can't use the
// generated client's GetTimeseries directly for streaming because
// oapi-codegen returns the raw *http.Response with the connection
// open — fine, but the JSON-decode-into-typed-struct step it
// otherwise inlines doesn't apply (each frame is its own value with
// its own type discriminated by the SSE `event:` line). So we own the
// frame loop ourselves.
//
// Why hand-rolled instead of a third-party SSE library: the wire
// dialect we need is tiny (event:/data:/id:/retry: + blank-line
// delimiter), the third-party deps add weight, and the dashboard's
// SharedWorker already proves you can scan it with a 5-line loop.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// SSEFrame is one fully-assembled Server-Sent Event. ID is empty if
// the server didn't send an `id:` line; LastEventID isn't tracked
// here because the v2 timeseries server doesn't honour Last-Event-ID
// resumes yet (it ignores the header per the spec comment).
type SSEFrame struct {
	Event string
	Data  string
	ID    string
}

// TimeseriesParams is the small subset of GET /api/v2/timeseries
// query params commands care about. Kept separate from the generated
// GetTimeseriesParams so callers can construct it without importing
// the forwarder pkg.
type TimeseriesParams struct {
	PlayerID string   // optional — empty streams all players
	PlayID   string   // optional — filter to a single play
	Streams  []string // required — at least one of samples|network|events
	Bundles  []string // optional — e.g. ["network", "lanes_v1"]
	MaxHz    int      // optional — 0 disables rate limiting
}

// Timeseries opens an SSE stream and invokes onFrame for every event
// the server sends. Blocks until ctx is cancelled, the server closes
// the connection, or a fatal read error occurs. Heartbeats (event:
// "heartbeat") are passed through — callers decide whether to log
// them. Returned errors are wrapped with the operation context.
func (c *Client) Timeseries(ctx context.Context, p TimeseriesParams, onFrame func(SSEFrame) error) error {
	if len(p.Streams) == 0 {
		return errors.New("timeseries: at least one stream required")
	}
	q := url.Values{}
	q.Set("streams", strings.Join(p.Streams, ","))
	if p.PlayerID != "" {
		q.Set("player_id", p.PlayerID)
	}
	if p.PlayID != "" {
		q.Set("play_id", p.PlayID)
	}
	if len(p.Bundles) > 0 {
		q.Set("bundles", strings.Join(p.Bundles, ","))
	}
	if p.MaxHz > 0 {
		q.Set("max_hz", strconv.Itoa(p.MaxHz))
	}

	// Forwarder reads live behind the /analytics prefix at the edge —
	// nginx strips it before proxying to the forwarder service. The
	// dashboard's useSessionTimeSeries does the same; if you remove
	// the prefix you hit the proxy 404 path instead.
	endpoint := c.BaseURL + "/analytics/api/v2/timeseries?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("timeseries: build request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if c.BasicAuth != "" {
		req.SetBasicAuth(splitBasic(c.BasicAuth))
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("timeseries: open stream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		return fmt.Errorf("timeseries: %d %s: %s", resp.StatusCode, resp.Status, strings.TrimSpace(string(body)))
	}

	return scanSSE(resp.Body, onFrame)
}

// scanSSE reads SSE frames from r and invokes onFrame for each
// complete event. Returns nil on clean EOF, the underlying error
// otherwise. A blank line terminates a frame; lines starting with
// ":" are comments (commonly used for keep-alive). We accumulate
// `data:` lines with newline separators per the spec.
func scanSSE(r io.Reader, onFrame func(SSEFrame) error) error {
	sc := bufio.NewScanner(r)
	// SSE frames can be large (a network row with cookies/headers
	// occasionally pushes well past the default 64 KiB scanner buffer).
	// 1 MiB is comfortably above anything the forwarder emits today.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var current SSEFrame
	var dataLines []string
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			// Blank line → dispatch.
			if len(dataLines) > 0 || current.Event != "" {
				current.Data = strings.Join(dataLines, "\n")
				if err := onFrame(current); err != nil {
					return err
				}
			}
			current = SSEFrame{}
			dataLines = nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment / keep-alive
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			current.Event = value
		case "data":
			dataLines = append(dataLines, value)
		case "id":
			current.ID = value
		case "retry":
			// We don't auto-reconnect today; ignore.
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("sse scan: %w", err)
	}
	return nil
}

func splitBasic(s string) (string, string) {
	user, pass, ok := strings.Cut(s, ":")
	if !ok {
		return s, ""
	}
	return user, pass
}

// EventsParams configures /api/v2/events (proxy SSE — lifecycle
// events, not the forwarder's timeseries). Type filters narrow the
// stream server-side.
type EventsParams struct {
	PlayerID string   // optional — filter to one player's events
	Types    []string // optional — repeated `type=` query params
}

// Events opens an SSE stream against the PROXY's /api/v2/events
// (note: no /analytics prefix — events is proxy-side, not forwarder).
// Same callback shape as Timeseries.
func (c *Client) Events(ctx context.Context, p EventsParams, onFrame func(SSEFrame) error) error {
	q := url.Values{}
	if p.PlayerID != "" {
		q.Set("player_id", p.PlayerID)
	}
	for _, t := range p.Types {
		q.Add("type", t)
	}
	endpoint := c.BaseURL + "/api/v2/events"
	if encoded := q.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("events: build request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if c.BasicAuth != "" {
		req.SetBasicAuth(splitBasic(c.BasicAuth))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("events: open stream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		return fmt.Errorf("events: %d %s: %s", resp.StatusCode, resp.Status, strings.TrimSpace(string(body)))
	}
	return scanSSE(resp.Body, onFrame)
}
