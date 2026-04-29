// Package har builds W3C HAR 1.2 documents from go-proxy NetworkLogEntry data.
//
// HAR 1.2 spec: http://www.softwareishard.com/blog/har-12-spec/
//
// Extensions (prefixed with `_`) carry InfiniteStream-specific metadata that
// generic HAR viewers ignore but our dashboard surfaces:
//   - entry._extensions.fault: fault injection metadata per request
//   - entry._extensions.requestKind: "manifest" / "segment" / "master_manifest"
//   - log._extensions.incident: snapshot reason + player metadata at capture
//   - log._extensions.session: session_id / player_id / group_id
package har

import (
	"time"
)

const (
	HARVersion     = "1.2"
	CreatorName    = "infinitestream-go-proxy"
	CreatorVersion = "1.0"
)

// HAR is the top-level document.
type HAR struct {
	Log Log `json:"log"`
}

// Log is the HAR log object.
type Log struct {
	Version    string                 `json:"version"`
	Creator    Creator                `json:"creator"`
	Pages      []Page                 `json:"pages,omitempty"`
	Entries    []Entry                `json:"entries"`
	Comment    string                 `json:"comment,omitempty"`
	Extensions map[string]interface{} `json:"_extensions,omitempty"`
}

type Creator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Comment string `json:"comment,omitempty"`
}

type Page struct {
	StartedDateTime string      `json:"startedDateTime"`
	ID              string      `json:"id"`
	Title           string      `json:"title"`
	PageTimings     PageTimings `json:"pageTimings"`
}

type PageTimings struct {
	OnContentLoad float64 `json:"onContentLoad,omitempty"`
	OnLoad        float64 `json:"onLoad,omitempty"`
}

// Entry is a single HTTP request/response pair.
type Entry struct {
	StartedDateTime string                 `json:"startedDateTime"`
	Time            float64                `json:"time"`
	Request         Request                `json:"request"`
	Response        Response               `json:"response"`
	Cache           Cache                  `json:"cache"`
	Timings         Timings                `json:"timings"`
	ServerIPAddress string                 `json:"serverIPAddress,omitempty"`
	Connection      string                 `json:"connection,omitempty"`
	PageRef         string                 `json:"pageref,omitempty"`
	Comment         string                 `json:"comment,omitempty"`
	Extensions      map[string]interface{} `json:"_extensions,omitempty"`
}

type Request struct {
	Method      string        `json:"method"`
	URL         string        `json:"url"`
	HTTPVersion string        `json:"httpVersion"`
	Cookies     []NameValue   `json:"cookies"`
	Headers     []NameValue   `json:"headers"`
	QueryString []NameValue   `json:"queryString"`
	PostData    *PostData     `json:"postData,omitempty"`
	HeadersSize int64         `json:"headersSize"`
	BodySize    int64         `json:"bodySize"`
}

type Response struct {
	Status      int         `json:"status"`
	StatusText  string      `json:"statusText"`
	HTTPVersion string      `json:"httpVersion"`
	Cookies     []NameValue `json:"cookies"`
	Headers     []NameValue `json:"headers"`
	Content     Content     `json:"content"`
	RedirectURL string      `json:"redirectURL"`
	HeadersSize int64       `json:"headersSize"`
	BodySize    int64       `json:"bodySize"`
}

type NameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type PostData struct {
	MimeType string      `json:"mimeType"`
	Params   []NameValue `json:"params,omitempty"`
	Text     string      `json:"text,omitempty"`
}

type Content struct {
	Size     int64  `json:"size"`
	MimeType string `json:"mimeType"`
	Text     string `json:"text,omitempty"`
	Comment  string `json:"comment,omitempty"`
}

type Cache struct {
	BeforeRequest *CacheState `json:"beforeRequest,omitempty"`
	AfterRequest  *CacheState `json:"afterRequest,omitempty"`
}

type CacheState struct {
	Expires    string `json:"expires,omitempty"`
	LastAccess string `json:"lastAccess,omitempty"`
	ETag       string `json:"eTag,omitempty"`
	HitCount   int    `json:"hitCount,omitempty"`
}

// Timings — all values in milliseconds, use -1 to indicate "not applicable".
type Timings struct {
	Blocked float64 `json:"blocked"`
	DNS     float64 `json:"dns"`
	Connect float64 `json:"connect"`
	Send    float64 `json:"send"`
	Wait    float64 `json:"wait"`
	Receive float64 `json:"receive"`
	SSL     float64 `json:"ssl"`
}

