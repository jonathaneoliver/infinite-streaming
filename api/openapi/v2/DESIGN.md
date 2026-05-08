# Harness API v2 — Design

## Goals

A clean rebuild of the harness API around the things users actually model:
**players** (devices) and **plays** (one continuous playback). v1's `session_id`
disappears from the user-facing surface; the proxy keeps it internally for
port-binding, but consumers never see it.

The seven v1 design issues identified in review are resolved together with
three additive features that were impossible without the reshape:
**player/play mutation scoping**, **labels** for cross-cutting tagging
(test runs, CI, branches), and **`play_summaries`** for long-retention
trend analysis.

v1 stays mounted under `/api/...`. v2 lives at `/api/v2/...` until consumers
migrate.

## Non-goals

- **Not** a feature expansion outside what the reshape enables.
- **Not** a rewrite of the proxy internals. Handlers translate v2 requests
  into the same in-memory data structures v1 uses; only the contract changes.
- **Not** a redesign of the live streaming path (`/go-live/*`); that surface
  is unaffected.

## Versioning strategy

- Path-prefixed: `/api/v2/...` and `/analytics/api/v2/...`. Both versions
  served from the same nginx, same binaries.
- v1 paths continue to work unchanged. No `Deprecation:` headers initially —
  added once a migration plan is dated.
- Hand-written specs at `api/openapi/v2/{proxy,forwarder}.yaml`. Code-first
  generation (swag) stays for v1 only — v2's hand-written spec is the
  source of truth, and handlers are codegened *from* it via `oapi-codegen`
  during implementation.

## Core resources

| v1 concept | v2 concept | Stable? | Server-issued? |
|---|---|---|---|
| `session_id` (sequential int, recycled) | **gone** from API; lives in proxy internals | n/a | n/a |
| `player_id` (UUIDv4 from client) | **`player_id`** (UUIDv4 supplied by client, registered on first contact) | yes | no |
| `play_id` (UUIDv4 in archive) | **`play_id`** (UUIDv7, server-issued) | yes | yes |
| `port` (int, recycled) | proxy internal only | n/a | n/a |
| (none) | **`labels: Map<String,String>`** on player + play | mutable | client-set |

UUIDv7 (RFC 9562) for `play_id` because the leading 48 bits are a unix-millis
timestamp — IDs sort by creation time, which makes log scans and ClickHouse
range queries dramatically faster. Library: `github.com/google/uuid` v1.6+.
`player_id` stays UUIDv4 because the player generates it.

## The seven design issues, fixed

### 1. PATCH → JSON Merge Patch + field-level optimistic concurrency

**v1**

```
PATCH /api/session/2
{ "set": {...}, "fields": [...], "base_revision": "..." }
```

The `base_revision` is checked at the **resource level** — if anything
on the session changed since you read it, the server rejects. Two tabs
editing different controls fight each other through false conflicts.

**v2**

```
PATCH /api/v2/players/{player_id}
Content-Type: application/merge-patch+json
If-Match: "<control_revision>"

{ "fault_settings": { "all": { "type": "500", "frequency": 10 } } }
```

`null` clears a field (RFC 7396). GET returns `ETag: "<control_revision>"`.

**Concurrency is checked at the field level, not the resource level.**
The server tracks per-leaf-path revisions (`fault_settings.all.type` →
rev17, `shape.rate_mbps` → rev12, etc.). On PATCH:

1. Recurse into the merge-patch body and collect every leaf path the
   client is writing.
2. For each path, compare `field_revisions[path]` against the client's
   `If-Match`. Path was last modified *at or before* `If-Match` → fine.
   Path was last modified *after* `If-Match` → conflict on that path.
3. If any path conflicts, return `412 Precondition Failed` with
   `conflicts: [string]` listing the offending paths and
   `current_revision` for retry.
4. If no conflict, apply, bump the per-field revisions for written paths,
   set the resource ETag to the new max revision.

