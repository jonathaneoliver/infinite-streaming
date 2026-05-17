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

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/snapshot"
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
	// Snap, when non-nil, enables snapshot-before-mutate. The CLI's
	// main() opens a Store from the working tree's basename so
	// snapshots live under ~/.claude/state/harness/<repo>/.
	Snap *snapshot.Store
}

// Client wraps both v2 clients (proxy + forwarder) so commands don't
// have to think about which one to invoke for which endpoint — the
// facade method names tell you (Players, Plays, Snapshots, etc.).
type Client struct {
	BaseURL   string
	HTTP      *http.Client
	BasicAuth string

	// Snap is optional. When non-nil and a mutation method is called
	// with a non-empty `action` argument, the facade fetches the
	// player, runs the mutation, and writes a Snapshot covering the
	// before-state + the wire patch. `harness undo` consumes these.
	Snap *snapshot.Store

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
		Snap:      opts.Snap,
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

// preMutate captures the player's before-state when snapshot is on.
// Returns (record, etag) — both empty when Snap is nil or action is
// empty. Errors propagate to the caller so a snapshot failure prevents
// the mutation (per the no-silent-failure design).
func (c *Client) preMutate(ctx context.Context, playerID, action string) (*proxy.PlayerRecord, string, error) {
	if c.Snap == nil || action == "" {
		return nil, "", nil
	}
	rec, etag, err := c.Player(ctx, playerID)
	if err != nil {
		return nil, "", err
	}
	return rec, etag, nil
}

// postMutate writes the snapshot file when snapshot is on. Patch is
// the wire body that was just sent (any JSON-encodable struct). A
// snapshot write error is *surfaced* (returned) — if the mutation
// landed but the snapshot didn't, the operator should know so they
// can decide whether to re-run; pretending otherwise would silently
// erode undo coverage.
func (c *Client) postMutate(playerID, action, etagBefore, etagAfter string, before *proxy.PlayerRecord, patch any) error {
	if c.Snap == nil || action == "" {
		return nil
	}
	beforeJSON, err := json.Marshal(before)
	if err != nil {
		return fmt.Errorf("snapshot: marshal before: %w", err)
	}
	patchJSON, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("snapshot: marshal patch: %w", err)
	}
	snap := snapshot.Snapshot{
		PlayerID:   playerID,
		Action:     action,
		EtagBefore: etagBefore,
		EtagAfter:  etagAfter,
		Before:     beforeJSON,
		Patch:      patchJSON,
	}
	if _, err := c.Snap.Save(snap); err != nil {
		return err
	}
	return nil
}

// PatchPlayer applies a JSON-merge-patch to a player record using
// If-Match. The etag is fetched automatically if needed (or as part
// of snapshot prep). Action is a short label written into the
// snapshot for replay; empty disables snapshotting.
func (c *Client) PatchPlayer(ctx context.Context, playerID, action string, patch proxy.PlayerPatch) (string, error) {
	before, etag, err := c.preMutate(ctx, playerID, action)
	if err != nil {
		return "", err
	}
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
	newETag := strings.Trim(resp.Header.Get("ETag"), `"`)
	if err := c.postMutate(playerID, action, etag, newETag, before, patch); err != nil {
		return newETag, err
	}
	return newETag, nil
}

// AddFaultRule POSTs a new fault rule onto a player. The proxy
// generates the rule_id if rule.Id is unset.
func (c *Client) AddFaultRule(ctx context.Context, playerID, action string, rule proxy.FaultRule) (string, error) {
	before, etag, err := c.preMutate(ctx, playerID, action)
	if err != nil {
		return "", err
	}
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
	newETag := strings.Trim(resp.Header.Get("ETag"), `"`)
	if err := c.postMutate(playerID, action, etag, newETag, before, rule); err != nil {
		return newETag, err
	}
	return newETag, nil
}

