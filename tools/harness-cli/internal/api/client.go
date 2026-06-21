// Package api is the hand-written facade over the codegen'd v2 clients
// (internal/v2gen/{proxy,forwarder}). It exists because oapi-codegen
// produces verbose method names + raw *http.Response returns:
//
//	resp, err := c.proxy.GetApiV2Players(ctx, nil)
//	defer resp.Body.Close()
//	var out []proxy.PlayerRecord
//	json.NewDecoder(resp.Body).Decode(&out)
//
// vs what callers want:
//
//	players, err := c.Players(ctx)
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

	"github.com/google/uuid"
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
	// Forwarder lives behind `/analytics/` at the edge — nginx strips
	// the prefix before proxying. Wire the generated client's Server
	// to <base>/analytics so all its typed /api/v2/* calls land at
	// /analytics/api/v2/* externally. (sse.go does the same trick
	// by hand for the SSE path; this generalises it.)
	fc, err := forwarder.NewClient(base+"/analytics", forwarder.WithHTTPClient(httpClient), forwarder.WithRequestEditorFn(asForwarderEditor(editor)))
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

// ----- Info ---------------------------------------------------------------

// ProxyInfo is the subset of GET /api/v2/info fields the CLI consumes.
// The oapigen-typed Info schema doesn't carry default_rate_mbps yet
// (proxy ships it via an anonymous-embed wrapper — see #480), so we
// decode the response loosely. Extend this struct as the typed Info
// gains fields the CLI cares about.
type ProxyInfo struct {
	DefaultRateMbps int    `json:"default_rate_mbps"`
	Version         string `json:"version,omitempty"`
}

// Info fetches GET /api/v2/info and returns the parsed subset. Errors
// from the underlying GetApiV2Info call surface unchanged; a 4xx/5xx
// becomes a checkProxyError. DefaultRateMbps==0 means "deployment is
// unlimited" (no baseline cap applied to new sessions).
func (c *Client) Info(ctx context.Context) (ProxyInfo, error) {
	var out ProxyInfo
	resp, err := c.proxy.GetApiV2Info(ctx)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if err := checkProxyError(resp, "GET /api/v2/info"); err != nil {
		return out, err
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("decode info: %w", err)
	}
	return out, nil
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

// patchETagMaxAttempts bounds the read-modify-write retry loop in
// patchWithETagRetry. 5 attempts with the staged backoff below tolerates a
// busy doc (N sims heartbeating + group fan-out) without hanging on a truly
// stuck PATCH.
const patchETagMaxAttempts = 5

// patchWithETagRetry runs a read-modify-write PATCH under If-Match, retrying
// on 412 (precondition failed). The player doc's control_revision bumps on
// every heartbeat — and a born-group has N sims heartbeating PLUS the proxy's
// group fan-out cross-writing every member — so a single etag read routinely
// goes stale between the fetch and the PATCH. The server explicitly tells us
// to "refetch the conflicting paths and retry"; this loop does exactly that:
// a FRESH etag every attempt (via preMutate / Player) and a short staged
// backoff so the concurrent writer settles. do() performs ONE PATCH attempt
// with the supplied etag and returns the raw response; snapshotPatch is the
// wire body recorded for undo.
func (c *Client) patchWithETagRetry(
	ctx context.Context,
	playerID, action, errCtx string,
	snapshotPatch any,
	do func(etag string) (*http.Response, error),
) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= patchETagMaxAttempts; attempt++ {
		before, etag, err := c.preMutate(ctx, playerID, action)
		if err != nil {
			return "", err
		}
		if etag == "" {
			_, e, perr := c.Player(ctx, playerID)
			if perr != nil {
				return "", perr
			}
			etag = e
		}
		resp, err := do(etag)
		if err != nil {
			return "", err
		}
		if resp.StatusCode == http.StatusPreconditionFailed {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 32*1024))
			resp.Body.Close()
			lastErr = fmt.Errorf("%s: 412 revision conflict — doc moved under us (attempt %d/%d)",
				errCtx, attempt, patchETagMaxAttempts)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt) * 75 * time.Millisecond):
			}
			continue
		}
		if cerr := checkProxyError(resp, errCtx); cerr != nil {
			resp.Body.Close()
			return "", cerr
		}
		newETag := strings.Trim(resp.Header.Get("ETag"), `"`)
		resp.Body.Close()
		if err := c.postMutate(playerID, action, etag, newETag, before, snapshotPatch); err != nil {
			return newETag, err
		}
		return newETag, nil
	}
	return "", lastErr
}