**Two tabs editing different controls both succeed.** Disjoint write-sets
don't fight. `412` only fires when two clients actually contend on the
same field, and the response tells the client *which* field — actionable
UI ("`fault_settings.all.type` was changed by another tab; reload?").

This stays fully compatible with stock Merge Patch + If-Match clients —
a client that doesn't know about the field-level model just sees
resource-level ETags and conflict checks; the granularity is purely on
the server side.

### 2. Drop `session_id` — player-centric API

**v1:** every mutation is keyed on `session_id` (sequential int, recycled
when sessions are reaped). Caching "the iPad is sid=2" breaks the moment the
iPad reconnects.

**v2:** mutations target `player_id` (stable per device) or `play_id`
(stable per playback). `session_id` is invisible from the API. The proxy
still tracks per-connection state internally — it's just not exposed.

| v1 path | v2 path |
|---|---|
| `GET  /api/session/{sid}` | `GET  /api/v2/players/{player_id}` |
| `PATCH /api/session/{sid}` | `PATCH /api/v2/players/{player_id}` |
| `GET  /api/sessions` | `GET  /api/v2/players` |
| (none) | `GET  /api/v2/plays/{play_id}` (archive read) |
| (none) | `PATCH /api/v2/plays/{play_id}` (play-scoped mutation) |

`POST /api/v2/players { "synthetic": true }` mints a synthetic player for
integration tests — replaces v1's "session creation as a side effect of a
real player connecting."

### 3. Typed `Player` and `Play` schemas

The ~80-field `map[string]any` of v1 becomes two structured schemas:

- **`PlayerRecord`** — connection-scoped state. Identity, current play,
  effective fault config, current shape, recent player metrics.
- **`PlayRecord`** — playback-scoped state. play_id, started_at,
  duration, effective rendition, archived after the play ends.

Sub-objects: `fault_settings`, `fault_counters`, `shape`, `player_metrics`,
`server_metrics`, `manifest`, `labels`. `additionalProperties: false`
everywhere — typo'd field names return `400` instead of silently no-opping.

### 4. Filter on what the manifest declares, not what URLs spell

**v1:** `all_failure_urls: ["2160p"]` — substring match against the URL string,
undocumented; magic `"All"` sentinel. The whole filter depends on URLs
being spelled a particular way (`generate_abr/`'s convention of
`playlist_6s_<height>p.m3u8` and `<height>p/segment_*.m4s`). Rename the
rungs and every filter silently stops matching anything.

**v2:** filter against **manifest-declared properties**. The master and
variant playlists declare *everything* the proxy needs:

| Manifest declaration | Filter primitive |
|---|---|
| `#EXT-X-STREAM-INF BANDWIDTH/RESOLUTION/CODECS` | `variant.{bandwidth_above, resolutions, codec_prefix, rung_positions, rung_indexes}` |
| `#EXT-X-MAP URI=` (HLS init segment) | `request_kind: [init]` |
| `#EXT-X-MEDIA TYPE=AUDIO` (HLS audio rendition) | `request_kind: [audio_manifest, audio_segment]` |
| variant playlist URI in master | `request_kind: [manifest]` (with `variant: {…}` to scope) |
| master playlist URL | `request_kind: [master_manifest]` |
| segment URI in variant playlist | `request_kind: [segment, partial]` |

The proxy parses the master + variant playlists at session start, builds
a per-session `URL → (request_kind, variant_descriptor)` lookup table,
and uses it on every fault evaluation. **Filter primitives are
manifest-declared, not URL-spelled, end-to-end.** Rename `init.m4s` to
`bootstrap.cmfv` and existing rules still hit because the manifest
still declares "this URL is the init segment for the 2160p variant."

```yaml
filter:
  request_kind: [segment]
  variant:
    rung_positions: [top]            # whatever's currently the top rung
    # OR
    bandwidth_above: 15000000        # ≥15 Mbps
    # OR
    resolutions: ["3840x2160"]
    # OR
    rung_indexes: [3]                # the 4th rung from the bottom
    # OR
    codec_prefix: "avc1."            # all H.264 variants
```

