---
name: fault
description: Inject HTTP errors, socket faults, or transport-layer drops on a player session via the harness CLI. Invoke when the user says "inject", "drop a 404", "fault on segments", "every N seconds", "kill the connection mid-body", "throttle", "reset faults", or otherwise wants to mutate the proxy's behaviour for one player. This skill owns the English-to-CLI-flag translation including the ambiguous-phrase tables; resolve disambiguation BEFORE running any harness command.
---

# Inject faults via `harness fault`

Default target: `https://jonathanoliver-ubuntu.local:21000` (test-dev). Every mutation is snapshot-protected by the CLI — `harness undo <target>` rolls back. You never have to manage state.

## Always do this first

Before any `fault add`, show the user what's already there:

```sh
harness --insecure fault list <target>
```

If rules are already active, surface them before adding new ones. The CLI's `first-match-wins` rule evaluation means a stale rule at the top of the list can mask a new one entirely.

After every successful add, echo the inverse:

```
added rule abc12345 — undo with `harness undo <target> --yes` or
                              `harness fault rm <target> abc12345`
```

## Phrase disambiguation — read this before parsing the user

English asks routinely map to genuinely different harness behaviours. Resolve these *before* picking flags; ask if you can't.

### "Drop"

| User says | Means | Command |
|---|---|---|
| "drop a 404", "drop 500s" | HTTP error | `harness fault add <t> --type 404` (or 500/503) |
| "drop packets", "blackhole it" | Transport drop (kernel discards) | `harness shape <t> --transport-fault drop` ⚠ |
| "drop the connection mid-request" | Socket fault | `harness fault add <t> --type request_body_reset` |
| "drop the session" | Delete the player | `harness players rm <t>` — **confirm first** |

⚠ Transport drop isn't exposed as a top-level shape flag yet — see "Known gaps" below. For now, use `harness raw PATCH` with `{"shape":{"transport_fault":{"type":"drop"}}}`.

### "Every / per / 1-in"

| User phrasing | Flags |
|---|---|
| "every 10 seconds", "1 every 10s" | `--frequency 10 --mode seconds` |
| "every 10 requests", "1 in 10" | `--frequency 10 --mode requests` |
| "X faults per second on every request" | `--frequency X --mode failures_per_seconds` (rare) |
| Bare "every 10" | **Ambiguous** — ask requests vs seconds. |

The default `--mode requests` is what most operators mean. `failures_per_seconds` means *X faults per second window*, NOT "once every X seconds" — easy to get wrong; confirm if the user says it.

### "Kill the network" / "break it" / "hose it"

Don't guess. Offer a menu:

- 100% transport drop → `harness raw PATCH /api/v2/players/<id> --body '{"shape":{"transport_fault":{"type":"drop","frequency":1,"mode":"requests","consecutive":1}}}'`
- Bandwidth = 0 → `harness shape <t> --rate 0` (player will stall on next segment)
- 100% packet loss → `harness shape <t> --loss 100`
- All 5xx on every request → `harness fault add <t> --type 503 --frequency 1 --mode requests`

### "All requests" vs "all sessions"

- **"every request kind"** / **"any request"** → omit `--kind` (matches all)
- **"all sessions"** / **"every player"** → iterate `harness players list`, run the mutation per id. Or create a group with `harness groups create --members A,B,C` and PATCH the group once.

### Reset / clear / undo — three different things

| Verb | Command | Behaviour |
|---|---|---|
| **Undo** ("undo that") | `harness undo <t>` | Replays the most-recent snapshot. Restores exactly what was there. |
| **Clear faults** | `harness fault clear <t>` | Removes all fault rules. Doesn't restore prior state — wipes. |
| **Wipe everything** | `harness players prune --yes` | Destroys ALL players + state. **Confirm before running.** |

`fault clear` does NOT undo a prior `fault clear`. Once cleared, `undo` will reverse the clear (restore the rules that existed before the clear); `fault clear` again would re-clear them.

### Number/unit normalisation