// DeleteFaultRule removes a single fault rule by rule_id.
func (c *Client) DeleteFaultRule(ctx context.Context, playerID, ruleID, action string) (string, error) {
	before, etag, err := c.preMutate(ctx, playerID, action)
	if err != nil {
		return "", err
	}
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
	newETag := strings.Trim(resp.Header.Get("ETag"), `"`)
	if err := c.postMutate(playerID, action, etag, newETag, before, map[string]string{"deleted_rule_id": ruleID}); err != nil {
		return newETag, err
	}
	return newETag, nil
}

// ClearFaultRules drops all fault rules in one PATCH by sending an
// empty array. Equivalent to N deletes but atomic w.r.t. ETag.
func (c *Client) ClearFaultRules(ctx context.Context, playerID, action string) (string, error) {
	empty := []proxy.FaultRule{}
	return c.PatchPlayer(ctx, playerID, action, proxy.PlayerPatch{FaultRules: &empty})
}

// PatchShape PATCHes player.shape only. A nil pointer omits the key;
// use ClearShape to send the explicit-null merge-patch sentinel.
func (c *Client) PatchShape(ctx context.Context, playerID, action string, shape *proxy.Shape) (string, error) {
	return c.PatchPlayer(ctx, playerID, action, proxy.PlayerPatch{Shape: shape})
}

// ClearShape sends `{"shape": null}` — the merge-patch sentinel that
// removes all kernel shaping in one PATCH. Can't be expressed through
// the generated typed body (PlayerPatch.Shape is `*Shape`, so nil
// means "omit the key"), so we ship the raw JSON via the body-reader
// variant of the generated client.
func (c *Client) ClearShape(ctx context.Context, playerID, action string) (string, error) {
	before, etag, err := c.preMutate(ctx, playerID, action)
	if err != nil {
		return "", err
	}
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
	newETag := strings.Trim(resp.Header.Get("ETag"), `"`)
	if err := c.postMutate(playerID, action, etag, newETag, before, map[string]any{"shape": nil}); err != nil {
		return newETag, err
	}
	return newETag, nil
}

// CreatePlayer POSTs a new player. Does not snapshot (there's no
// before-state) but returns the created record + its initial ETag.
func (c *Client) CreatePlayer(ctx context.Context, req proxy.PlayerCreateRequest) (*proxy.PlayerRecord, string, error) {
	resp, err := c.proxy.PostApiV2Players(ctx, req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if err := checkProxyError(resp, "POST /api/v2/players"); err != nil {
		return nil, "", err
	}
	var rec proxy.PlayerRecord
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		return nil, "", fmt.Errorf("decode created player: %w", err)
	}
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	return &rec, etag, nil
}

// DeletePlayer removes a player. Snapshots first when action != ""
// (so undo can recreate via POST + restore — though that's a Phase 5
// follow-up; today undo of a delete is documented as not-supported).
func (c *Client) DeletePlayer(ctx context.Context, playerID, action string) error {
	before, _, err := c.preMutate(ctx, playerID, action)
	if err != nil {
		return err
	}
	resp, err := c.proxy.DeleteApiV2PlayersPlayerId(ctx, proxy.PlayerId(playerID))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkProxyError(resp, "DELETE /api/v2/players/"+playerID); err != nil {
		return err
	}
	if err := c.postMutate(playerID, action, "", "(deleted)", before, map[string]string{"op": "delete"}); err != nil {
		return err
	}
	return nil
}

// DeleteAllPlayers nukes the entire player list. Snapshots a single
// "before" capturing the prior count; per-player rollback is not
// supported (would need N POSTs with reconstructed labels/shape).
func (c *Client) DeleteAllPlayers(ctx context.Context, action string) error {
	if c.Snap != nil && action != "" {
		players, err := c.Players(ctx)
		if err != nil {
			return err
		}
		beforeJSON, _ := json.Marshal(players)
		snap := snapshot.Snapshot{
			PlayerID:   "(all)",
			Action:     action,
			Before:     beforeJSON,
			Patch:      json.RawMessage(`{"op":"delete-all"}`),
		}
		if _, err := c.Snap.Save(snap); err != nil {
			return err
		}
	}
	resp, err := c.proxy.DeleteApiV2Players(ctx)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkProxyError(resp, "DELETE /api/v2/players"); err != nil {
		return err
	}
	return nil
}

