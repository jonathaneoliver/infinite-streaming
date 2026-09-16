# Event taxonomy reclassification — spec

Status: **partially implemented** (branch `fix/fault-timeout-surfacing`).
Replaces the 2-way "cause / effect" axis the session dashboards use today.

- ✅ **Viewer classifier (§ Classifier change + § Testing-tier handling)** —
  done in `SessionDisplay.vue`, typechecks + builds, deployed to test-dev.
  The viewer now shows the 4-pill axis (Actions / Injected / Conditions /
  Reactions) and drops the `testing` tier off-axis.
- ✅ **"Test run" chip** — done in `SessionDisplay.vue`, deployed; renders on
  any play carrying `run_id` (viewer + live `testing.html`), so it also covers
  the live-banner case.
- ✅ **Sessions-list facet (§ below)** — client-side v1 done in `Sessions.vue`
  (4-category chip grouping + tint + filter facet). Reliable because the
  disambiguating sibling label co-occurs in the histogram.
- ⤓ **Forwarder `category_histogram`** — **optional / deferred (probably skip).**
  Only buys precise count badges (not built); even those are approximable
  client-side except on plays mixing injected + timeout faults. Backend redeploy
  not worth it unless exact badges are specifically wanted.

## Problem

The dashboard's event filter splits every labelled row into **cause** vs
**effect**, defined entirely by three `emit()` calls in
`content/dashboard-v3/src/components/SessionDisplay.vue` (≈187-192):

```js
session_events   → always 'effect'
network_requests → faulted ? 'cause' : 'effect'
control_events   → always 'cause'
```

This conflates four genuinely different causal roles into two buckets, and
sweeps in a fifth thing (harness metadata) that isn't causal at all. Concrete
symptoms:

- Filtering the Sessions list for `error=fault_timeout`, then opening the
  session viewer, shows **nothing** — fault rows land in "cause", and the
  Causes lane defaults **off** (`enabledKind = { effect: true, cause: false }`,
  SessionDisplay.vue ~524).
- A server-enforced **transfer timeout** (a guard firing on an already-slow
  transfer) is filed identically to a **deliberately injected 404** — even
  though `categorizeFaultType()` in `go-proxy` already separates them
  ("clock glyph" vs "scissors").
- `testing`-tier harness metadata (`run_id_*`, `total_stalls_0`,
  `profile_shifts_16`, `shocks_2`, `completed_*`) shows up as bogus "causes".

## Model: 4 tiers + a metadata side-channel