| User says | Flag value |
|---|---|
| "5mb", "5 megabits", "5 Mbps", "5M" | `--rate 5` |
| "100k", "100 kbps", "100 Kbits" | `--rate 0.1` |
| "1 gig", "1 Gbps" | `--rate 1000` |
| "100ms", "0.1s" | `--delay 100` |
| "5%", "5 percent loss" | `--loss 5` |

For ambiguous units ("100k") pick the streaming-plausible value (kbps for bandwidth) and tell the user what you assumed.

## Fault types

```sh
harness fault add <target> --type TYPE [--kind KIND] [--frequency N] [--mode MODE] [--consecutive N]
```

Available `--type`:

| Type | Layer | Notes |
|---|---|---|
| `403`, `404`, `500`, `503` | HTTP | Status code returned |
| `connection_refused` | HTTP | Server rejects the connection |
| `dns_failure` | HTTP | Synthetic DNS error |
| `rate_limiting` | HTTP | Returns 429 + Retry-After |
| `corrupted` | HTTP | Segment payload zero-filled (segment-only) |
| `request_connect_delayed/hang/reset` | Socket | TCP connect phase |
| `request_first_byte_delayed/hang/reset` | Socket | First-byte phase |
| `request_body_delayed/hang/reset` | Socket | Body-transfer phase |
| `none` | n/a | Disables a rule without removing it |

Available `--kind` (request_kind filter; omit for all):
`segment`, `partial`, `manifest`, `master_manifest`, `init`, `audio_segment`, `audio_manifest`.

`corrupted` is segment-only — pass `--kind segment` (CLI will reject with 400 otherwise).

## Common recipes

| Ask | Command |
|---|---|
| "404 every 10s on every request" | `harness fault add ipad --type 404 --frequency 10 --mode seconds` |
| "one-shot 404 on the next segment" | `harness fault add ipad --type 404 --kind segment` |
| "manifest timeout every 5 requests" | `harness fault add ipad --type request_first_byte_hang --kind manifest --frequency 5` |
| "10 consecutive 503s every minute" | `harness fault add ipad --type 503 --frequency 60 --mode seconds --consecutive 10` |
| "drop the manifest playlist" | `harness fault add ipad --type 404 --kind manifest` |
| "kill the connection mid-body" | `harness fault add ipad --type request_body_reset` |
| "503 only on 2160p segments" | `harness fault add ipad --type 503 --kind segment --url-substr 2160p` |
| "clear faults on the iPad" | `harness fault clear ipad` |
| "undo the last thing" | `harness undo ipad --yes` |

## Edit / remove individual rules

```sh
harness fault list ipad                          # shows short rule_id (8-char prefix ok)
harness fault edit ipad 20260517 --frequency 5   # change cadence without removing
harness fault rm ipad 20260517                   # delete one
harness fault clear ipad                         # delete all
```

The short rule_id from `fault list` is enough — the CLI expands prefixes.

## Known gaps (CLI follow-ups, not skill bugs)

- `harness shape` doesn't expose `--transport-fault drop|reject` as top-level flags yet. For now use `harness raw PATCH` with the shape body, or wait for the CLI to add typed flags.
- `harness shape` doesn't expose `--pattern` (square_wave / ramp / pyramid) — for ramp behaviour use `harness procedure abr-sweep --rates X,Y,Z --hold Ns` instead.
- `harness fault add` for play-scoped (vs player-scoped) rules isn't wrapped yet — use `harness raw POST /api/v2/plays/<play_id>/fault_rules` for play-scope.

## What NOT to do here

- **Don't snapshot first.** The CLI does it automatically on every mutation. Skipping the `harness snapshot` call isn't an oversight — it's redundant.
- **Don't add a rule without showing the existing list first.** Stale top-of-list rules silently consume requests that the operator thought their new rule would catch.
- **Don't run `players prune` or `players rm` without explicit user confirmation.** Even with `--yes` the CLI proceeds without asking; require the operator to say yes in the chat first.
- **Don't echo "rule added" without the undo command.** Every mutation reply ends with the inverse.

## See also

- `shape` — bandwidth / delay / loss / patterns (separate skill)
- `triage` / `investigate` — find what broke before deciding to inject
- `finding` — capture what a fault test reproduced
- `.claude/standards/avplayer-quirks.md` — how iOS reacts to specific faults