// PatchPlayer applies a JSON-merge-patch to a player record using
// If-Match (retrying on 412 via patchWithETagRetry). The etag is fetched
// automatically if needed (or as part of snapshot prep). Action is a short
// label written into the snapshot for replay; empty disables snapshotting.
func (c *Client) PatchPlayer(ctx context.Context, playerID, action string, patch proxy.PlayerPatch) (string, error) {
	return c.patchWithETagRetry(ctx, playerID, action,
		"PATCH /api/v2/players/"+playerID, patch,
		func(etag string) (*http.Response, error) {
			params := &proxy.PatchApiV2PlayersPlayerIdParams{IfMatch: quoteETag(etag)}
			return c.proxy.PatchApiV2PlayersPlayerIdWithApplicationMergePatchPlusJSONBody(
				ctx, proxy.PlayerId(playerID), params, patch,
			)
		})
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
	return c.patchWithETagRetry(ctx, playerID, action,
		"PATCH /api/v2/players/"+playerID+" (clear shape)", map[string]any{"shape": nil},
		func(etag string) (*http.Response, error) {
			params := &proxy.PatchApiV2PlayersPlayerIdParams{IfMatch: quoteETag(etag)}
			return c.proxy.PatchApiV2PlayersPlayerIdWithBody(
				ctx, proxy.PlayerId(playerID), params,
				"application/merge-patch+json", bytes.NewReader([]byte(`{"shape": null}`)),
			)
		})
}

// ResetSession clears ALL server-side per-session settings to default in one
// atomic merge-patch: shape (rate/delay/loss/pattern/transport_fault), HTTP fault
// rules (error/hang/corrupt injection), and content (master-playlist) mutations.
// Gives a player_id a known-clean proxy baseline before a test so nothing carries
// over from a prior run that reused the same player_id — config-on-connect (#712)
// drops proxy.* args on reattach, so it can't self-clear. Client-side per-play
// config is reset separately by the app's reset_advanced sentinel.
func (c *Client) ResetSession(ctx context.Context, playerID, action string) (string, error) {
	const body = `{"shape": null, "fault_rules": [], "content": null}`
	return c.patchWithETagRetry(ctx, playerID, action,
		"PATCH /api/v2/players/"+playerID+" (reset session)",
		map[string]any{"shape": nil, "fault_rules": []any{}, "content": nil},
		func(etag string) (*http.Response, error) {
			params := &proxy.PatchApiV2PlayersPlayerIdParams{IfMatch: quoteETag(etag)}
			return c.proxy.PatchApiV2PlayersPlayerIdWithBody(
				ctx, proxy.PlayerId(playerID), params,
				"application/merge-patch+json", bytes.NewReader([]byte(body)),
			)
		})
}