| Tier | Meaning | Source signal |
|---|---|---|
| **1 · Actions** | what the operator / proxy / harness *did* (stimuli) | `control_events` (config + lifecycle) |
| **2 · Injected faults** | proxy *actively fabricated or destroyed* a response / connection | network row, `fault_category ∈ {http, corruption, socket, transport}` |
| **3 · Conditions & results** | emergent outcomes / policy guards firing on a *real* degraded transfer — not fabricated | network row, `fault_category = transfer_timeout`; or clean-row degradation labels |
| **4 · Reactions** | what the player did | `session_events`; plus `fault_category = client_disconnect` (player hung up) |
| *(off-axis)* | **Metadata** — harness run annotations, not causal | the `testing` severity tier (#571) |

## Authoritative mapping (all source-confirmed)

**Tier 1 — Actions** — `control_events.event` (`computeControlLabels`,
`labels.go`):
`fault_on`, `fault_off`, `fault_rule_enabled`, `fault_rule_disabled`,
`fault_rule_config_change`, `pattern_step`, `pattern_enabled`,
`pattern_disabled`, `pattern_config_change`, `shaper_changed`,
`shaper_config_change`, `timeouts_changed`, `loop_server`, `content_changed`,
`server_start`, `session_start`, `session_end`, `control_change`.

**Tier 2 — Injected faults** — `categorizeFaultType()` (`go-proxy`):
- `http` — 404 / 5xx injection
- `corruption` — `corrupted`
- `socket` — every `request_*` (`connect`/`first_byte`/`body` ×
  `hang`/`reset`/`delayed`). **`delay_body` / `delay_header` live here.**
- `transport` — `transport_*` (nftables DROP / REJECT)

**Tier 3 — Conditions & results**:
- `transfer_timeout` — `transfer_active_timeout`, `transfer_idle_timeout`.
  These are **passive guards**: the proxy passes the real transfer through and
  cancels it once the timer elapses (`fault-injection-wire-contract.md`). They
  fire *because* the transfer was already slow (e.g. a big 2160p segment under
  active bandwidth shaping), not because the proxy injected an error.
- clean-row degradation labels (`faulted=0`, `labels.go`): `slow_request`,
  `slow_segment`, `qoe_ttfb_breach`, `qoe_transfer_stall`.

**Tier 4 — Reactions**:
- all `session_events` labels: `play_start`, `first_frame`, `shift_up`/
  `shift_down`, `stall_frozen`, `timejump`, player `qoe_*` breaches.
- VOMM anomaly labels `anomaly_<cond>_<surf>` (cond ∈ startup/fault/stall/end;
  surf ∈ net/event) — per-row surprise from `analytics/tools/derive_labels.py`,
  NOT a `session_events` lifecycle event. Legacy name: `unexpected_<cond>`.
- `fault_category = client_disconnect` — the player gave up mid-transfer.

**Off-axis — Metadata** (`testing` tier, `labels.go:44`, deliberately
unranked): `run_id_*`, `total_stalls_*`, `profile_shifts_*`, `shocks_*`,
`completed_*`, and any `label_changed`-carried KV. **Never** counts as cause or
effect. See "Testing-tier handling" below.

> Per-row, not per-label. A single network row carries several labels from one
> tier (`error=fault_timeout`, `warning=*segment_failure`,
> `warning=*transfer_active_timeout`). Tier the **row** by `fault_category`;
> every label on it inherits that tier. This matches `emit()`'s existing
> row-at-a-time shape and avoids the ambiguity that synthesized rollups like
> `*segment_failure` (which fire for *both* injected and timeout faults) would
> create under per-label tiering.

## Classifier change

Replace the `faulted`-int predicate in `SessionDisplay.vue` `emit()`:

```js
function tierForNetworkRow(r) {
  switch (r.fault_category) {
    case 'http': case 'corruption': case 'socket': case 'transport':
      return 'injected_fault';                 // Tier 2
    case 'transfer_timeout':
      return 'condition';                       // Tier 3
    case 'client_disconnect':
      return 'reaction';                        // Tier 4
    default:
      return hasDegradationLabel(r) ? 'condition' : 'reaction';
  }
}
// control        → 'action'   (Tier 1)  — UNLESS testing tier → metadata facet
// session_events → 'reaction' (Tier 4)
```

`hasDegradationLabel(r)` = row carries any `slow_*` / `qoe_*_breach` label.

### Minimal variant (keep the 2-way toggle)

If the 4-lane UI is too big a first step, map the tiers onto the existing
binary so it's a one-function edit:

- **Causes** = Tier 1 (Actions) + Tier 2 (Injected faults)
- **Effects** = Tier 3 (Conditions & results) + Tier 4 (Reactions)
- **Metadata** = excluded (see below)

This alone puts transfer timeouts on the **Effects** side (visible by default)
and removes the harness metadata — which fixes the original
"filtered `fault_timeout`, saw nothing" report.

## Testing-tier handling (folded in)

The `testing` severity tier is harness run metadata, written by the
characterization / server-behavior tests (`run_id` at run start; `total_stalls`
/ `profile_shifts` / `shocks` / `completed_*` as summaries at run end). It has a
purpose-built consumer already: **`Characterization.vue`**, which groups plays
by `run_id_<ts>` from each play's `label_histogram` and renders summary cards
(its `findLabel` reads the `testing=` prefix first, then legacy `info=`).

It does **not** belong in the cause/effect event filter. Two changes:

1. **Exclude `testing` from the cause/effect axis.** In `SessionDisplay.vue`,
   the tier filter (`SEVERITY_ORDER` / `tierTypes` / `tierCounts`, ~520-700)
   must skip `severity === 'testing'`; `emit()` must not assign these rows a
   cause/effect kind. Net effect: the empty/clutter `testing` lane disappears
   from the filter on the viewer, `testing.html`, and `testing-session.html`.

2. **Render run metadata as a session-header "Test run" chip.** Source it from
   the *same* labels `Characterization.vue` reads (`testing=run_id_`,
   `testing=total_stalls`, `testing=profile_shifts`, `testing=shocks`,
   `testing=completed_`). Show a compact chip in the session header on:
   - the **session viewer** (archived runs — full summary available), and
   - `testing.html` once a run is associated.

   This is the off-axis home that replaces the bogus "cause" rows.

### Separate small task — live `run_id` banner on `testing.html`

`testing.html` (`Testing.vue`) follows the **live** active player. End-of-run
summaries don't exist mid-run, so they can't populate live — but the
**`run_id`** (stamped at run start) can. Add a small run-context banner
("Run `…174229Z` · characterization in progress") sourced from the live
player's `run_id` label. Tracked separately from the reclassification because it
touches only `Testing.vue` and the live player record, not the classifier.

## Validation — this session, re-sorted

Play `8ca12887` (player `a45a161d`), window 17:42:28–17:51:45Z. Old model: 36
"causes". New model:

- **Actions (11):** `shaper_config_change` ·5, `pattern_disabled` ·4,
  `loop_server` ·2, `timeouts_changed` ·1, `label_changed` ·2
- **Injected faults (0):** none — no http/socket/corruption/transport injection
  in this play
- **Conditions & results (→ now Effects):** `transfer_active_timeout` ·2,
  `*segment_failure` ·2, `fault_timeout` ·2
- **Metadata (→ off-axis chip):** `run_id_*` ·2, `total_stalls_0`,
  `profile_shifts_16`, `shocks_2`, `completed_*`

This play had **zero deliberately-injected faults** — the "faults" were the
active-transfer-timeout policy firing because 2160p segments couldn't complete
under bandwidth shaping. Under the new model that reads correctly:
shaper/pattern/timeout *actions* (Tier 1) → over-long transfers hit the timeout
guard (Tier 3) → visible by default; metadata off-axis.

## Sessions-list category facet (follow-up)

The viewer change above is the per-event timeline classifier. The **Sessions
list** (`Sessions.vue` / `sessions.html`) is a different job — *finding* a
session among many — and should adopt the same vocabulary without cloning the
per-event toggle.

### What the list does today

Filters by **raw label strings** (`filters.labels` / `labelsExclude`, the
tristate AND include/exclude — this is the `error=fault_timeout` filter), plus
scenario facets (`platform`/`test` from the `testing=` tier), a harness-origin
filter, and a time range. Each row carries only the play's **label histogram**
(`labels: [string, count][]`) — *not* per-row `fault_category`.

### What was built — client-side v1 (✅ done, `Sessions.vue`)

1. **4-category chip grouping.** The Labels column's old `plyr`/`inj` split
   becomes four groups — `act` / `inj` / `cond` / `rxn` — same vocabulary as
   the viewer (`labelCategory()` + `chipsByCategory()`).
2. **Chip tint.** Each label chip gets a category-colored left accent; the
   background keeps the severity tint.
3. **Category filter facet.** A multi-select `act / inj / cond / rxn` toggle row
   (`filters.categories`, OR-semantics), AND-combined with the existing label
   filter — which stays.

All three are pure-derived from the play's existing `label_histogram`; **no
forwarder change**.

### Ambiguity — resolved client-side, not via the forwarder

The synthesized rollups (`*segment_failure`, `manifest_failure`) can't be mapped
from the label string alone. But the histogram is reliable anyway because the
**disambiguating sibling always co-occurs**: every `*segment_failure` row also
carries an `http_*` (→injected) or `fault_timeout`/transfer (→condition) label,
and both land in the play's histogram. So `labelCategory()` disambiguates an
ambiguous label by scanning the play's other labels (defaulting to `injected`
only if nothing else applies). The **facet, grouping, and tint are therefore
reliable client-side** — the forwarder rollup is not required for them.