`url_match` stays as the explicit escape hatch for URLs the proxy
**can't** classify from a manifest — preroll, SCTE markers, ad
insertions, custom paths:

```yaml
filter:
  url_match:
    mode: substring
    patterns: ["/preroll/"]
```

Omit `filter:` to match every request on the surface (replaces v1's
magic `"All"` sentinel).

### 4b. Arbitrary fault rules, not five fixed surfaces

**v1:** five hardcoded surfaces (`all`, `segment`, `manifest`,
`master_manifest`, `transport`), each holding one fault config. At most
five concurrent faults; precedence between `all` and the surface-specific
slots is undocumented; surfaces duplicate `request_kind` filtering.

**v2:** an **array of `FaultRule`s** on the player and play records.
Each rule is an independent `(filter, behavior)` pair:

```yaml
fault_rules:
  - id: top-rung-500s
    filter:
      request_kind: [segment, partial]
      variant: { rung_positions: [top] }
    type: 500
    frequency: 10
    mode: failures_per_seconds

  - id: kill-the-init
    filter:
      request_kind: [init]
      variant: { rung_positions: [top] }
    type: 404
    frequency: 0          # one-shot

  - id: master-manifest-corruption
    filter:
      request_kind: [master_manifest]
    type: corrupted
    frequency: 1
```

**Init segments need no special mechanism** — they're just
`request_kind: [init]`, classified from `#EXT-X-MAP`. v1's "init falls
under segment" footgun goes away.

