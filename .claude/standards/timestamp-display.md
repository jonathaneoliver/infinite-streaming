# Timestamp display

How to render timestamps in user-facing prose. Canonical for both Claude Code (loaded via `CLAUDE.md` pointer) and the dashboard chat bot (loaded via `read_standard("timestamp-display")` and referenced from the embedded chat system prompt).

## Rule

**Every conversational timestamp must show BOTH the operator's local time AND UTC, side-by-side.**

- **Local first** — matches what the operator sees on their device clock and on dashboard chart axes (which render in local time).
- **UTC in parentheses** — matches what every API response, ClickHouse row, log line, URL parameter, and chat handoff file carries.

Showing only one forces mental conversion to find the other. Showing both = zero conversion cost.

## Format

| Context | Format | Example |
|---|---|---|
| Short prose (same day) | `HH:MM:SS LOCAL (HH:MM:SS UTC)` | `08:43:04 PDT (15:43:04 UTC)` |
| Longer prose (cross-day) | `YYYY-MM-DD HH:MM:SS LOCAL (HH:MM:SS UTC)` | `2026-05-24 18:58:34 PDT (01:58:34 UTC on 2026-05-25)` |
| Tables | Single column labelled `Time (LOCAL / UTC)` with values stacked, OR two columns | `18:58:34 PDT`<br>`(01:58:34 UTC)` |
| Sub-second precision | Carry it through both renders | `08:43:04.761 PDT (15:43:04.761 UTC)` |

## Where UTC stays the only format

The rule above is **for conversational prose only**. UTC remains the sole canonical format anywhere data is stored, transmitted, or referenced by machine:

- URL parameters (`?from=20260525T015825Z`)
- `curl` / `harness` CLI arguments
- ClickHouse queries (`WHERE ts >= '2026-05-25T01:58:25Z'`)
- API response bodies
- File names (`startup-manual-20260525T013430Z.log`)
- Finding doc names (`pyramid-stall-2026-05-24.md` — date stamps use the user's local day for findability)
- Commit messages
- Code (variables, log lines, comments)
- Citations the chat bot's `cite()` tool encodes (the citation card surfaces local in the UI; underlying data stays UTC)

## How to apply

1. **When the source is UTC** (API response, harness output, CH row, ledger record): convert to the operator's local zone, then render in `LOCAL (UTC)` form.
2. **When the source is local** (operator typed a time, dashboard URL shows a local-time-displayed value): infer UTC, render in `LOCAL (UTC)` form.
3. **macOS conversion command** (Claude Code, for ad-hoc work):
   ```sh
   # UTC → local
   date -jf '%Y-%m-%dT%H:%M:%S' '2026-05-25T01:58:25' '+%H:%M:%S %Z'
   # → 18:58:25 PDT
   ```
4. **Don't hardcode the operator's offset.** Determine per session:
   - Claude Code: `date +%Z` (zone label like `PDT`) and `date +%z` (numeric offset like `-0700`).
   - Dashboard chat bot: the operator's IANA timezone is passed in every chat request body as `tz` (e.g. `"America/Los_Angeles"`) and surfaced in the scope preamble. Use it. If absent (legacy clients), default to UTC-only with an apology, not a guess.
5. **Sub-millisecond timestamps are noise in prose** — round to the precision that matters for the question (typically seconds or, for stall investigations, milliseconds).

## Why

UTC is unambiguous (no DST, no location drift) and IS the wire/storage format — so the operator constantly sees it in raw API output, log lines, URL params, and chat handoff files they paste between tools. Local time matches what they see on their device clock and on dashboard chart axes. Showing only one means the operator does the conversion every time they cross-reference. Showing both eliminates that friction without ambiguity.

Refined 2026-05-24 after a repeated operator complaint about UTC-only prose forcing them to subtract 7 hours in their head every time the bot quoted an event time.

## Examples (right and wrong)

**WRONG (UTC-only in prose):**

> The stall happened at 15:43:04 UTC, about 34 seconds after the network was throttled to 1.259 Mbps at 15:42:30.501Z.

**WRONG (local-only in prose):**

> The stall happened at 08:43:04 PDT, about 34 seconds after the network was throttled to 1.259 Mbps at 08:42:30.501 PDT.

**RIGHT:**

> The stall happened at 08:43:04.761 PDT (15:43:04.761 UTC), about 34 seconds after the network was throttled to 1.259 Mbps at 08:42:30.501 PDT (15:42:30.501 UTC).

**RIGHT (table form):**

| Time (PDT / UTC) | Event |
|---|---|
| 08:42:30.501 PDT (15:42:30.501 UTC) | Shaper set to 1.259 Mbps |
| 08:43:04.761 PDT (15:43:04.761 UTC) | Stall registered |