// EditFaultRuleRaw is a partial-update PATCH where the body is a
// hand-built map. Necessary because the typed FaultRule has a
// non-omitempty Type field — sending `{"type": ""}` for an
// edit-frequency-only request fails server-side with 501.
func (c *Client) EditFaultRuleRaw(ctx context.Context, playerID, ruleID, action string, patchMap map[string]any) (string, error) {
	body, err := json.Marshal(patchMap)
	if err != nil {
		return "", err
	}
	before, etag, err := c.preMutate(ctx, playerID, action)
	if err != nil {
		return "", err
	}
	if etag == "" {
		_, e, err := c.Player(ctx, playerID)
		if err != nil {
			return "", err
		}
		etag = e
	}
	params := &proxy.PatchApiV2PlayersPlayerIdFaultRulesRuleIdParams{IfMatch: quoteETag(etag)}
	resp, err := c.proxy.PatchApiV2PlayersPlayerIdFaultRulesRuleIdWithBody(
		ctx, proxy.PlayerId(playerID), proxy.RuleId(ruleID), params,
		"application/merge-patch+json", bytes.NewReader(body),
	)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := checkProxyError(resp, "PATCH /api/v2/players/"+playerID+"/fault_rules/"+ruleID); err != nil {
		return "", err
	}
	newETag := strings.Trim(resp.Header.Get("ETag"), `"`)
	if err := c.postMutate(playerID, action, etag, newETag, before, patchMap); err != nil {
		return newETag, err
	}
	return newETag, nil
}

// EditFaultRule PATCHes one fault rule in place. Used by
// `harness fault edit`.
func (c *Client) EditFaultRule(ctx context.Context, playerID, ruleID, action string, patch proxy.FaultRule) (string, error) {
	before, etag, err := c.preMutate(ctx, playerID, action)
	if err != nil {
		return "", err
	}
	if etag == "" {
		_, e, err := c.Player(ctx, playerID)
		if err != nil {
			return "", err
		}
		etag = e
	}
	params := &proxy.PatchApiV2PlayersPlayerIdFaultRulesRuleIdParams{IfMatch: quoteETag(etag)}
	resp, err := c.proxy.PatchApiV2PlayersPlayerIdFaultRulesRuleIdWithApplicationMergePatchPlusJSONBody(
		ctx, proxy.PlayerId(playerID), proxy.RuleId(ruleID), params, patch,
	)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := checkProxyError(resp, "PATCH /api/v2/players/"+playerID+"/fault_rules/"+ruleID); err != nil {
		return "", err
	}
	newETag := strings.Trim(resp.Header.Get("ETag"), `"`)
	if err := c.postMutate(playerID, action, etag, newETag, before, patch); err != nil {
		return newETag, err
	}
	return newETag, nil
}

// PatchRaw is the universal escape hatch — sends a hand-crafted
// merge-patch+json body straight at PATCH /api/v2/players/{id}.
// Used by `harness undo` (replay) and `harness raw` (Phase 7). The
// caller is responsible for body shape; this method does not
// snapshot (the snapshot machinery would be circular for undo, and
// `raw` is a debug escape hatch that wants to opt out).
func (c *Client) PatchRaw(ctx context.Context, playerID, etag string, body []byte) (string, error) {
	if etag == "" {
		_, e, err := c.Player(ctx, playerID)
		if err != nil {
			return "", err
		}
		etag = e
	}
	params := &proxy.PatchApiV2PlayersPlayerIdParams{IfMatch: quoteETag(etag)}
	resp, err := c.proxy.PatchApiV2PlayersPlayerIdWithBody(
		ctx, proxy.PlayerId(playerID), params,
		"application/merge-patch+json", bytes.NewReader(body),
	)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := checkProxyError(resp, "PATCH /api/v2/players/"+playerID+" (raw)"); err != nil {
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
