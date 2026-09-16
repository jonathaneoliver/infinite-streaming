# v1 is the runtime engine, v2 is a translating facade — the "not-yet-wired" gaps trace here — 2026-07-09

## Summary
The proxy has a **v2 control API** (`/api/v2/players/{id}/fault_rules`, request_kind
taxonomy, ordered first-match rules, rich filters) that is *not* the thing that decides
behaviour. At request time the runtime handler reads a **flat v1 model** —
`SessionData map[string]any` with `<surface>_failure_type` / `_frequency` fields plus an
`all_*` override (main.go ~3405, `shouldApplyFailure`). A v2 write is translated *down*
into those flat fields by `translate_faults.go`, because the runtime only knows how to
read v1. **v1 is the machine; v2 is a nicer steering wheel bolted on.**

The translation is lossy by construction, and that loss is the direct cause of the
"not yet wired" gaps: v2 can express `audio_segment`, `init`, `filter.variant`,
`filter.codec`, `filter.url_match`, and ordered first-match arrays; v1 has **6 hardcoded
surfaces + `all_*`** on a flat map. Anything richer 501s (`unsupportedFaultRuleError`)
or silently falls back to `targets=["all"]`. That fallback is **#917** (video-scoped
fault also hits audio) and the missing surface is **#918** (init has no v1 surface, so it
can't be scoped or even labelled — it lands as `request_kind='segment'`).

## Evidence

### The runtime reads v1, not v2
- `translate_faults.go:5-30` doc block: "v1 model: … five hardcoded surfaces … plus an
  `all_*` family". `v1SurfaceForRequestKind` returns `("", false)` for `init` / `audio_*`.
- Runtime consumer reads `<surface>_failure_type` off the session map
  (`go-proxy/cmd/server/main.go:3405`, `shouldApplyFailure`).
- v2 is mounted as a facade: `v2server.Mount(router, v2server.New(NewV2Adapter(app)))`
  (main.go:2698); `V1Adapter` interface (`internal/v2/server/v1adapter.go`) exposes v1
  state to v2 handlers as `map[string]any` — v2 reads/patches v1, never owns state.

### Who still speaks v1 externally (inventory 2026-07-09)
**Category ① — control-plane MUTATORS (the real blast radius, small + concentrated):**
1. **server_behavior suite** — `tests/server_behavior/*` (`sb_common_test.go` helper +
   limit/pattern/delay/transport/reported-rate/config-on-connect/restart tests):
   `PATCH /api/session/{id}` (faults), `POST /api/nftables/shape|pattern|loss/{port}`
   (shaping). The whole suite drives the v1 control plane directly.
2. **dashboard-v3 shaping-mode control** — `content/dashboard-v3/src/composables/
   useSessionShaping.ts` `POST /api/nftables/shaping-mode` (+ `StatusBanners.vue` reads
   `/api/nftables/capabilities`). The **new** UI still mutates shaping-mode via v1.

**Category ② — telemetry / discovery / SSE (v1 by URL prefix only; NOT the fault/shape
problem, keep as-is):**
- iOS (`apple/…/PlayerViewModel.swift`, `AVMetricsSubscriber.swift`): `POST
  /api/session/{id}/avmetrics`, `GET /api/sessions` (port discovery).
- Android ×2 (`android/ExoPlayerTestApp`, `android/InfiniteStreamPlayer`): `POST
  /api/session/{id}/metrics`, `/har/snapshot`, `GET /api/sessions`, `/api/sessions/stream`.
- forwarder (`analytics/go-forwarder/*`): `GET /api/sessions`, SSE `/api/network/stream`,
  `/api/control/stream`, `/api/avmetrics/stream` — archival ingest, pure read.
- harness-cli (`tools/harness-cli/…`): `GET /api/sessions` discovery **fallback**;
  **mutates shape via `/api/v2/players`** (v2, not v1).

### The math this changes
Fault editing from the primary dashboard already flows through **v2**
(`FaultRules.vue` → `player.fault_rules` → `usePlayer.ts` PATCH `/api/v2/players/{id}`),
and the harness shapes via v2. So v1's **remaining unique control-plane job is exactly
two writers**: the server_behavior suite and the dashboard shaping-mode POST. Everything
else on v1 is telemetry/discovery/streams that you keep regardless.

