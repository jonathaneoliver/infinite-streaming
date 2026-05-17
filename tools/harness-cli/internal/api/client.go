// Package api is the hand-written facade over the codegen'd v2 clients
// (internal/v2gen/{proxy,forwarder}). It exists because oapi-codegen
// produces verbose method names + raw *http.Response returns:
//
//   resp, err := c.proxy.GetApiV2Players(ctx, nil)
//   defer resp.Body.Close()
//   var out []proxy.PlayerRecord
//   json.NewDecoder(resp.Body).Decode(&out)
//
// vs what callers want:
//
//   players, err := c.Players(ctx)
//
// The facade also owns:
//   - the HTTP client (timeouts, optional self-signed-cert tolerance)
//   - ETag concurrency (read-then-PATCH-with-If-Match, 412-retry-once)
//   - error-envelope decoding (proxy ProblemDetails → Go error)
//   - basic auth wiring from $HARNESS_BASIC_AUTH
//
// Everything in internal/v2gen/ is generated and rewritten on every
// `make gen-harness-cli-client`; everything here is hand-written and
// stable across spec edits.
package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/forwarder"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/proxy"
)

// DefaultBaseURL points at test-dev. Override via Options.BaseURL or
// the $HARNESS_BASE_URL env var; the CLI's --base flag flows through
// to here.
const DefaultBaseURL = "https://jonathanoliver-ubuntu.local:21000"

// Options bundle the per-process knobs callers can twiddle. Zero values
// are sensible defaults (test-dev, 30 s HTTP timeout, no TLS skip).
type Options struct {
	BaseURL string
	// Insecure skips TLS verification. test-dev runs HTTPS with a
	// self-signed cert; without --insecure (or HARNESS_INSECURE=1)
	// every call surfaces an x509 error.
	Insecure bool
	// BasicAuth is "user:password" if HTTP Basic is enabled on the
	// target; empty disables. Mirrors the dashboard convention.
	BasicAuth string
	// Timeout caps individual requests. SSE subscriptions (later) will
	// use their own context, not this timeout.
	Timeout time.Duration
}

// Client wraps both v2 clients (proxy + forwarder) so commands don't
// have to think about which one to invoke for which endpoint — the
// facade method names tell you (Players, Plays, Snapshots, etc.).
type Client struct {
	BaseURL   string
	HTTP      *http.Client
	BasicAuth string

	proxy     *proxy.Client
	forwarder *forwarder.Client
}