// Source is the minimal NetworkLogEntry shape the builder reads. Defining it
// here as an interface-free struct keeps the har package decoupled from
// main.go's full type — the caller copies fields in.
//
// Two timing perspectives are carried:
//
//   - DNSMs/ConnectMs/TLSMs/TTFBMs are the *upstream* timings (proxy →
//     origin). They land under the entry's _extensions.upstream block
//     for forensics ("was latency on our side or the origin's?").
//
//   - ClientWaitMs and TransferMs are the *downstream* (proxy → player)
//     timings — what the player perceived. These map into HAR's
//     standard timings.wait and timings.receive so a HAR viewer
//     surfaces them by default.
type Source struct {
	Timestamp    time.Time
	Method       string
	URL          string // player-facing URL
	RequestKind  string
	Status       int
	BytesIn      int64
	BytesOut     int64
	ContentType  string
	ClientWaitMs float64 // request received → first response byte sent to client
	TransferMs   float64 // first response byte → response complete
	TotalMs      float64

	// Upstream context, surfaced via _extensions.upstream.
	UpstreamURL string
	DNSMs       float64
	ConnectMs   float64
	TLSMs       float64
	TTFBMs      float64

	// HTTP-level metadata captured from the request and upstream
	// response. Sensitive headers (Cookie / Authorization) are filtered
	// at the proxy capture site before they reach here.
	RequestHeaders  []NameValue
	ResponseHeaders []NameValue
	QueryString     []NameValue

	Faulted       bool
	FaultType     string
	FaultAction   string
	FaultCategory string
}

