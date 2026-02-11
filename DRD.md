# Design Requirements Document (DRD)

## Overview
This DRD captures the implementation-level behavior and operational complexity that underpin the testing session system. It complements the PRD by describing how the SSE session stream, session state caching, and control propagation actually work in practice.

## Goals
- Document the authoritative session state model and where it lives.
- Explain how SSE and polling produce a consistent UI state.
- Capture the control update path, conflict handling, and fan-out behavior.
- Record operational constraints and known failure modes.

## Non-Goals
- Re-stating product requirements already in PRD.
- Full protocol specs for LL-HLS/LL-DASH.
- Low-level OS tuning or kernel configuration details.

## Session Lifecycle Model
### Creation and binding
- A session is created on first request that includes `player_id` on the base proxy port.
- The proxy allocates a session number and binds it to `player_id` for subsequent requests.
- A session is effectively identified by its `session_id`/`session_number` and has a dedicated port mapping.

### Port allocation and routing
- The session port is derived by replacing the third-from-last digit of the external port with the allocated session number.
- External ports may be remapped to internal ports via `EXTERNAL_PORT_BASE`, `INTERNAL_PORT_BASE`, and `PORT_RANGE_COUNT`.
- Subsequent requests with the same `player_id` are redirected to the previously allocated session port.

### Teardown and expiry
- Session data is ephemeral and stored in memcache.
- Sessions are not persisted across proxy restarts; the UI should tolerate missing sessions.
- Ports are effectively recycled when session entries expire or are cleared.

## Session State Storage and Invariants
### Storage location
- Session state is stored in memcache under a shared `session_list` entry.
- The proxy treats this list as the single source of truth.

### Authoritative fields
- Control fields (failure settings, shaping parameters) are authoritative in the server.
- UI should treat server responses as authoritative and reconcile any local drift.

### Derived or runtime fields
- Runtime fields such as shaping pattern step index and last applied rate are server-populated.
- These values should not be mutated by the UI except through explicit APIs.

### Invariants
- Each session has a unique `session_id` and `session_number`.
- If `group_id` is present, group propagation applies to all sessions with that group.
- `control_revision` increments on control updates and is used for UI reconciliation.

## Control Propagation Rules
### Single-session updates
- PATCH requests update only the targeted session unless a `group_id` is set.
- Server validates and normalizes control values before storage.

### Group updates
- If a session has a `group_id`, a control update propagates to all sessions in the group.
- Propagation is server-side to ensure consistent state regardless of which UI tab initiated the change.

### Conflict handling
- Control updates include a `base_revision` to detect stale UI state.
- The server resolves conflicts by rejecting or re-basing updates according to the current `control_revision`.

### Idempotency expectations
- Repeating the same update should be safe and yield the same state.
- The server avoids redundant traffic shaping operations when inputs are unchanged.

## SSE Session Stream and Polling Fallback
### SSE behavior
- `/api/sessions/stream` emits full session snapshots with a monotonic `revision`.
- The client replaces its local list on each event, avoiding incremental drift.
- The server drops slow clients; dropped counts are surfaced for visibility.

### Polling behavior
- When SSE is unavailable, the UI polls `/api/sessions` at a steady interval.
- The UI applies the same normalization logic as SSE events.

### Normalization and UI state
- UI control widgets should only resync from the server when `control_revision` changes.
- Telemetry fields (counters, timestamps) can refresh every tick without changing inputs.

## Session Settings Caching
### Server-side caching
- Traffic shaping uses a per-port cache of last applied rate/delay/loss.
- The server only re-applies `tc` and `netem` when desired values change.

### UI-side caching
- UI holds last-known control values to avoid unnecessary re-renders.
- The UI should treat `control_revision` as the switch to accept new control values.

## Network Shaping Internals
### Shaping systems
- Traffic shaping uses `tc`/`netem` on Linux.
- The server provides a capability flag indicating shaping availability.

### Pattern shaping
- Pattern steps change the desired rate/delay/loss over time.
- Runtime values are written back into session state for UI visualization.

### Failure modes
- Non-Linux environments report shaping as disabled.
- A partially applied shaping state may exist if a system command fails mid-apply.

## Failure Injection Semantics
- Failures target segments, manifests, and master manifests independently.
- Consecutive and frequency parameters define the active failure window.
- Failures-per-second mode separates frequency and consecutive units.
- Corrupted payloads use deterministic offsets to keep the failure repeatable.

## Port Mapping Model
- External ports are used by browser clients; internal ports are used inside the container.
- The proxy maps external to internal ports for routing and shaping.
- Session port derivation must be consistent for redirect logic and shaping logic.

## Operational Risks and Recovery
### SSE drops
- Slow clients may miss SSE messages; the client should always accept the next full snapshot.

### Stale cache or missing sessions
- Memcache eviction can remove sessions; UI should handle missing entries gracefully.

### Partial updates
- Failure to apply shaping or rules should not block session updates; errors are logged.

### Recovery behavior
- Refreshing the UI should restore state via SSE or polling.
- Server restarts will clear sessions and require new `player_id` allocations.

## Observability
- Logs include session creation, redirects, and control propagation events.
- Shaping logs show apply/clear actions and verification output.
- SSE logs record dropped events for slow clients.

## Open Questions
- Should control updates be strictly rejected on stale `base_revision`, or re-based?
- Should group propagation be configurable per-field to allow partial linking?
- Do we want a server-side session TTL surfaced to the UI?
