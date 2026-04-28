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
type Source struct {
	Timestamp     time.Time
	Method        string
	URL           string
	RequestKind   string
	Status        int
	BytesIn       int64
	BytesOut      int64
	ContentType   string
	DNSMs         float64
	ConnectMs     float64
	TLSMs         float64
	TTFBMs        float64
	TransferMs    float64
	TotalMs       float64
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

// BuildOptions controls extra metadata embedded at the HAR Log level.
type BuildOptions struct {
	SessionID string
	PlayerID  string
	GroupID   string
	Incident  *Incident
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
	if len(ext) > 0 {
		log.Extensions = ext
	}

	return HAR{Log: log}
}

func buildEntry(s Source) Entry {
	startedDate := s.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z")

	timings := Timings{
		Blocked: -1,
		DNS:     msOrNeg(s.DNSMs),
		Connect: msOrNeg(s.ConnectMs),
		Send:    -1,
		Wait:    msOrNeg(s.TTFBMs),
		Receive: msOrNeg(s.TransferMs),
		SSL:     msOrNeg(s.TLSMs),
	}

	statusText := statusTextFor(s.Status)
	mimeType := s.ContentType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	entry := Entry{
		StartedDateTime: startedDate,
		Time:            s.TotalMs,
		Request: Request{
			Method:      defaultStr(s.Method, "GET"),
			URL:         s.URL,
			HTTPVersion: "HTTP/1.1",
			Cookies:     []NameValue{},
			Headers:     []NameValue{},
			QueryString: []NameValue{},
			HeadersSize: -1,
			BodySize:    s.BytesOut,
		},
		Response: Response{
			Status:      s.Status,
			StatusText:  statusText,
			HTTPVersion: "HTTP/1.1",
			Cookies:     []NameValue{},
			Headers:     []NameValue{},
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
