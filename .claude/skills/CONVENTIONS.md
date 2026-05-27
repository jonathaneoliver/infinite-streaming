---
name: skill-conventions
description: Shared conventions for every skill in .claude/skills/. Each SKILL.md should cite this file in its frontmatter or intro and follow the rules below verbatim.
last_reviewed: 2026-05-19
---

# Skill conventions

This file is the canonical source of cross-skill rules. The individual SKILL.md files reference it instead of duplicating; if you're maintaining the skills, edits land here.

## 1. Bash command discipline

Claude Code's `Bash(<tool>:*)` permission rules match only the **first token** of each shell command. Compound prefixes (`VAR=…`, `cd …`, `export …`, `echo "header"; <real cmd>`) bypass any allowlist and re-prompt the user every call.

- **Lead with the tool name** — `harness …`, `jq …`, `curl …`.
- **Inline values instead of shell variables** — `harness query events abc123…`, not `PLAY=abc123\nharness …`.
- **One command per Bash invocation** — split labelled sections into separate calls.
- **Pipelines are fine** — `harness … | jq …` matches `Bash(harness:*)` because `harness` is first.

Why this matters: every re-prompt interrupts the operator's flow and undermines their `.claude/settings.json` allowlist. The user explicitly configured `Bash(harness:*)`, `Bash(jq:*)`, `Bash(date:*)`, etc. — respect that.

## 1a. Touching fault-injection wire behaviour

The proxy's HTTP fault types (`request_body_*`, `request_first_byte_*`, `request_connect_*`, `corrupted`, `transfer_active_timeout`) each produce a SPECIFIC wire shape that characterization tests interpret against. The canonical reference is [`.claude/standards/fault-injection-wire-contract.md`](../standards/fault-injection-wire-contract.md) — read it before editing `go-proxy/cmd/server/main.go § applySocketFault` or any fault-type case branch. Subtle behaviour changes (e.g. "X" filler vs real upstream bytes) silently invalidate test results. If you need a new behaviour, add a new fault-type name; don't repurpose an existing one.

## 1b. Using the harness CLI

Every skill in this directory shells out to `harness`. Before guessing flag names, output shapes, or subcommand boundaries, consult:

- [`harness/SKILL.md`](harness/SKILL.md) — Claude-discoverable wrapper, lists when to use sibling skills vs. raw harness.
- [`.claude/standards/harness-cli.md`](../standards/harness-cli.md) — canonical gotchas (flag-name traps, `--json` stdout-vs-stderr contract, label-encoding round-trip).
- [`tools/harness-cli/README.md`](../../tools/harness-cli/README.md) — full subcommand surface + common patterns.

The shortest correct path is usually: read the standards file once, run the command, only retry on failure after reading the printed usage block (which lists every flag for that subcommand).

## 2. No guessing during triage / investigation

Every causal claim must be **tagged**:

- `confirmed` — direct evidence in data, code, or an authoritative reference.
- `refuted` — evidence rules it out.
- `needs-test` — don't know yet; here's how to find out.

Untagged "this happened because X" is a guess. When uncertain, list the candidates + one distinguishing test rather than picking and hoping. Sending the operator down the wrong investigation path is worse than admitting uncertainty.

Before attributing meaning to an error code, label, or quirk:

1. Grep `.claude/memory/` for prior dispositions (e.g. `-12174` is documented sim-only noise, not a real error).
2. Grep `.claude/findings/` for tagged investigations.
3. Read the source mapping (`analytics/go-forwarder/labels.go`, `interpretCoreMediaErrorCode` in iOS code, etc.).
4. Only after those are exhausted, state a candidate tagged `needs-test`.

Synthesised labels (the `*` prefix in label names, e.g. `warning=*transport_disconnect`) are derived from specific fault tags in `labels.go` — the mapping is explicit. Grep before attributing blame.

## 3. Local time for display, UTC for storage

- **User-facing output** (responses, dashboards, reports, tables) → **local time**. The user reads on-device clocks; UTC mismatches make timelines hard to cross-reference.
- **Durable storage** (CH columns, JSON-on-the-wire, OpenAPI examples, finding files, file names, code) → **UTC**. Don't "fix" stored timestamps to local.
- **Don't guess the timezone offset** — macOS reports it: `date +%Z` (e.g. `PDT`) and `date +%z` (e.g. `-0700`). DST and travel both shift it.

## 4. Test-dev defaults (mutation skills only)

For `fault`, `shape`, and any other skill that mutates player/proxy state:

- **Default target**: `https://dev.jeoliver.com:21000` (test-dev, public Let's Encrypt cert via `dev.jeoliver.com`).
- **Every mutation is snapshot-protected**: `harness undo <target>` rolls back the most recent change to that player. No need to manage state manually.
- **Always show current state before mutating**: run the equivalent `--show` / `list` / `players show` first so the operator sees what's changing. This is mandatory for `fault` and `shape`.

## 5. Stop-conditions: don't manufacture concern

If a triage / investigation reveals nothing notable, say so in one line and stop. Examples of legitimate outputs:

- "Last 60 min on iPad: 0 stalls, 0 errors, 99% avg quality. Healthy baseline."
- "No critical events in the window. Nothing to drill."
- "Hypothesis I had is refuted by the data — moving on."

Don't fabricate findings to justify a report. The operator can read a "clean" outcome as easily as a problem report.

## 6. After-this-skill chaining

Every SKILL.md should end with a `## See also` section listing the most likely next-step skills given common outcomes. Be honest when there's no obvious next step ("you're done; nothing left to investigate" is a fine result).

## 7. Device + environment specifics

- The **iPad** in this dev setup is an **iOS simulator** on the same Mac, not a physical device. When diagnosing iPad streaming oddities, pair server-side data with `xcrun simctl spawn <UDID> log stream --predicate 'process == "InfiniteStreamPlayer"'` for AVPlayer/CoreMedia internals. Swift `print()` doesn't reach the unified log; CoreMedia subsystem logs do.
- The **iPhone** and **Apple TV** are real devices. iPhone is reachable via `idevicesyslog -u <UDID> -n` over WiFi. Apple TV (4K 2nd gen) has no USB port — uses Apple Configurator 2 over Bonjour for profile / cert work.
- The **Android TV** is a Google TV Streamer reachable via `adb` over WiFi.

## 8. CLI vocabulary (post-#474)

The harness CLI surface has been refreshed:

- `harness query plays|play|events|network|control|aggregate|heatmap|bundle` — read-only CH queries (formerly `harness archive …`). The `archive` name is gone; any `harness archive …` snippet found in docs is stale and should be rewritten.
- `harness checkpoint list|show` — pre-mutation state captures (formerly `harness snapshot`).
- `harness undo` — rolls back the last checkpoint.
- `harness ts <target> --streams events,network,control` — live SSE multiplex (replaces the old per-stream subcommands).
- `harness shape <target> --pattern pyramid|ramp_up|ramp_down|square_wave|sliders [--step-seconds N]` — pattern shaping (newer than the `--rate / --delay / --loss` static form, which still works for one-shot caps).

If a skill snippet still references `archive` or `snapshot`, it's stale.