// PatchShapeMap PATCHes the player's shape with an arbitrary map body,
// allowing explicit nulls (e.g. `{"pattern": null}`) that the typed
// `*Pattern` struct field can't express because of `omitempty` JSON
// tags. Use this when setRate / setDelay / setLoss needs to disarm an
// active pattern: the typed PatchShape path would just drop the nil
// pointer and leave the pattern running. Same body-reader path
// ClearShape uses, just with a richer payload.
func (c *Client) PatchShapeMap(ctx context.Context, playerID, action string, shape map[string]any) (string, error) {
	body, err := json.Marshal(map[string]any{"shape": shape})
	if err != nil {
		return "", err
	}
	return c.patchWithETagRetry(ctx, playerID, action,
		"PATCH /api/v2/players/"+playerID+" (shape map)", map[string]any{"shape": shape},
		func(etag string) (*http.Response, error) {
			params := &proxy.PatchApiV2PlayersPlayerIdParams{IfMatch: quoteETag(etag)}
			return c.proxy.PatchApiV2PlayersPlayerIdWithBody(
				ctx, proxy.PlayerId(playerID), params,
				"application/merge-patch+json", bytes.NewReader(body),
			)
		})
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
		beforeJSON, err := json.Marshal(players)
		if err != nil {
			return fmt.Errorf("snapshot: marshal players: %w", err)
		}
		snap := snapshot.Snapshot{
			PlayerID: "(all)",
			Action:   action,
			Before:   beforeJSON,
			Patch:    json.RawMessage(`{"op":"delete-all"}`),
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

// ----- Play-scoped mutations ----------------------------------------------

// Play returns one PlayRecord by play_id (proxy-side live read). Use
// ArchivePlay (forwarder) for historical reads.
func (c *Client) Play(ctx context.Context, playID string) (*proxy.PlayRecord, string, error) {
	uid, err := uuid.Parse(playID)
	if err != nil {
		return nil, "", fmt.Errorf("invalid play_id %q: %w", playID, err)
	}
	resp, err := c.proxy.GetApiV2PlaysPlayId(ctx, uid)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if err := checkProxyError(resp, "GET /api/v2/plays/"+playID); err != nil {
		return nil, "", err
	}
	var rec proxy.PlayRecord
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		return nil, "", fmt.Errorf("decode play %s: %w", playID, err)
	}
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	return &rec, etag, nil
}

// PatchPlay applies a play-scoped merge-patch. Snapshots like other
// mutations when action is non-empty.
func (c *Client) PatchPlay(ctx context.Context, playID, action string, body []byte) (string, error) {
	rec, etag, err := c.Play(ctx, playID)
	if err != nil {
		return "", err
	}
	uid, err := uuid.Parse(playID)
	if err != nil {
		return "", err
	}
	params := &proxy.PatchApiV2PlaysPlayIdParams{IfMatch: quoteETag(etag)}
	resp, err := c.proxy.PatchApiV2PlaysPlayIdWithBody(
		ctx, uid, params,
		"application/merge-patch+json", bytes.NewReader(body),
	)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := checkProxyError(resp, "PATCH /api/v2/plays/"+playID); err != nil {
		return "", err
	}
	newETag := strings.Trim(resp.Header.Get("ETag"), `"`)
	if c.Snap != nil && action != "" {
		beforeJSON, err := json.Marshal(rec)
		if err != nil {
			return newETag, fmt.Errorf("snapshot: marshal play: %w", err)
		}
		if _, err := c.Snap.Save(snapshot.Snapshot{
			PlayerID:   rec.PlayerId.String(),
			Action:     action + " play=" + playID,
			EtagBefore: etag,
			EtagAfter:  newETag,
			Before:     beforeJSON,
			Patch:      body,
		}); err != nil {
			return newETag, err
		}
	}
	return newETag, nil
}

// ----- Network (live HAR) -------------------------------------------------

// PlayerNetwork returns the live HAR-shaped network log for one
// player. The raw JSON page envelope is decoded as-is so callers
// don't need to know the entire pagination contract — the bytes are
// the bytes.
func (c *Client) PlayerNetwork(ctx context.Context, playerID string, params *proxy.GetApiV2PlayersPlayerIdNetworkParams) ([]byte, error) {
	resp, err := c.proxy.GetApiV2PlayersPlayerIdNetwork(ctx, proxy.PlayerId(playerID), params)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkProxyError(resp, "GET /api/v2/players/"+playerID+"/network"); err != nil {
		return nil, err
	}
	return io.ReadAll(resp.Body)
}

// ----- Archive reads (forwarder) ------------------------------------------

// archiveGET is a thin wrapper that runs the supplied forwarder
// closure and reads the body. Used by every archive command — those
// commands care about presenting bytes, not interpreting them
// (formatter lives in cmd/harness/archive.go).
func (c *Client) archiveGET(call func() (*http.Response, error), ctx string) ([]byte, error) {
	resp, err := call()
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
		return nil, fmt.Errorf("%s: %d: %s", ctx, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return io.ReadAll(resp.Body)
}

// ArchivePlays lists ended plays from the forwarder archive.
func (c *Client) ArchivePlays(ctx context.Context, params *forwarder.GetApiV2PlaysParams) ([]byte, error) {
	return c.archiveGET(func() (*http.Response, error) {
		return c.forwarder.GetApiV2Plays(ctx, params)
	}, "GET /analytics/api/v2/plays")
}

// ArchivePlay fetches a single archived play with its _links bundle.
func (c *Client) ArchivePlay(ctx context.Context, playID string) ([]byte, error) {
	uid, err := uuid.Parse(playID)
	if err != nil {
		return nil, fmt.Errorf("invalid play_id %q: %w", playID, err)
	}
	return c.archiveGET(func() (*http.Response, error) {
		return c.forwarder.GetApiV2PlaysPlayId(ctx, uid)
	}, "GET /analytics/api/v2/plays/"+playID)
}

// ArchivePlaysAggregate runs aggregate stats across plays.
func (c *Client) ArchivePlaysAggregate(ctx context.Context, params *forwarder.GetApiV2PlaysAggregateParams) ([]byte, error) {
	return c.archiveGET(func() (*http.Response, error) {
		return c.forwarder.GetApiV2PlaysAggregate(ctx, params)
	}, "GET /analytics/api/v2/plays/aggregate")
}

// ArchiveEvents returns the session_events rows for one play.
// (Renamed from ArchiveSnapshots in v2.0.0 — the pre-#472 alias
// /api/v2/snapshots was retired.)
func (c *Client) ArchiveEvents(ctx context.Context, params *forwarder.GetApiV2EventsParams) ([]byte, error) {
	return c.archiveGET(func() (*http.Response, error) {
		return c.forwarder.GetApiV2Events(ctx, params)
	}, "GET /analytics/api/v2/events")
}

// ArchiveNetworkRequests returns the network_requests rows.
func (c *Client) ArchiveNetworkRequests(ctx context.Context, params *forwarder.GetApiV2NetworkRequestsParams) ([]byte, error) {
	return c.archiveGET(func() (*http.Response, error) {
		return c.forwarder.GetApiV2NetworkRequests(ctx, params)
	}, "GET /analytics/api/v2/network_requests")
}

// ArchiveControlEvents returns the proxy/harness action log
// (control_events). Issue #474 Milestone B replacement for the retired
// ArchiveSessionEvents (session_markers).
func (c *Client) ArchiveControlEvents(ctx context.Context, params *forwarder.GetApiV2ControlEventsParams) ([]byte, error) {
	return c.archiveGET(func() (*http.Response, error) {
		return c.forwarder.GetApiV2ControlEvents(ctx, params)
	}, "GET /analytics/api/v2/control_events")
}

// ArchiveAVMetricEvents returns the iOS AVMetrics event log
// (ios_avmetric_events) — the highest-resolution failure-timing feed.
// Bounded read, so it closes (no SSE --max-time hack). Issue #693.
func (c *Client) ArchiveAVMetricEvents(ctx context.Context, params *forwarder.GetApiV2AvmetricEventsParams) ([]byte, error) {
	return c.archiveGET(func() (*http.Response, error) {
		return c.forwarder.GetApiV2AvmetricEvents(ctx, params)
	}, "GET /analytics/api/v2/avmetric_events")
}

// ArchiveSessionHeatmap returns the bucketed heatmap.
func (c *Client) ArchiveSessionHeatmap(ctx context.Context, params *forwarder.GetApiV2SessionHeatmapParams) ([]byte, error) {
	return c.archiveGET(func() (*http.Response, error) {
		return c.forwarder.GetApiV2SessionHeatmap(ctx, params)
	}, "GET /analytics/api/v2/session_heatmap")
}

// ArchivePlayBundle streams the ZIP bundle for a play. Returns the
// open response body so callers can copy it to a file without
// buffering. Caller MUST close the returned ReadCloser.
func (c *Client) ArchivePlayBundle(ctx context.Context, playID string) (io.ReadCloser, error) {
	uid, err := uuid.Parse(playID)
	if err != nil {
		return nil, fmt.Errorf("invalid play_id %q: %w", playID, err)
	}
	resp, err := c.forwarder.GetApiV2PlaysPlayIdBundle(ctx, uid)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		resp.Body.Close()
		return nil, fmt.Errorf("GET bundle: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp.Body, nil
}

// ArchiveBundles returns the bundle catalogue (debug — Phase 7).
func (c *Client) ArchiveBundles(ctx context.Context) ([]byte, error) {
	return c.archiveGET(func() (*http.Response, error) {
		return c.forwarder.GetBundles(ctx)
	}, "GET /analytics/api/v2/bundles")
}

// ----- Player groups ------------------------------------------------------

// Groups returns all player groups. Wraps GET /api/v2/player-groups
// + un-wraps the {items:[…]} list envelope.
func (c *Client) Groups(ctx context.Context) ([]proxy.PlayerGroup, error) {
	resp, err := c.proxy.GetApiV2PlayerGroups(ctx)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkProxyError(resp, "GET /api/v2/player-groups"); err != nil {
		return nil, err
	}
	var page struct {
		Items []proxy.PlayerGroup `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, err
	}
	return page.Items, nil
}

// Group fetches one group + its ETag (for the next PATCH).
func (c *Client) Group(ctx context.Context, groupID string) (*proxy.PlayerGroup, string, error) {
	uid, err := uuid.Parse(groupID)
	if err != nil {
		return nil, "", fmt.Errorf("invalid group_id %q: %w", groupID, err)
	}
	resp, err := c.proxy.GetApiV2PlayerGroupsGroupId(ctx, uid)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if err := checkProxyError(resp, "GET /api/v2/player-groups/"+groupID); err != nil {
		return nil, "", err
	}
	var grp proxy.PlayerGroup
	if err := json.NewDecoder(resp.Body).Decode(&grp); err != nil {
		return nil, "", err
	}
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	return &grp, etag, nil
}

// CreateGroup POSTs a new group. Member ids must be valid UUIDs.
func (c *Client) CreateGroup(ctx context.Context, body proxy.PostApiV2PlayerGroupsJSONRequestBody) (*proxy.PlayerGroup, string, error) {
	resp, err := c.proxy.PostApiV2PlayerGroups(ctx, body)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if err := checkProxyError(resp, "POST /api/v2/player-groups"); err != nil {
		return nil, "", err
	}
	var grp proxy.PlayerGroup
	if err := json.NewDecoder(resp.Body).Decode(&grp); err != nil {
		return nil, "", err
	}
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	return &grp, etag, nil
}

// PatchGroup applies a merge-patch (label / labels / member_player_ids).
// Snapshots into the special "(group)" player slot so undo can find it.
func (c *Client) PatchGroup(ctx context.Context, groupID, action string, patch proxy.PlayerGroupPatch) (string, error) {
	uid, err := uuid.Parse(groupID)
	if err != nil {
		return "", fmt.Errorf("invalid group_id %q: %w", groupID, err)
	}
	before, etag, err := c.Group(ctx, groupID)
	if err != nil {
		return "", err
	}
	params := &proxy.PatchApiV2PlayerGroupsGroupIdParams{IfMatch: quoteETag(etag)}
	resp, err := c.proxy.PatchApiV2PlayerGroupsGroupIdWithApplicationMergePatchPlusJSONBody(
		ctx, uid, params, patch,
	)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := checkProxyError(resp, "PATCH /api/v2/player-groups/"+groupID); err != nil {
		return "", err
	}
	newETag := strings.Trim(resp.Header.Get("ETag"), `"`)
	if c.Snap != nil && action != "" {
		beforeJSON, err := json.Marshal(before)
		if err != nil {
			return newETag, fmt.Errorf("snapshot: marshal group: %w", err)
		}
		patchJSON, err := json.Marshal(patch)
		if err != nil {
			return newETag, fmt.Errorf("snapshot: marshal patch: %w", err)
		}
		if _, err := c.Snap.Save(snapshot.Snapshot{
			PlayerID:   "group-" + groupID,
			Action:     action,
			EtagBefore: etag,
			EtagAfter:  newETag,
			Before:     beforeJSON,
			Patch:      patchJSON,
		}); err != nil {
			return newETag, err
		}
	}
	return newETag, nil
}

// DeleteGroup removes a player group. Members stay; only the group
// metadata is deleted.
func (c *Client) DeleteGroup(ctx context.Context, groupID string) error {
	uid, err := uuid.Parse(groupID)
	if err != nil {
		return fmt.Errorf("invalid group_id %q: %w", groupID, err)
	}
	resp, err := c.proxy.DeleteApiV2PlayerGroupsGroupId(ctx, uid)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkProxyError(resp, "DELETE /api/v2/player-groups/"+groupID)
}

// PatchRawWithSnapshot is the snapshot-aware sibling of PatchRaw.
// Use it from typed commands (labels rm, timeouts --clear,
// content --clear) that need raw-body merge-patch but must still
// participate in 'harness undo'. Empty action disables snapshotting
// (matches the convention used by the typed mutation methods).
func (c *Client) PatchRawWithSnapshot(ctx context.Context, playerID, action string, body []byte) (string, error) {
	before, etag, err := c.preMutate(ctx, playerID, action)
	if err != nil {
		return "", err
	}
	newETag, err := c.PatchRaw(ctx, playerID, etag, body)
	if err != nil {
		return newETag, err
	}
	if err := c.postMutate(playerID, action, etag, newETag, before, json.RawMessage(body)); err != nil {
		return newETag, err
	}
	return newETag, nil
}

// PatchRaw is the universal escape hatch — sends a hand-crafted
// merge-patch+json body straight at PATCH /api/v2/players/{id}.
// Used by `harness undo` (replay) and `harness raw` (Phase 7). The
// caller is responsible for body shape; this method does not
// snapshot (the snapshot machinery would be circular for undo, and
// `raw` is a debug escape hatch that wants to opt out). Typed
// commands that need raw bodies should use PatchRawWithSnapshot.
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