## Hypothesis / recommendation — tagged: needs-design
**Collapse the split (plan B).** Make the runtime request handler evaluate the v2
`fault_rules` array natively (first-match against the live request), deleting
`translate_faults.go`'s 501/`all` fallbacks → **#917 and #918 resolve at the root, not
per-surface**. Then repoint the two v1 mutators (shaping-mode → a v2 shape field;
server_behavior helpers → v2 or a thin v1 write-shim kept only for tests) and leave the
telemetry/discovery/SSE endpoints untouched.

Alternative (plan A, treadmill): keep translating and widen v1 with `init_*` / `audio_*`
surfaces + variant tracking. Rejected as the default because a flat surface map
fundamentally can't express ordered first-match rules — every future richer fault hits
the same wall.

**Distinguishing test before committing to B:** confirm no v1-only control field is read
by the runtime that has no v2 equivalent (shaping-mode, pattern steps, transport faults,
content manipulation, live-offset). Those must all be expressible in the v2 player model
first, or B leaves gaps.

## Gate result — 2026-07-09 (PASS with one required spec addition)
Ran the distinguishing check: exhaustively enumerated every v1 control field the go-proxy
runtime **reads** to decide per-request behaviour (fault / shaping / pattern / transport /
content / transfer-timeout / degraded-mode), then cross-referenced each against the v2
player model in `api/openapi/v2/proxy.yaml`. Authoritative runtime reads:
`NewFailureHandler` (main.go:10777, dynamic `prefix+"_suffix"` keys — literal grep
under-counts), `transportFaultConfigFromSession` (4914), `applySessionShaping` (7854),
`netemParamsFromSession` (2127), `parseShapeStepsFromSession` (3804),
`shouldApplyContentManipulation`/`newContentManipulation` (7365/7411),
`transferTimeoutsFor` (6114), `effectiveShapingForSession` (7795).

**Verdict: the v2 model can express the entire v1 control surface with exactly ONE
missing field.** Every domain maps 1:1 or as a v2 *superset*:
- Fault behavior/units/one-shot → `FaultRule.type/frequency/mode/consecutive` (units narrow
  from v1's split consecutive-vs-frequency units into one `mode`; one-shot is `frequency=0`).
- Fault scope → `FaultFilter` (`request_kind`+`variant`+`url_match`) — **superset**; the v1
  runtime only had `segment/manifest/master_manifest/all` surfaces and substring
  `*_failure_urls`. This is exactly why #917/#918 vanish once the runtime reads `fault_rules`
  natively — the model already carries `init`/`audio_*`/variant.
- Shaping (rate/delay/loss/jitter/2×correlation) → `Shape.*` 1:1.
- Pattern (enabled/steps/runtime-rate/template/driven-by) → `Shape.pattern.*` + readOnly
  mirrors; group fanout via the separate `PlayerGroup` resource.
- Transport fault (type + cadence, many v1 fallback keys) → `Shape.transport_fault`
  (type/frequency/mode/consecutive) 1:1.
- Content (7 fields) → `ContentManipulation.*` 1:1. Transfer-timeout (5) → `TransferTimeouts.*`
  1:1. Concurrency → `PlayerRecord.control_revision`.
- Schedule/count/loop fields (`*_failure_at`, `*_recover_at`, `*_count`,
  `transport_fault_active/started_at/phase/cycle`) are evaluator STATE, not stored config —
  no parity concern.

**The one GAP:** `shaping_forced_mode` (#910 degraded/http-only per-session shaping toggle,
read at `effectiveShapingForSession` main.go:7795) has **no field in the v2 `Shape` schema**.
It's the last v1-only control writer (dashboard POSTs `/api/nftables/shaping-mode`). Must add
a `Shape.mode` (enum `kernel`/`http_only`, or nullable `forced_mode`) + v2translate plumbing
BEFORE the native-evaluator refactor. Tracked as a precursor issue (see Related).

**Port-time notes (behaviour nuances, not gaps):** (1) confirm no rule relies on v1's split
consecutive-vs-frequency units; (2) confirm no flow uses `_reset_failure_type` to revert to a
non-`none` type (v2 one-shot only stops firing); (3) native evaluator must read `PlayerGroup`
membership for pattern fanout, not a session field.

## Related
- #917 (audio scope leak — `targets=["all"]` fallback), #918 (init kind never emitted).
- #919 — epic: collapse the v1/v2 split (this gate is its first step; PASSED).
- Precursor: add `shape.mode` to v2 (filed 2026-07-09 — the one gate gap).