### Forwarder `category_histogram` — optional, deferred (probably skip)

The rollup would only buy **precise per-play count badges** (a literal
"Conditions · 8" per row), which aren't built. Even those are approximable
client-side; the *only* inaccuracy is the ambiguous labels on a play that mixes
**both** an injected fault **and** a transfer timeout (rare) — those chips bucket
to one category instead of splitting per-row. Given it's a backend redeploy
(heavier, riskier) for a rare edge case in an unbuilt feature, **skip unless
exact count badges are specifically wanted.** If built later: add a per-play
`category_histogram` to the `query plays` aggregate (`CASE` over `fault_category`
+ source stream, mirroring the viewer's `categoryForNetworkRow`); this doc is the
canonical `fault_category → category` contract for both sides.

### Label-family → category (the client-side mapping, `Sessions.vue`)

| Label family | Category |
|---|---|
| `http_4xx`, `http_5xx`, `fault_other` | injected |
| `*master_manifest_failure`, `*manifest_failure` *(on injected rows)* | injected |
| `*transport_socket`, `corrupted` | injected |
| `fault_timeout`, `*transfer_active_timeout`, `*transfer_idle_timeout` | condition |
| `slow_request`, `slow_segment`, `qoe_ttfb_breach`, `qoe_transfer_stall` | condition |
| `*transport_disconnect` (client_disconnect), player `qoe_*`, `shift_*`, `stall_*`, `play_start`, `first_frame`, `unexpected_*` | reaction |
| any `control_events` event (`fault_on`, `pattern_*`, `shaper_*`, …) | action |
| **`*segment_failure`, `fault_incomplete`** | **ambiguous** — needs row `fault_category`; chip falls back to severity tint until the rollup lands |

### Scope

Done: grouping + tint + facet in `Sessions.vue` (frontend-only, ships via the
safe hot-deploy). Deferred/optional: the forwarder `category_histogram` for
exact count badges (backend redeploy — probably skip).

## Open items

1. **`anomaly_<cond>_<surf>` (VOMM anomaly labels; legacy `unexpected_<cond>`)** —
   per-row surprise labels from the VOMM scorer `analytics/tools/derive_labels.py`
   (`--mode score`), NOT the forwarder. Each rides on the exact net/event row whose
   token transition was improbable. (Earlier drafts mis-attributed these to "a build
   newer than `dev` HEAD / deployed-build drift" — they are simply the VOMM tool,
   which runs against ClickHouse out-of-band; the literal never greps because the
   label is built as `f"anomaly_{cond}_{surf}"`.) Decide: inherit the host row's
   tier (→ Tier 3), or give anomalies their own overlay facet.
2. **Deployed-build drift** — the running test-dev classifier already diverges
   from `dev` source (it treats `faulted=0` fault rows as causes, which `dev`'s
   `emit()` would not). Confirm the actually-deployed predicate before
   implementing, or implement on `dev` and redeploy the frontend.

## Files touched

- ✅ `content/dashboard-v3/src/components/SessionDisplay.vue` — classifier
  (`emit`/`sessionEvents` → `categoryForNetworkRow`), 4-pill axis,
  `enabledKind` defaults, `testing` off-axis (`SEVERITY_ORDER`). *(done)*
- ⬜ `content/dashboard-v3/src/components/SessionDisplay.vue` — "Test run"
  header chip (sourced from the `testing=` labels Characterization reads).
- ⬜ `content/dashboard-v3/src/pages/Testing.vue` — live `run_id` banner.
- ✅ `content/dashboard-v3/src/pages/Sessions.vue` — 4-category chip grouping +
  tint + filter facet (`labelCategory` / `chipsByCategory` / `matchesCategories`).
- ⤓ `analytics/go-forwarder/` — `category_histogram` on `query plays`
  (OPTIONAL; only for exact count badges; probably skip).
- (reference, no change) `content/dashboard-v3/src/pages/Characterization.vue` —
  existing run-grouped consumer of the same `testing=` labels.
- (reference) `go-proxy/cmd/server/main.go` `categorizeFaultType()` — the
  `fault_category` source of truth the classifier keys on.
- (reference) `analytics/go-forwarder/labels.go` — label vocabulary +
  `SevTesting`.
