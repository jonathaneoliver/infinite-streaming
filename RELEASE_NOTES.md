# v2.0.0 — Release notes

**Headline:** the analytics surface, the dashboard, and the operator
tooling are all rebuilt around a coherent v2 model — three sibling
ClickHouse tables sharing one severity-tagged `labels[]` vocabulary,
a Vue 3 dashboard, a `harness` CLI binary covering the full v2 API,
and a set of Claude Code skills for driving the rig and analysing
incidents through prose prompts.

This is a **major** release: several v1 surfaces are removed. Read
the **Breaking changes** section before upgrading.

---

## TL;DR

- **Three coherent ClickHouse tables** — `session_events`,
  `network_requests`, `control_events` — share one severity-tagged
  `labels[]` vocabulary that drives every chip, tint, and filter in
  the UI.
- **A new Vue 3 dashboard** at `/dashboard/v3/...` replaces the
  legacy static-HTML pages.
- **A `harness` CLI binary** under `tools/harness-cli/` covers the
  full v2 API surface (24 endpoints + snapshot/undo discipline).
- **Six project-level Claude Code skills** under `.claude/skills/`
  (`triage`, `investigate`, `forensics`, `fault`, `shape`, `finding`)
  let operators drive the rig — and run forensic analyses — through
  natural-language prompts.
- **An in-dashboard AI chat panel** (`#497`, `#511`–`#515`) — ask the
  rig questions in prose, scoped to the session/play you're viewing,
  backed by a provider-agnostic forwarder chat backend.
- **A player ABR characterization framework** (`#482`, `#483`, `#493`)
  plus an **Automated Testing** dashboard page that groups runs and
  drills into per-step detail.
- **A server-behavior control-surface test suite** (`#518`–`#524`)
  that calibrates rate caps, delay, loss, patterns, fault injection,
  transport faults, and transfer timeouts against a live deployment.
- **A baseline rate cap** (`#480`) every new session inherits, with
  the kernel-truth `effective_rate_limit_mbps` surfaced in the UI.

---

## ⚠️ Breaking changes (read before upgrading)

### ClickHouse schema

