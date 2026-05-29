// CMCD (Common Media Client Data, CTA-5004) capture. iOS 18+ AVPlayer
// emits CMCD-Request / CMCD-Object / CMCD-Status / CMCD-Session headers
// when the app sets resourceLoader.sendsCommonMediaClientDataAsHTTPHeaders.
// We also accept the query-string form (?CMCD=...) used by web players.
// Parser preserves all keys (typed ones go to extracted fields for SQL;
// the full map goes to Raw so the test framework can discover the
// ground-truth set of keys a given player actually emits).
package main

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// cmcdHeaderNames are the four CTA-5004 header buckets. Lower-cased for
// canonical comparison against http.Header (which already normalises).
var cmcdHeaderNames = []string{
	"CMCD-Request",
	"CMCD-Object",
	"CMCD-Status",
	"CMCD-Session",
}

// CMCDData carries the parsed CMCD payload of a single request. Raw is
// the union of every key seen across the four headers and the ?CMCD=
// query parameter — values are post-unquoting and post-percent-decoding.
// Typed fields are populated for the well-known CTA-5004 keys so the
// dashboard / SQL can query them without re-parsing the map.
type CMCDData struct {
	// Raw header payloads, verbatim — useful for cross-checking the
	// parsed map against the wire form when discovering unknown keys.
	HeaderRequest string `json:"header_request,omitempty"`
	HeaderObject  string `json:"header_object,omitempty"`
	HeaderStatus  string `json:"header_status,omitempty"`
	HeaderSession string `json:"header_session,omitempty"`
	HeaderQuery   string `json:"header_query,omitempty"`

	// Parsed key/value pairs across all buckets. Keys are the CMCD short
	// names from CTA-5004; values are the string form after dequoting.
	Raw map[string]string `json:"raw,omitempty"`

	// Extracted typed fields for the well-known CTA-5004 keys. Stored as
	// individual columns in ClickHouse for cheap GROUP BY / WHERE.
	BR      uint32 `json:"br,omitempty"`       // encoded bitrate kbps
	BL      uint32 `json:"bl,omitempty"`       // buffer length ms
	BS      bool   `json:"bs,omitempty"`       // buffer starvation
	DL      uint32 `json:"dl,omitempty"`       // deadline ms
	MTP     uint32 `json:"mtp,omitempty"`      // measured throughput kbps
	RTP     uint32 `json:"rtp,omitempty"`      // requested max throughput kbps
	TB      uint32 `json:"tb,omitempty"`       // top bitrate kbps
	D       uint32 `json:"d,omitempty"`        // object duration ms
	SU      bool   `json:"su,omitempty"`       // startup
	OT      string `json:"ot,omitempty"`       // object type token (m / a / v / av / i / c / tt / k / o)
	SF      string `json:"sf,omitempty"`       // streaming format (d / h / s / o); iOS reports lh for LL-HLS
	ST      string `json:"st,omitempty"`       // stream type (v / l)
	CID     string `json:"cid,omitempty"`      // content id
	SID     string `json:"sid,omitempty"`      // session id (UUID per CMCD session)
	PR      string `json:"pr,omitempty"`       // playback rate (string for non-1.0 fractional values)
	V       uint32 `json:"v,omitempty"`        // CMCD version
}

// parseCMCD pulls CMCD-* headers and the ?CMCD= query parameter from a
// player request. Returns nil if neither carrier is present, so callers
// can leave the NetworkLogEntry.CMCD field empty for non-CMCD clients.
func parseCMCD(headers http.Header, q url.Values) *CMCDData {
	d := &CMCDData{Raw: map[string]string{}}
	any := false
	for _, name := range cmcdHeaderNames {
		v := headers.Get(name)
		if v == "" {
			continue
		}
		any = true
		switch name {
		case "CMCD-Request":
			d.HeaderRequest = v
		case "CMCD-Object":
			d.HeaderObject = v
		case "CMCD-Status":
			d.HeaderStatus = v
		case "CMCD-Session":
			d.HeaderSession = v
		}
		parseCMCDPayload(v, d.Raw)
	}
	if qv := q.Get("CMCD"); qv != "" {
		any = true
		d.HeaderQuery = qv
		parseCMCDPayload(qv, d.Raw)
	}
	if !any {
		return nil
	}
	d.extractTyped()
	return d
}

// parseCMCDPayload tokenises a single CMCD payload string ("br=2500,
// bl=12345,sid=\"...\"") into key/value pairs, respecting quoted values
// that may contain commas. Boolean keys (no `=`) are stored as "true".
func parseCMCDPayload(payload string, out map[string]string) {
	for _, pair := range splitCMCD(payload) {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		var key, value string
		if eq := strings.IndexByte(pair, '='); eq >= 0 {
			key = pair[:eq]
			value = pair[eq+1:]
		} else {
			key = pair
			value = "true"
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) && len(value) >= 2 {
			value = strings.ReplaceAll(value[1:len(value)-1], `\"`, `"`)
		}
		if key == "" {
			continue
		}
		out[strings.ToLower(key)] = value
	}
}

// splitCMCD splits a CMCD payload on commas that are NOT inside a
// quoted string. CTA-5004 quoting allows commas inside `"..."`.
func splitCMCD(s string) []string {
	var out []string
	var buf strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQuote = !inQuote
			buf.WriteByte(c)
		case c == ',' && !inQuote:
			out = append(out, buf.String())
			buf.Reset()
		default:
			buf.WriteByte(c)
		}
	}
	if buf.Len() > 0 {
		out = append(out, buf.String())
	}
	return out
}

// extractTyped pulls the well-known CMCD keys out of Raw into the
// typed CMCDData fields. Silent on parse errors — Raw still has the
// original string so the dashboard can investigate.
func (d *CMCDData) extractTyped() {
	if d == nil || d.Raw == nil {
		return
	}
	d.BR = uintFromMap(d.Raw, "br")
	d.BL = uintFromMap(d.Raw, "bl")
	d.DL = uintFromMap(d.Raw, "dl")
	d.MTP = uintFromMap(d.Raw, "mtp")
	d.RTP = uintFromMap(d.Raw, "rtp")
	d.TB = uintFromMap(d.Raw, "tb")
	d.D = uintFromMap(d.Raw, "d")
	d.V = uintFromMap(d.Raw, "v")
	d.BS = boolFromMap(d.Raw, "bs")
	d.SU = boolFromMap(d.Raw, "su")
	d.OT = d.Raw["ot"]
	d.SF = d.Raw["sf"]
	d.ST = d.Raw["st"]
	d.CID = d.Raw["cid"]
	d.SID = d.Raw["sid"]
	d.PR = d.Raw["pr"]
}

func uintFromMap(m map[string]string, k string) uint32 {
	v, ok := m[k]
	if !ok {
		return 0
	}
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		return 0
	}
	return uint32(n)
}

func boolFromMap(m map[string]string, k string) bool {
	v, ok := m[k]
	if !ok {
		return false
	}
	return v == "true" || v == "1"
}