**Precedence:** first match wins. Rules are evaluated in array order;
the first whose `filter` matches the request determines the fault. Same
semantics as nftables / iptables / most reverse-proxy rule engines.
Predictable, easy to reason about, easy to debug ("why didn't the 4th
rule fire? — the 2nd one matched first").

**Transport faults move to `shape`.** They're nftables/kernel-level,
not HTTP-level — listing them alongside HTTP fault rules was a layer
mix:

```yaml
shape:
  rate_mbps: 5
  loss_pct: 5
  transport_fault:
    type: drop          # or reject
    frequency: 5
```

**Player vs play scope precedence** stays simple: if `play.fault_rules`
is set (including to `[]`), it *replaces* `player.fault_rules` for that
play. `null` or absent means "inherit." Per-rule merging by id was
considered and rejected — it's clever but rare-need, easy to get wrong,
and the user-visible behavior is harder to predict than whole-list
override.

### 5. Single URL convention

Every endpoint follows REST plural + sub-resource. No verb-in-path. Ports
disappear (proxy internal). Full path table in `proxy.yaml`.

### 6. Consistent SSE envelope

Every SSE frame is `{ type, data }`:

```json
{ "type": "play.network.entry", "data": { "play_id": "...", "entry": {...} } }
```

Frame types: `player.created`, `player.updated`, `player.deleted`,
`play.started`, `play.updated`, `play.ended`, `play.network.entry`,
`heartbeat`. Modelled with `oneOf` + `discriminator` in the schema.

### 7. Standard errors + auth

All errors return RFC 7807 `application/problem+json`:

```json
{
  "type":     "https://harness/errors/precondition-failed",
  "title":    "If-Match revision mismatch",
  "status":   412,
  "detail":   "Player was modified since revision <X>",
  "instance": "/api/v2/players/...",
  "current_revision": "<Y>"
}
```

`securitySchemes.basicAuth` declared in spec; opt-in via
`INFINITE_STREAM_AUTH_HTPASSWD`.

## Additive features (v2-only)

### A. Player-scope vs play-scope mutations

Mutations target either resource. They mean different things:

| Scope | Path | Lifetime | Use for |
|---|---|---|---|
| **Player** | `PATCH /api/v2/players/{id}` | Persists across `play_id` rotations and reconnects until explicitly cleared | Long soaks, persistent device-wide config |
| **Play** | `PATCH /api/v2/plays/{id}` | Auto-clears when the play ends or rotates | One-off "test what happens to *this* playback" experiments |

**Precedence** when both are set: `effective = play_override ?? player_default`.
Play-scope wins per attribute. Set device-wide 5% loss + per-play 500s; the
500s vanish when the play ends, the loss persists.

**No server-side `expires_at`.** Considered and rejected. State that mutates
under the user without an explicit call is confusing to debug, depends on
wallclock correctness, races with concurrent PATCHes, and adds a timer
subsystem to the proxy. The leaked-test-state problem is better solved
client-side: pytest `try/finally`, the CLI's `--for 5m` flag that wraps
apply+sleep+clear, the `session-controller` agent's existing teardown
pattern. Play-scope mutations already auto-clear on play end (the natural
lifecycle); synthetic player deletion clears the rest. Player-scope
mutations are deliberately long-lived until explicitly cleared.

### B. Labels (k/v tagging)

`labels: Map<String, String>` on every `PlayerRecord` and `PlayRecord`. Free-form,
client-set, mutable via PATCH (Merge Patch semantics — present key upserts,
`null` value removes that key, `null` for the whole map wipes).

```yaml
labels:
  test:        tests/integration/test_abr.py::test_downshift_under_loss
  pytest_run:  2026-05-08T05:00:00Z
  branch:      feat/claude-skills-437
  fixture:     insane-fpv-shots
```

**Inheritance:** plays inherit the player's labels; play-scope additions
override per-key.

**Propagated to ClickHouse** at insert time as a `Map(String, String)` column on
`session_snapshots`, `network_requests`, `session_events`, and `play_summaries`.
Rows are snapshotted with the labels in effect *at insert time* — labels
added later don't retroactively appear on prior rows.

**Filter pushdown:** archive endpoints accept `label.<key>=<value>` query
params, AND'd together. e.g.:

```
GET /analytics/api/v2/plays?label.test=test_abr&label.pytest_run=2026-05-08T05:00:00Z
```

**Validation:**
- Key: `[a-z][a-z0-9_./-]{0,62}` (lowercase, ≤63 chars).
- Value: any UTF-8, ≤256 chars.
- Max 32 labels per resource.
- Reserved namespace: keys starting with `harness.` or `_` are server-managed,
  rejected on PATCH.

### C. `play_summaries` for long-retention trends

Raw archive tables (`network_requests`, `session_events`, `session_snapshots`)
keep a 30-day TTL. The forwarder additionally writes a `play_summaries` row
when a play ends (last snapshot received, idle timeout, or explicit
`play.ended` SSE event). One row per play, ~200 bytes:

```sql
CREATE TABLE play_summaries (
  play_id            UUID,
  player_id          String,
  display_id         UInt32,
  started_at         DateTime64(3),
  ended_at           DateTime64(3),
  duration_seconds   UInt32,
  labels             Map(String, String),
  -- quality
  stall_count        UInt32,
  stall_seconds      Float32,
  rebuffer_ratio     Float32,
  downshifts         UInt32,
  upshifts           UInt32,
  dropped_frames     UInt32,
  avg_quality_pct    Float32,
  min_quality_pct    Float32,
  -- harness signals
  fault_count_total  UInt32,
  fault_categories   Array(String),
  shape_pattern      String,
  player_error       String,
  classification     String
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(started_at)
ORDER BY (started_at, play_id)
TTL started_at + INTERVAL 1 YEAR;
```

Trend queries that previously needed `network_requests` rows now hit a
small, dense table:

```sql
SELECT
  parseDateTime64BestEffort(labels['pytest_run']) AS run_at,
  count()                                          AS plays,
  avg(rebuffer_ratio)                              AS avg_rebuffer_ratio,
  quantile(0.95)(stall_seconds)                    AS p95_stall_seconds
FROM play_summaries
WHERE labels['test'] = ?
GROUP BY run_at
ORDER BY run_at;
```

The forwarder exposes this through `GET /analytics/api/v2/plays/aggregate`
(generic group-by) so consumers don't need direct ClickHouse access for
common queries.

**Who writes the summary row.** The **forwarder** owns this. The proxy
emits a `play.ended` SSE event (new in v2); the forwarder consumes it and
writes the summary row. Reasoning: the proxy doesn't talk to ClickHouse in
the current architecture, and a ClickHouse materialized view can't tell
when a play has ended (it only sees rows arrive). The forwarder is the
only component that has both signals.

Concrete flow:

1. Forwarder maintains a `Map<play_id, Accumulator>` while plays are live.
2. Each `play.updated` event nudges the accumulator (`stall_count++`,
   `total_stall_ms += delta`, etc.).
3. On `play.ended` — or a 5-minute idle timeout if the proxy died mid-play —
   the accumulator is finalised and one INSERT writes the `play_summaries`
   row.
4. **Crash recovery**: on forwarder restart, recompute summaries from raw
   rows for any `play_id` that has snapshots but no summary yet.
   Idempotent because `(play_id, started_at)` is the natural key.

### D. Filter pushdown on archive endpoints

Every list endpoint accepts:
- `label.<key>=<value>` (multiple → AND)
- `from`, `to` (RFC 3339 time bounds)
- `status_min`, `status_max` (on `network_requests`)
- `fault_category`, `fault_action` (on `network_requests`)
- `event_type` (on `session_events`)
- `limit`, `cursor` (cursor-paginated)

Replaces v1's "pull whole result set, filter client-side."

## Resource model summary

```
Player (live, in proxy memory)
  ├── id            : UUIDv4 (client-issued)
  ├── port          : (internal — not exposed)
  ├── labels        : Map<String, String>
  ├── fault_settings (player-scope)
  ├── shape          (player-scope)
  ├── current_play   : Play | null
  └── ...

Play (live in proxy, archived in ClickHouse)
  ├── id            : UUIDv7 (server-issued, time-sortable)
  ├── player_id     : ↑
  ├── started_at, ended_at
  ├── labels        : Map<String, String>  (inherits player labels + own)
  ├── fault_settings (play-scope override)
  ├── shape          (play-scope override)
  ├── manifest, server_metrics, player_metrics
  └── ...

PlaySummary (post-play rollup, ClickHouse, 1-year TTL)
  └── one row per finished play; quality + harness signals + labels
```

## Implementation prerequisites

These are signals the proxy doesn't emit today; v2 depends on them.

- **`play.ended` SSE event** — currently play boundaries are implicit (the
  proxy silently overwrites `play_id` when a new value arrives in a query
  string). v2 needs explicit emission on three triggers:
  1. **Rotation** — new `play_id` for the same player → emit `play.ended`
     for the old + `play.started` for the new. ~10 LOC at the request
     handler boundary.
  2. **Session reap** — `removeInactiveSessions` had a play active →
     emit `play.ended` with `reason=idle_timeout`. ~5 LOC.
  3. **Content complete** — no current signal. Subsumed under
     `idle_timeout` with a tunable threshold; revisit if false-positives
     hurt the rollups.
- **`play.started` SSE event** — same gap as `play.ended`. Emit when a
  player's first request carries a new `play_id`.
- **`player.created`/`player.deleted` SSE events** — v1 has session-state
  diffs only; v2 makes "new player appeared" a discrete event.

Total proxy work for the new emissions: ~50 LOC, all near
`removeInactiveSessions` and the play_id stamping site (main.go:4473).

## Migration plan

1. **Land v2 spec** (this doc + proxy.yaml + forwarder.yaml). No code yet.
2. **Implement v2 handlers** under `/api/v2/` — one feature group per PR
   (players, plays, faults, shape, labels, streams, errors).
3. **ClickHouse migrations**: add `labels Map(String, String)` columns to
   existing tables; create `play_summaries` table.
4. **Forwarder**: read labels from session record, write to row at insert.
   Write `play_summaries` row on play end. Add label-filter query params.
5. **Migrate the dashboard** to v2.
6. **Codegen `harness-cli/internal/api/`** from v2 spec via `oapi-codegen`;
   migrate CLI subcommands.
7. **Migrate agents and skills** — `session-control`, `session-analysis`.
8. **Add `Deprecation:` headers** to v1 endpoints.
9. **Remove v1** when no traffic for ≥30 days.

Realistic timeline: spec freeze + first handler PRs week 1; full v2 surface
+ ClickHouse migrations week 2; v1 removal month 3.

## Open questions

- **SSE frame size budget**: a `player.updated` event embeds the full
  `PlayerRecord` (~3 KB serialized). Heavy. Consider emitting just the
  changed fields with a discriminator, but that complicates clients. Worth
  measuring before deciding.

## Resolved

- **Path prefix**: `/api/v2/...` (and `/analytics/api/v2/...`).
- **Session ID format**: gone from user-facing API.
- **Play ID format**: UUIDv7 via `github.com/google/uuid` v1.6+.
- **`session_chat`**: stripped from CLAUDE.md and the session-analysis skill;
  dispatched to `session-analyzer` subagent for synthesis instead.
- **`llm_budget`, `llm_profiles`**: same — phantom endpoints removed from docs.
- **No server-side `expires_at`**: leaked-test-state is a client-side problem
  solved by pytest `try/finally` and the CLI's `--for` flag.
- **Filter on manifest-declared properties** (`bandwidth`, `resolution`,
  `codec`, `rung_position`), not URL substrings. URL match stays as an
  escape hatch.
- **ETag quoting (RFC 7232)**: `ETag: "<control_revision>"` and `If-Match:
  "<control_revision>"` — quoted, strong tags. The `control_revision`
  string is reused verbatim inside the quotes (no opaque token mint).
  Server emits with quotes; clients echo verbatim. Strip outer quotes
  server-side via `strings.Trim(headerValue, "\"")` before comparison.
  No support for wildcard `If-Match: *` or comma-separated multi-ETag
  lists in v2.
- **`oneOf` + discriminator**: kept as the SSE envelope shape. End-to-end
  validated with `oapi-codegen v2.7.0` against the actual v2 spec — 3853
  lines of compiling Go, 25 discriminator accessor methods, type aliases
  for every enum. Consumer pattern:
  ```go
  v, _ := ev.ValueByDiscriminator()
  switch x := v.(type) {
  case PlayerCreatedEvent:    ...
  case PlayNetworkEntryEvent: ...
  }
  ```
- **OpenAPI version**: **3.0.3**, not 3.1. `oapi-codegen` does not yet
  support 3.1 ([oapi-codegen#373](https://github.com/oapi-codegen/oapi-codegen/issues/373)) — fails on `type: [string, "null"]` and
  `const`. We tested this directly: the 3.1 spec produced a hard error;
  the converted 3.0.3 spec produced clean compiling Go. Scalar renders
  3.0 fine. When `oapi-codegen` ships 3.1 support, the upgrade is one
  find/replace away (semantics identical).
- **Nullable convention**: `nullable: true` (3.0 idiom) on a `type:`-ed
  schema. e.g. `{ type: string, nullable: true }`. Reviewers should
  reject any `type: [..., "null"]` introductions until we move to 3.1.
- **Single-value discriminator constraint**: use `enum: [value]` (3.0
  idiom), not `const: value` (3.1-only).
- **Field-level optimistic concurrency.** `If-Match` is required, but
  conflict detection runs against per-leaf-path revisions, not the
  whole resource. Two clients PATCHing disjoint paths both succeed.
  `412 Precondition Failed` only fires on actual per-field contention,
  with the offending paths listed in `conflicts: [string]` for
  actionable client recovery. Resource ETag is the max of all field
  revisions for backward-compatible RFC 7232 behaviour.