| Was | Now | Migration |
|---|---|---|
| Table `session_snapshots` | Renamed to `session_events` (#472) | Update any direct SQL / Grafana panels / harness scripts that referenced `session_snapshots`. |
| Table `session_events` *(classifier output, pre-#472)* → renamed to `session_markers` (#472) → **dropped** (#474) | Gone | The classification semantic moved onto `labels Array(LowCardinality(String))` on the three live tables. Replace `SELECT … FROM session_markers WHERE type=X` with `SELECT … FROM session_events WHERE has(labels, 'severity=X')`. Pre-cutover rows age out via the 30-day TTL. |
| Table `control_events` (new) | Adds the third sibling | Additive — no migration required. |
| `labels Array(LowCardinality(String))` column on all three tables | New | Additive. Format: `<severity>=<event>`; severities `error \| critical \| warning \| info`; synthesized labels prefixed `*` (e.g. `*stall_severe_midplay`). |
| Case-sensitivity | Every forwarder ingest path now runs `player_id` / `play_id` through `canonicalV2ID()` (lowercases UUIDs). | Operator queries against historical rows that hardcoded **uppercase** UUID filters silently match zero post-cutover. Lowercase your WHERE-clause UUIDs. |

### HTTP / SSE surface

| Was | Now | Migration |
|---|---|---|
| `/api/session_markers`, `/api/v2/session_markers`, `/api/v2/session_events` *(the markers alias)* | **Removed** | Read `labels[]` off `session_events` / `network_requests` rows for the bucket-A signals; read `control_events` rows for proxy/operator actions. |
| `streams=markers` on `/api/v2/timeseries` | **Removed** → use `streams=control` | Update SSE subscriptions. The SSE event name `marker` is replaced by `control`. |
| `GET /api/v2/control_events` (new) | Reads the new table | Additive. |
| `GET /api/control/stream` (SSE, new) | Live proxy-side action stream | Additive. The forwarder subscribes to it for ingest; clients can subscribe directly too. |
| `play_id` synthesis by proxy | Player-driven only | Pre-bug-#4 clients that relied on the proxy minting a `play_id` from `control_revision` now see an empty `play_id` column. iOS 1.x+ already mints client-side. Other clients should mint UUID per play boundary and pass on every request URL + metrics POST. |
| Legacy v1/v2 dashboard pages (non-v3) | **Removed** in #459 | Replace bookmarks with `/dashboard/v3/...` equivalents (testing, testing-session, session-viewer, sessions, dashboard, grid). |
| v1 archive read API (the pre-v2 forwarder archive endpoints) | **Removed** (#478 / #496) | The dashboard's plays surface now reads exclusively from the v2 archive (`/api/v2/...`) via TanStack Query. Any external consumer of the old v1 archive endpoints must move to the v2 equivalents listed above. |

### Tooling

| Was | Now | Migration |
|---|---|---|
| `harness archive markers` subcommand (the `archive` family is renamed `query` in v2.0.0) | **Removed** → `harness query control` | The control_events table is the closest analog (proxy/operator actions). Player-emitted signals now live as `labels[]` on session_events. |
| Forwarder Go package `analytics/go-forwarder/eventclass/` | **Removed** | Internal to the forwarder; only matters if you forked. Classification logic moved into `labels.go` as ingest-time label computation. |
| `priority` numeric field on the retired `session_markers` table | Gone with the table | Use the severity prefix on the row's `labels[]` instead (`error` / `critical` / `warning` / `info`). |
| `restart_id` (UUID, pre-cutover) on session_events | Renamed to `attempt_id` (UInt32 counter) | Player-driven sticky counter, +1 per restart event. Reset to 1 at each play boundary. |

### Migration checklist

```sh
# 1. Apply the schema changes. Fresh deploys pick these up from
#    analytics/clickhouse/init.d/01-schema.sql automatically; for an
#    existing cluster, run each migration explicitly:
make analytics-migrate SQL='ALTER TABLE infinite_streaming.session_events ADD COLUMN labels Array(LowCardinality(String)) DEFAULT [] CODEC(ZSTD(1))'
make analytics-migrate SQL='ALTER TABLE infinite_streaming.network_requests ADD COLUMN labels Array(LowCardinality(String)) DEFAULT [] CODEC(ZSTD(1))'
make analytics-migrate SQL='ALTER TABLE infinite_streaming.session_events ADD COLUMN attempt_id UInt32 DEFAULT 0 CODEC(ZSTD(1))'
make analytics-migrate SQL='ALTER TABLE infinite_streaming.session_events ADD COLUMN last_buffering_time_s Float32 CODEC(ZSTD(1))'
# New tables — paste each CREATE TABLE IF NOT EXISTS block from the
# init.d schema files (re-running one that already exists is a no-op):
#   - control_events, characterization_runs → analytics/clickhouse/init.d/01-schema.sql
#   - llm_calls (AI chat panel)             → analytics/clickhouse/init.d/03-llm-calls.sql
make analytics-migrate SQL='DROP TABLE IF EXISTS infinite_streaming.session_markers'

# 2. Rebuild the forwarder + proxy.
make analytics-rebuild-forwarder
make test-deploy-dev          # or your env's full-deploy target

# 3. Rebuild and re-install the harness CLI.
make harness-cli

# 4. Update bookmarks / dashboards to /dashboard/v3/.
```

For self-hosted operators with **custom Grafana dashboards**: search
your dashboard JSON for `session_snapshots` and `session_markers` and
swap to `session_events` + `has(labels, 'severity=…')` predicates.

---

## What's new

### 1. API v2 + new Vue 3 dashboard

A from-scratch, OpenAPI-typed v2 API now sits alongside v1, modelled
around **plays** (one row per playback episode) rather than v1's
`(session_id, play_id)` tuples.

- Server: `go-proxy/internal/v2/server/` — typed handlers, fault rule
  resource, content / labels / shape PATCH, snapshot+restore.
- Forwarder archive: `/api/v2/snapshots`, `/api/v2/network_requests`,
  `/api/v2/control_events`, `/api/v2/plays`, `/api/v2/plays/aggregate`,
  `/api/v2/session_heatmap`, `/api/v2/session_bundle`, plus the
  **unified `/api/v2/timeseries` SSE** that multiplexes
  `streams=events,network,control` over one connection.
- Spec: `api/openapi/v2/{proxy,forwarder}.yaml` + Scalar UI mirror at
  `/dashboard/api-docs/`.

The dashboard at `/dashboard/v3/` is the canonical UI. Vue 3 SPA,
TanStack Query, brush-as-source-of-truth chart-coordination model,
Sessions picker for archived plays. Pages: `dashboard`, `testing`,
`testing-session`, `session-viewer`, `sessions`, `grid`.

### 2. Player identity model (bug #4)

Both `play_id` and `attempt_id` are now **player-driven**:

- iOS mints both at app launch; rotates `play_id` on real boundaries
  (content selection / fresh page-load) and `attempt_id` on every
  restart event.
- The proxy never synthesises them; the field stays empty when the
  player hasn't sent one yet.
- `attempt_id` is a UInt32 sticky counter on every row of all three
  tables.

### 3. Labels-driven classification

Every row in the three CH tables carries a severity-tagged `labels[]`
column. Same vocab drives row tint, chip rendering, severity filters,
and the Sessions multi-select.

- **session_events** labels: stalls with duration buckets +
  startup/scrub/midplay context, errors, restarts, ABR shifts.
- **network_requests** labels: HTTP outcomes (`error=http_5xx`,
  `warning=http_4xx`), fault categories (`*transport_socket`,
  `*transport_disconnect`, `*transfer_*_timeout`), per-kind failures,
  slow segments, request_retry (only on real retries, not normal
  manifest polling).
- **control_events** labels: operator and proxy actions
  (`*fault_rule_enabled`, `*pattern_enabled_rampUp` *(per pattern
  name)*, `*fault_on`, `*pattern_step`, `*session_start`, etc.).

### 4. `control_events` table

Brand-new sibling capturing every server-side or operator-driven
action: fault toggles, pattern step advances, shaper edits, harness
PATCHes (label edits, content swap, timeouts), session lifecycle.
Distinguished by `source ∈ {harness, proxy, auto}`. Replaces the
retired `session_markers` table.

### 5. Dashboard UX

**Sessions page**

- New **Labels** column rendering severity-tinted chips
  (`count× event_name`), sorted most-frequent-first. Backed by a
  per-`(session, play)` label histogram across all three tables.
- New **hierarchical tristate filter**: one tier per severity
  (`Critical → Error → Warning → Info`), per-label cycle on click
  (none → include → exclude → none), per-tier group toggles. Compose
  AND-INCLUDES with AND-EXCLUDES for queries like *"has http_4xx
  AND has-not fault_rule_enabled"*.

**Session viewer**

- New **Play Log** fold interleaving event / network / control rows
  on one chronological scroll with per-source filters, severity
  tint, and label chips. **Flags column** mirroring NetworkLog
  glyph semantics for network-source rows. **Multi-line row
  tooltip** per source type.
- New **Focus Window** fold with synchronised brush + a severity
  filter accordion derived from labels across all three streams.
- `start_time` / `end_time` URL contract scopes the focus window
  before any SSE backfill lands.
- "Show context" toggle widens the SSE backfill window around an
  archived play.

**NetworkLog**

- New **Labels** column lands right after Flags so the operator can
  read "glyph + label chips" at a glance.

**Cause / Effect axis** (Focus Window severity filter)

- `cause` = every `control_events` row + every `network_requests`
  row with `faulted=1` (proxy-injected faults).
- `effect` = the player's reactions + clean network traffic.

### 6. `harness` CLI

A from-scratch greenfield CLI under `tools/harness-cli/` covers the
full v2 surface (24 endpoints). Subcommand families:

- **`harness players`** — list, show, search by UUID prefix /
  substring / label / User-Agent.
- **`harness fault`** / **`harness shape`** / **`harness label`** —
  mutation. Every command snapshots state before applying and
  supports `harness <verb> undo` for replay-to-prior-state.
- **`harness query`** (alias `q`) — `plays`, `play`, `aggregate`,
  `events`, `network`, `control`, `heatmap`, `bundle` for archive reads.

Build with `make harness-cli`. Client stubs are checked in.

### 7. Claude Code skills + standards + findings

Six focused skills under `.claude/skills/` let operators drive the
rig and analyse incidents through prose prompts:

- **`triage`** — survey what's broken on a session in <10 s.
- **`investigate`** — drill a single event with windowed context.
- **`forensics`** — dispatch a Sonnet subagent for "why does this
  keep happening" multi-event causal hypotheses.
- **`fault`** / **`shape`** — mutate the rig with built-in
  snapshot + undo discipline.
- **`finding`** — capture what an investigation taught you to a
  searchable library.

Supporting docs:

- `.claude/standards/` — playback-knowledge references (AVPlayer
  quirks, ABR decision model, HLS taxonomy, codec strings).
- `.claude/findings/` — capture-finding library; `forensics`
  consults it before dispatching the subagent.
- `.claude/agents/playback-forensics-expert.md` — the read-only
  subagent's system prompt + tool allowlist.

### 8. iOS player

- Player-driven `play_id` + `attempt_id` (bug #4 fix).
- Persistent `player_id` across rebuilds via UserDefaults.
- `player_metrics_last_buffering_time_s` on `buffering_end` POSTs.
- Always-emit `last_event=error` on `player_error` transitions.
- User-Agent preservation across metrics POSTs so the proxy's
  `iPad/iPhone/AppleTV` family label survives.

### 9. In-dashboard AI chat panel (#497, #511–#515)

Ask the rig questions in prose, without leaving the dashboard.

- **Forwarder chat backend** — Anthropic-native client with prompt
  caching, plus an OpenAI-compatible path so hosted (OpenAI / litellm /
  HF) and local (mlx) providers all work. Live model discovery from
  `{base_url}/v1/models`; per-user base-url + per-profile API-key
  overrides in the UI.
- **`ChatPanel`** mounts on Testing, TestingSession, and SessionViewer
  with **harness-aware scope** — the bot knows which `player_id` / play
  / brush window you're looking at, and citations carry the `player_id`.
- **Tooling discipline** — context-window + token meter, bytes-budget
  guard with summary mode + history trimming, an `investigate` subagent,
  `propose_finding`, and label-vocabulary discovery (`list_labels`,
  wildcard match) so the bot queries the real `labels[]` vocab.
- A standalone **`/ask` fleet page** for fleet-wide questions.

### 10. Player ABR characterization framework (#482, #483, #493)

A Go-driven framework that puts a real device through scripted ABR
scenarios and archives the result.

- **Go test framework** under `tests/characterization/` with an Appium
  launcher, symmetric margin sweeps, and a cold-start-under-throttle
  wrapper. Test modes include a **startup test** (app-cold + channel-
  change) and a **client-fetch-abort test** (5-shape fault matrix).
- **OTel spans + standard cycle-label schema** per cycle, carrying
  `player_id` / `play_id` and failure status, ingested into ClickHouse.
- **Automated Testing dashboard page** groups characterization runs,
  with per-cycle session-viewer links, a cycle-band overlay, and an
  expandable per-run **Details** panel (summary + variants).
- `AVERAGE-BANDWIDTH` end-to-end with a 5% margin default (#483).

### 11. Server-behavior control-surface test suite (#518–#524)

An integration suite under `tests/server_behavior/` that calibrates
every go-proxy control surface against a live deployment and records the
baselines in `.claude/standards/server-behavior.md`:

- Rate-cap accuracy, `nftables` delay / loss / pattern fidelity,
  per-kind HTTP fault frequency + failure-type coverage, transfer
  timeouts, **socket-phase faults** (connect/first_byte/body ×
  reset/hang/delayed), fault **variant-scope** isolation, and content
  manipulation (+ m3u8 parseability).
- **Transport faults** (`drop`/`reject`) are characterized via the
  kernel `nftables` packet counters — the un-maskable ground truth,
  since a fresh connect is masked by Docker's userland proxy.
- Supporting proxy changes: generic numeric-status HTTP fault injection
  (any 4xx/5xx honored directly) and an adaptive ICMP path-ping timeout.

### 12. Baseline rate cap (#480)

Every new player session inherits a configurable baseline rate cap.

- `INFINITE_STREAM_DEFAULT_RATE_MBPS` sets it; `GET /api/v2/info`
  exposes `default_rate_mbps`; new sessions are capped at creation.
- `effective_rate_limit_mbps` is surfaced as a first-class field (kernel
  truth, distinct from operator intent), with a persistent baseline chip
  + slider label in the UI and a `harness shape --show` baseline view.
- `restoreShapeApplication()` re-installs tc state for sessions that
  survive a proxy restart, closing a silent-uncap regression.

### 13. Test consolidation: pytest retired (#529)

All Python/pytest integration tests are removed — the Go
characterization (#482) and server-behavior (#518) suites now cover that
surface. Contributors run the Go suites; there's no Python test
dependency left.

---

### 14. HTTP / HTTPS toggle + cert options

The dashboard, API, and shaper ports now default to **HTTPS + HTTP/2**,
gated by `INFINITE_STREAM_TLS` (`on` by default; `off`/`0`/`false`/`no` for
plain HTTP). HTTP/2 is what keeps the dashboard's many SSE streams under
Chrome's 6-per-origin cap. The toggle flips both the nginx listener **and**
the nginx→go-proxy `proxy_pass` scheme + go-proxy's own listener, so an
HTTP-only deploy no longer 502s on `/api/v2/*`. The auto-generated
self-signed cert takes its SAN list from `INFINITE_STREAM_TLS_SAN` (so it
can match `.local` / LAN-IP names), and a supplied mkcert / Let's Encrypt
cert in the certs dir is used untouched. New `make test-deploy-dev-http`
stands up a plain-HTTP mirror that shares the content library but keeps its
own state. Full reference — cert modes, the Cloudflare/LE DNS-01 runbook,
mkcert, and installing a CA on Apple clients — in `docs/TLS.md`.

---

## Known gaps

These are carried into the next release:

- **#475** (P3) — iOS bootstrap fires `state_change` twice (one
  empty, one real).
- **#476** (P2) — sprint-tracking anchor for the dashboard polish
  work (Sessions filter, NetworkLog labels, PlayLog flags+tooltip).
- **#477** (P2) — top-level README/CLAUDE.md still don't cover the
  new `harness` CLI or the skills; `.claude/skills/USAGE_WALKTHROUGH.md`
  is a fresh scaffold that needs a live run-through.

---

## Issues delivered in this release

#437 #441 #454 #455 #456 #457 #458 #459 #461 #462 #463 #464 #465 #466
#467 #468 #469 #470 #471 #472 #473 #474, plus the `#444` follow-up
that was completed by the labels-on-source-rows architecture.

Landed since the initial v2.0.0 draft and folded into this release:
#478 #480 #482 #483 #493 #496 #497 #498 #511 #512 #514 #515 #518 #519
#520 #521 #522 #523 #524 #525 #526 #527 #529.

---

## Compatibility matrix

| Client | Works against v2.0.0? | Notes |
|---|---|---|
| iOS 1.0 (current) | ✓ | All player-id / attempt-id / labels work is in the current build. |
| Roku / AndroidTV (no v2 work yet) | ✓ degraded | They emit metrics POSTs as before; the proxy stamps an empty `play_id` until those clients mint client-side IDs. Analytics rows still land. |
| `harness` CLI (pre-v2) | ✗ | The legacy CLI is gone. Rebuild from `tools/harness-cli/` via `make harness-cli`. |
| Custom Grafana dashboards on v1 schema | ✗ | Update to reference `session_events` / `labels[]`. |
| Saved Scalar UI tabs | ✓ | Endpoints regenerated from the v2 yaml; the Scalar UI auto-reflects. |
| AI chat panel (`#497`) | ✓ opt-in | Inert until an LLM provider is configured (Anthropic-native or any OpenAI-compatible / local endpoint). No provider → the panel just doesn't answer; nothing else is affected. |