// Incident captures why a HAR snapshot was taken (for player-driven captures).
type Incident struct {
	Reason    string                 `json:"reason"`
	Source    string                 `json:"source"` // "dashboard", "rest", "player"
	PlayerID  string                 `json:"player_id,omitempty"`
	SessionID string                 `json:"session_id,omitempty"`
	GroupID   string                 `json:"group_id,omitempty"`
	Timestamp time.Time              `json:"timestamp"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

// Context is an analyst-friendly snapshot of *everything around* a HAR
// at the moment it was captured (issue #281). Lands under
// `_extensions.context` at the document level so an incident HAR is
// self-contained — no cross-referencing of separate logs needed to
// answer "what device, what stream, what test scenario, how far into
// playback was this?".
type Context struct {
	Device        *DeviceContext         `json:"device,omitempty"`
	Stream        *StreamContext         `json:"stream,omitempty"`
	Scenario      *ScenarioContext       `json:"scenario,omitempty"`
	Timing        *TimingContext         `json:"timing,omitempty"`
	RecoveryChain []string               `json:"recovery_chain,omitempty"`
	Extra         map[string]interface{} `json:"extra,omitempty"`
}

// DeviceContext is the player-supplied device fingerprint.
type DeviceContext struct {
	Model       string `json:"model,omitempty"`
	OSVersion   string `json:"os_version,omitempty"`
	AppVersion  string `json:"app_version,omitempty"`
	NetworkType string `json:"network_type,omitempty"`
}

// StreamContext describes what the player is playing.
type StreamContext struct {
	ContentID         string `json:"content_id,omitempty"`
	Protocol          string `json:"protocol,omitempty"` // "hls" or "dash"
	Codec             string `json:"codec,omitempty"`
	InitialVariantURL string `json:"initial_variant_url,omitempty"`
}

// ScenarioContext is a snapshot of the test conditions active when the
// HAR was taken — server-side from the session record.
type ScenarioContext struct {
	FaultSettings map[string]interface{} `json:"fault_settings,omitempty"`
	NftablesShape map[string]interface{} `json:"nftables_shape,omitempty"`
}

// TimingContext anchors the incident in playback time.
type TimingContext struct {
	PlayStartedAt    string  `json:"play_started_at,omitempty"`    // RFC3339, when the play began
	IncidentOffsetS  float64 `json:"incident_offset_s,omitempty"`  // seconds since play_started_at
}

// BuildOptions controls extra metadata embedded at the HAR Log level.
type BuildOptions struct {
	SessionID string
	PlayerID  string
	GroupID   string
	Incident  *Incident
	Context   *Context
}

// Build constructs a HAR document from the given NetworkLogEntry sources.
func Build(sources []Source, opts BuildOptions) HAR {
	entries := make([]Entry, 0, len(sources))
	for _, s := range sources {
		entries = append(entries, buildEntry(s))
	}

	log := Log{
		Version: HARVersion,
		Creator: Creator{
			Name:    CreatorName,
			Version: CreatorVersion,
		},
		Entries: entries,
	}

	ext := map[string]interface{}{}
	if opts.SessionID != "" || opts.PlayerID != "" || opts.GroupID != "" {
		session := map[string]string{}
		if opts.SessionID != "" {
			session["session_id"] = opts.SessionID
		}
		if opts.PlayerID != "" {
			session["player_id"] = opts.PlayerID
		}
		if opts.GroupID != "" {
			session["group_id"] = opts.GroupID
		}
		ext["session"] = session
	}
	if opts.Incident != nil {
		ext["incident"] = opts.Incident
	}
	if opts.Context != nil && !opts.Context.isEmpty() {
		ext["context"] = opts.Context
	}
	if len(ext) > 0 {
		log.Extensions = ext
	}

	return HAR{Log: log}
}

// isEmpty reports whether the Context has nothing populated. Used to
// avoid emitting an `_extensions.context: {}` block.
func (c *Context) isEmpty() bool {
	if c == nil {
		return true
	}
	return c.Device == nil && c.Stream == nil && c.Scenario == nil &&
		c.Timing == nil && len(c.RecoveryChain) == 0 && len(c.Extra) == 0
}

func buildEntry(s Source) Entry {
	startedDate := s.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z")

	// Map *downstream* timings (proxy → player) into the standard HAR
	// Timings block — that's what HAR viewers surface as the player's
	// experience. Upstream timings live under _extensions.upstream below.
	//
	// HAR 1.2: -1 means "not applicable / not measured". DNS/Connect/SSL
	// describe how the *client* reached its server; from go-proxy → player
	// they're a keepalive HTTP transaction with no client-side connect
	// to measure here, so they're -1. (Issue #283 will add real client
	// RTT capture.)
	timings := Timings{
		Blocked: -1,
		DNS:     -1,
		Connect: -1,
		Send:    -1,
		Wait:    msOrNeg(s.ClientWaitMs),
		Receive: msOrNeg(s.TransferMs),
		SSL:     -1,
	}

	statusText := statusTextFor(s.Status)
	mimeType := s.ContentType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	requestHeaders := s.RequestHeaders
	if requestHeaders == nil {
		requestHeaders = []NameValue{}
	}
	responseHeaders := s.ResponseHeaders
	if responseHeaders == nil {
		responseHeaders = []NameValue{}
	}
	queryString := s.QueryString
	if queryString == nil {
		queryString = []NameValue{}
	}

	entry := Entry{
		StartedDateTime: startedDate,
		Time:            s.TotalMs,
		Request: Request{
			Method:      defaultStr(s.Method, "GET"),
			URL:         s.URL,
			HTTPVersion: "HTTP/1.1",
			Cookies:     []NameValue{},
			Headers:     requestHeaders,
			QueryString: queryString,
			HeadersSize: -1,
			BodySize:    s.BytesOut,
		},
		Response: Response{
			Status:      s.Status,
			StatusText:  statusText,
			HTTPVersion: "HTTP/1.1",
			Cookies:     []NameValue{},
			Headers:     responseHeaders,
			Content: Content{
				Size:     s.BytesIn,
				MimeType: mimeType,
			},
			RedirectURL: "",
			HeadersSize: -1,
			BodySize:    s.BytesIn,
		},
		Cache:   Cache{},
		Timings: timings,
	}

	ext := map[string]interface{}{}
	if s.RequestKind != "" {
		ext["requestKind"] = s.RequestKind
	}
	// Upstream context — useful when an analyst wants to answer "was the
	// wait the origin's fault or go-proxy's processing / fault-injection
	// delay?", or "did the proxy rewrite the variant URL before fetching?".
	// Includes the resolved upstream URL (often differs from the player's
	// URL after proxy rewriting) and the upstream phase timings.
	hasUpstream := s.UpstreamURL != "" && s.UpstreamURL != s.URL ||
		s.DNSMs > 0 || s.ConnectMs > 0 || s.TLSMs > 0 || s.TTFBMs > 0
	if hasUpstream {
		upstream := map[string]interface{}{
			"dns_ms":     s.DNSMs,
			"connect_ms": s.ConnectMs,
			"tls_ms":     s.TLSMs,
			"ttfb_ms":    s.TTFBMs,
		}
		if s.UpstreamURL != "" && s.UpstreamURL != s.URL {
			upstream["url"] = s.UpstreamURL
		}
		ext["upstream"] = upstream
	}
	if s.Faulted {
		fault := map[string]interface{}{
			"faulted": true,
		}
		if s.FaultType != "" {
			fault["type"] = s.FaultType
		}
		if s.FaultAction != "" {
			fault["action"] = s.FaultAction
		}
		if s.FaultCategory != "" {
			fault["category"] = s.FaultCategory
		}
		ext["fault"] = fault
	}
	if len(ext) > 0 {
		entry.Extensions = ext
	}
	return entry
}

func msOrNeg(v float64) float64 {
	if v <= 0 {
		return -1
	}
	return v
}

func defaultStr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func statusTextFor(status int) string {
	switch status {
	case 0:
		return ""
	case 200:
		return "OK"
	case 204:
		return "No Content"
	case 206:
		return "Partial Content"
	case 301:
		return "Moved Permanently"
	case 302:
		return "Found"
	case 304:
		return "Not Modified"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 408:
		return "Request Timeout"
	case 429:
		return "Too Many Requests"
	case 500:
		return "Internal Server Error"
	case 502:
		return "Bad Gateway"
	case 503:
		return "Service Unavailable"
	case 504:
		return "Gateway Timeout"
	}
	if status >= 200 && status < 300 {
		return "OK"
	}
	if status >= 400 && status < 500 {
		return "Client Error"
	}
	if status >= 500 {
		return "Server Error"
	}
	return ""
}
