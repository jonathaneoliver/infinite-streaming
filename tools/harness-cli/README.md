# harness CLI

Operator surface for the InfiniteStream test rig. Drives the v2 proxy + forwarder over HTTPS — every mutation surface and every read query lives behind a single command.

```sh
make harness-cli              # build + install onto $PATH
harness --insecure info       # smoke check
```

## Global flags

| Flag | Default | Notes |
|---|---|---|
| `--base URL` | `$HARNESS_BASE_URL` or `https://jonathanoliver-ubuntu.local:21000` | Override to point at a different rig. |
| `--insecure` | off (use env `HARNESS_INSECURE=1` to make it sticky) | **Required** against test-dev's self-signed cert. Drop it and you get `tls: failed to verify certificate for dev.jeoliver.com`. |
| `--basic USER:PW` | `$HARNESS_BASIC_AUTH` | HTTP Basic auth header for protected paths. |
| `--json` | off | Emit JSON for one-shot commands; NDJSON for streaming. **See "Output contract" below — errors come out as plain text on stdout.** |

## Command surface

| Family | Purpose | Subcommands |
|---|---|---|
| **`players`** | Inspect / manage player records | `list`, `show`, `create`, `rm`, `prune` |
| **`fault`** | HTTP failure injection rules | `list`, `add`, `edit`, `rm`, `clear` |
| **`shape`** | Network shape (rate / delay / loss) | one-shot flags on `shape <target> --rate N --delay N --loss N [--clear / --show]` |
| **`labels`** | Operator KV labels on player | `show`, `set`, `rm`, `clear` |
| **`timeouts`** | Transfer timeouts | `<target> --active --idle [--applies-* / --show / --clear]` |
| **`content`** | Master playlist mutators | `<target> --strip-* --overstate-* --live-offset` |
| **`play`** | Inspect live play | `show`, `patch` |
| **`groups`** | Player groups | `list`, `show`, `create`, `patch`, `add`, `remove`, `rm` |
| **`tail`**, **`ts`**, **`events`** | Live SSE streams | `tail` (network), `ts` (combined), `events` (lifecycle) |
| **`query`** / `q` | Read-only forwarder archive queries | `plays`, `play`, `aggregate`, `events`, `network`, `control`, `heatmap`, `bundle` |
| **`procedure`** | Composed multi-step tests | `soak`, `abr-sweep`, `fault-soak` |
| **`checkpoint`** / `ck` | Pre-mutation state snapshots | `list`, `show` |
| **`undo`** | Replay the most recent checkpoint | `[<target>|<id>]` |
| **`finding`** | Capture investigation results | `add <target>` |
| **`info`** | healthz + identity across both services | `info [--bundles]` |
| **`raw`** | Escape hatch (no resolver, no checkpoint) | `<METHOD> <PATH>` |

Run `harness <subcommand>` with no args for per-subcommand usage. Most one-line forms are also documented in [`.claude/standards/harness-cli.md`](../../.claude/standards/harness-cli.md).

## Targets

Most subcommands accept a target. The resolver matches against the live player list using any of:

- Full UUID (`3bff77d6-56af-468b-…`)
- 6+ char hex prefix (`3bff77d6`)
- Label value (`device=ipad` → the player labelled that)
- Player IP
- User-Agent substring

If nothing matches, the resolver fails fast — no side effects.

## Output contract

- **Success, default**: human-readable text on stdout.
- **Success, `--json`**: one JSON document for one-shot commands; one JSON document per line for streaming subcommands (`ts`, `tail`, `events`).
- **Errors**: plain-text `error: …` on **stdout** (not stderr), non-zero exit.

**Piping `--json` output straight to `jq` will explode on errors** because `jq` sees `error: foo` as a malformed JSON literal. Either:

```sh
out=$(harness --insecure --json query play "$pid") || { echo "$out"; exit 1; }
echo "$out" | jq …
```

… or run the command without `| jq` first to confirm you got JSON back.

## Flag-name traps

- **Label filters are `--label-has` / `--label-not`, NOT `--label`.** Used by every `query` subcommand. Repeatable (AND semantics):
    ```sh
    harness --insecure --json query plays \
        --label-has testing=test_rampup \
        --label-has testing=platform_iphone \
        --limit 20
    ```
- **`harness query control <play_id>`** parses a play_id positionally but the underlying endpoint requires `player_id`. Until that's fixed, use:
    ```sh
    harness --insecure raw GET \
        "/analytics/api/v2/control_events?player_id=<PLR>&play_id=<PLY>&limit=N"
    ```
- **There is no `harness archive …` subcommand.** Forwarder archive reads live under `harness query` / `q`.

## Operator labels — how they propagate

`harness labels set <player> k=v k2=v2` writes onto the player record. Each KV pair:

1. Lives on the player record (visible via `harness labels show`).
2. Emits a `label_changed` control event (the proxy fix in #487 made this work on the v2 PATCH path).
3. The forwarder turns each KV pair into a `testing=<key>_<value>` row label on the control_events table (the `testing` tier, #571 — was `info=` before that; legacy rows persist for the ≤30-day TTL).
4. The Sessions dashboard's Labels column renders them as chips on the current play, grouped under the Testing tier.

**Encoding gotcha:** `test=rampup` on the player → `testing=test_rampup` on the row. The filter is `query plays --label-has testing=test_rampup`, NOT `--label-has test=rampup`. (For rows written before #571, use the legacy `--label-has info=test_rampup`.)

## Common patterns

```sh
# Survey live players (one-line per row)
harness --insecure --json players list \
    | jq -r '.[] | "\(.id[:8])  \(.user_agent[:40])"'

# Show one player by short prefix
harness --insecure players show 3bff77d6

# Tail combined event/network/control SSE
harness --insecure ts 3bff77d6 --streams events,network,control

# Find every play tagged by a characterization run
harness --insecure --json query plays \
    --label-has testing=test_rampup \
    --label-has testing=platform_iphone \
    --limit 20

# Inspect one play's events + label histogram
harness --insecure --json query play <play_id>

# Apply a network cap (idempotent — re-apply with same value is a no-op)
harness --insecure shape ipad --rate 2.5

# Set operator labels on a player (overnight workflow uses this)
harness --insecure labels set <player> test=rampup run_id=20260521T160000Z

# Snapshot before mutating then undo if needed
harness --insecure checkpoint list | head
harness --insecure undo
```

## Checkpoints + undo

Every mutation writes a JSON checkpoint to `~/.claude/state/harness/<repo>/`. `harness undo` replays the most recent. Useful when an interactive session hits a wrong target or wrong rule.

## See also

- [`.claude/standards/harness-cli.md`](../../.claude/standards/harness-cli.md) — the canonical operational gotchas reference. Read this before suspecting a bug.
- [`.claude/skills/harness/SKILL.md`](../../.claude/skills/harness/SKILL.md) — Claude-discoverable wrapper that points here before invoking harness.
- [`.claude/skills/CONVENTIONS.md`](../../.claude/skills/CONVENTIONS.md) — keep `harness` as the first token in every Bash invocation so the allowlist matches.