// New builds a Client from Options. Reads $HARNESS_BASE_URL,
// $HARNESS_INSECURE, $HARNESS_BASIC_AUTH as fallbacks. Returns an
// error only on configuration that fails up-front (invalid URL); a
// bad cert / 401 surfaces later on the first call.
func New(opts Options) (*Client, error) {
	if opts.BaseURL == "" {
		opts.BaseURL = os.Getenv("HARNESS_BASE_URL")
	}
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	if !opts.Insecure && os.Getenv("HARNESS_INSECURE") != "" {
		opts.Insecure = true
	}
	if opts.BasicAuth == "" {
		opts.BasicAuth = os.Getenv("HARNESS_BASIC_AUTH")
	}
	if opts.Timeout == 0 {
		opts.Timeout = 30 * time.Second
	}

	base := strings.TrimRight(opts.BaseURL, "/")

	httpClient := &http.Client{Timeout: opts.Timeout}
	if opts.Insecure {
		httpClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	editor := makeAuthEditor(opts.BasicAuth)
	editors := []proxy.ClientOption{
		proxy.WithHTTPClient(httpClient),
		proxy.WithRequestEditorFn(asProxyEditor(editor)),
	}
	pc, err := proxy.NewClient(base, editors...)
	if err != nil {
		return nil, fmt.Errorf("proxy client: %w", err)
	}
	fc, err := forwarder.NewClient(base, forwarder.WithHTTPClient(httpClient), forwarder.WithRequestEditorFn(asForwarderEditor(editor)))
	if err != nil {
		return nil, fmt.Errorf("forwarder client: %w", err)
	}
	return &Client{
		BaseURL:   base,
		HTTP:      httpClient,
		BasicAuth: opts.BasicAuth,
		proxy:     pc,
		forwarder: fc,
	}, nil
}

// reqEditor is the protocol-agnostic signature used by both generated
// clients (proxy and forwarder define this same shape but in their own
// packages, so we keep a separate type and adapt at the call site).
type reqEditor func(ctx context.Context, req *http.Request) error

func makeAuthEditor(basicAuth string) reqEditor {
	if basicAuth == "" {
		return nil
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(basicAuth))
	header := "Basic " + encoded
	return func(_ context.Context, req *http.Request) error {
		// Don't clobber a caller-set header (e.g. a future per-user
		// override). Basic auth at the client level is the default.
		if req.Header.Get("Authorization") == "" {
			req.Header.Set("Authorization", header)
		}
		return nil
	}
}

func asProxyEditor(e reqEditor) proxy.RequestEditorFn {
	if e == nil {
		return func(ctx context.Context, req *http.Request) error { return nil }
	}
	return proxy.RequestEditorFn(e)
}

func asForwarderEditor(e reqEditor) forwarder.RequestEditorFn {
	if e == nil {
		return func(ctx context.Context, req *http.Request) error { return nil }
	}
	return forwarder.RequestEditorFn(e)
}

// ----- Players ------------------------------------------------------------

// Players returns the current set of v2 player records. Wraps
// GET /api/v2/players + JSON decode. The wire shape is the v2
// list-page envelope `{items: [...]}`; this method un-wraps it so
// callers see a plain slice.
func (c *Client) Players(ctx context.Context) ([]proxy.PlayerRecord, error) {
	resp, err := c.proxy.GetApiV2Players(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkProxyError(resp, "GET /api/v2/players"); err != nil {
		return nil, err
	}
	var page struct {
		Items []proxy.PlayerRecord `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, fmt.Errorf("decode players: %w", err)
	}
	return page.Items, nil
}

// Player returns a single PlayerRecord plus the response's ETag (for
// future PATCH callers that need If-Match). ETag is the server's
// strongly-typed control_revision; an empty string means the server
// didn't emit one (shouldn't happen on v2 but guard anyway).
func (c *Client) Player(ctx context.Context, playerID string) (*proxy.PlayerRecord, string, error) {
	resp, err := c.proxy.GetApiV2PlayersPlayerId(ctx, proxy.PlayerId(playerID))
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if err := checkProxyError(resp, "GET /api/v2/players/"+playerID); err != nil {
		return nil, "", err
	}
	var rec proxy.PlayerRecord
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		return nil, "", fmt.Errorf("decode player %s: %w", playerID, err)
	}
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	return &rec, etag, nil
}

// ----- Mutations ----------------------------------------------------------

// PatchPlayer applies a JSON-merge-patch to a player record using
// If-Match. If etag is empty we fetch one first (cost of an extra GET
// in exchange for a one-call signature for ad-hoc CLI use). Returns
// the new ETag after the PATCH.
func (c *Client) PatchPlayer(ctx context.Context, playerID, etag string, patch proxy.PlayerPatch) (string, error) {
	if etag == "" {
		_, e, err := c.Player(ctx, playerID)
		if err != nil {
			return "", err
		}
		etag = e
	}
	params := &proxy.PatchApiV2PlayersPlayerIdParams{IfMatch: quoteETag(etag)}
	resp, err := c.proxy.PatchApiV2PlayersPlayerIdWithApplicationMergePatchPlusJSONBody(
		ctx, proxy.PlayerId(playerID), params, patch,
	)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := checkProxyError(resp, "PATCH /api/v2/players/"+playerID); err != nil {
		return "", err
	}
	return strings.Trim(resp.Header.Get("ETag"), `"`), nil
}

// AddFaultRule POSTs a new fault rule onto a player. The proxy
// generates the rule_id if rule.Id is unset. Returns the new ETag.
func (c *Client) AddFaultRule(ctx context.Context, playerID, etag string, rule proxy.FaultRule) (string, error) {
	if etag == "" {
		_, e, err := c.Player(ctx, playerID)
		if err != nil {
			return "", err
		}
		etag = e
	}
	params := &proxy.PostApiV2PlayersPlayerIdFaultRulesParams{IfMatch: quoteETag(etag)}
	resp, err := c.proxy.PostApiV2PlayersPlayerIdFaultRules(ctx, proxy.PlayerId(playerID), params, rule)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := checkProxyError(resp, "POST /api/v2/players/"+playerID+"/fault_rules"); err != nil {
		return "", err
	}
	return strings.Trim(resp.Header.Get("ETag"), `"`), nil
}

// DeleteFaultRule removes a single fault rule by rule_id.
func (c *Client) DeleteFaultRule(ctx context.Context, playerID, ruleID, etag string) (string, error) {
	if etag == "" {
		_, e, err := c.Player(ctx, playerID)
		if err != nil {
			return "", err
		}
		etag = e
	}
	params := &proxy.DeleteApiV2PlayersPlayerIdFaultRulesRuleIdParams{IfMatch: quoteETag(etag)}
	resp, err := c.proxy.DeleteApiV2PlayersPlayerIdFaultRulesRuleId(
		ctx, proxy.PlayerId(playerID), proxy.RuleId(ruleID), params,
	)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := checkProxyError(resp, "DELETE /api/v2/players/"+playerID+"/fault_rules/"+ruleID); err != nil {
		return "", err
	}
	return strings.Trim(resp.Header.Get("ETag"), `"`), nil
}

// ClearFaultRules drops all fault rules in one PATCH by sending an
// empty array. Equivalent to N deletes but atomic w.r.t. ETag.
func (c *Client) ClearFaultRules(ctx context.Context, playerID, etag string) (string, error) {
	empty := []proxy.FaultRule{}
	return c.PatchPlayer(ctx, playerID, etag, proxy.PlayerPatch{FaultRules: &empty})
}

// PatchShape PATCHes player.shape only. A nil pointer omits the key
// (no-op for shape); use ClearShape to send the explicit-null body
// that clears all shaping server-side.
func (c *Client) PatchShape(ctx context.Context, playerID, etag string, shape *proxy.Shape) (string, error) {
	return c.PatchPlayer(ctx, playerID, etag, proxy.PlayerPatch{Shape: shape})
}

// ClearShape sends `{"shape": null}` — the merge-patch sentinel that
// removes all kernel shaping in one PATCH. Can't be expressed through
// the generated typed body (PlayerPatch.Shape is `*Shape`, so nil
// means "omit the key"), so we ship the raw JSON via the body-reader
// variant of the generated client.
func (c *Client) ClearShape(ctx context.Context, playerID, etag string) (string, error) {
	if etag == "" {
		_, e, err := c.Player(ctx, playerID)
		if err != nil {
			return "", err
		}
		etag = e
	}
	params := &proxy.PatchApiV2PlayersPlayerIdParams{IfMatch: quoteETag(etag)}
	body := bytes.NewReader([]byte(`{"shape": null}`))
	resp, err := c.proxy.PatchApiV2PlayersPlayerIdWithBody(
		ctx, proxy.PlayerId(playerID), params,
		"application/merge-patch+json", body,
	)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := checkProxyError(resp, "PATCH /api/v2/players/"+playerID+" (clear shape)"); err != nil {
		return "", err
	}
	return strings.Trim(resp.Header.Get("ETag"), `"`), nil
}

// quoteETag wraps a raw revision token in the literal double-quotes
// required by RFC 7232 If-Match. No-ops if already wrapped.
func quoteETag(rev string) string {
	if rev == "" {
		return ""
	}
	if strings.HasPrefix(rev, `"`) && strings.HasSuffix(rev, `"`) {
		return rev
	}
	return `"` + rev + `"`
}

// ----- Error envelope -----------------------------------------------------

// checkProxyError converts a non-2xx response to a Go error, attempting
// to decode the ProblemDetails envelope for nicer messages. Always
// closes the body before returning, so callers can ignore the body on
// error returns.
func checkProxyError(resp *http.Response, ctx string) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
	var pd proxy.ProblemDetails
	if json.Unmarshal(body, &pd) == nil && pd.Title != "" {
		detail := ""
		if pd.Detail != nil && *pd.Detail != "" {
			detail = ": " + *pd.Detail
		}
		return fmt.Errorf("%s: %d %s%s", ctx, resp.StatusCode, pd.Title, detail)
	}
	return fmt.Errorf("%s: %d %s", ctx, resp.StatusCode, strings.TrimSpace(string(body)))
}
