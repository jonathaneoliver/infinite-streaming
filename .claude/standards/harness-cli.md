# harness CLI

The `harness` CLI is the canonical operator surface for the test rig. Read this before reaching for `harness --help` — the help text lists commands but doesn't surface the flag-name traps below.

Install: `make harness-cli` puts it on `$PATH`. Pointed at test-dev (self-signed cert) by default; use `--insecure` on every call or export `HARNESS_INSECURE=1`.

## Flag-name traps

- **Label filters are `--label-has` / `--label-not`, NOT `--label`.** Used by every `query` subcommand that filters on row labels. The repeatable form is AND-semantics: `--label-has info=test_rampup --label-has info=platform_iphone`.
- **`--insecure` skips TLS verification — required against test-dev's self-signed cert.** Drop it and you get `tls: failed to verify certificate for dev.jeoliver.com`. Export `HARNESS_INSECURE=1` so it's automatic.
- **`--json` makes success output JSON. Errors still come out as plain-text `error: …` on stdout, NOT stderr.** Piping straight to `jq` therefore explodes with `parse error: Invalid numeric literal at line 1, column N`. Run the raw command first; only add `| jq` once you've seen JSON come back.

## Subcommand-name traps

- **Read queries against the forwarder archive live under `harness query …` (alias `q`), NOT `harness archive …`.** There is no `archive` subcommand. The query family covers `plays`, `play`, `events`, `network`, `control`, `aggregate`, `heatmap`, `bundle`.
- **`harness query control <play_id>` takes a play_id positionally but the underlying endpoint requires `player_id`.** Until [#TBD] is fixed, this fails with `400: player_id required`. Workaround: `harness raw GET "/analytics/api/v2/control_events?player_id=<PLR>&play_id=<PLY>&limit=N"`.

## Output contract

- Success: human-readable by default. With `--json`: a single JSON document for one-shot commands; newline-delimited JSON for streaming (`ts`, `tail`, `events`).
- Errors: plain text on stdout, exit non-zero. Don't pipe `--json` output to `jq` unblockingly — wrap in `2>/dev/null` and a presence check, or test the raw output first.

## Targets

Most subcommands take a target. The resolver accepts (any of):
- Full UUID
- 6+ char hex prefix
- Label value (e.g. `device=ipad` → the player labelled that)
- Player IP
- User-Agent substring

If no live player matches, the resolver fails fast — most commands that need an existing player short-circuit before any side effect.

## Operator-set labels

`harness labels set <player> k=v k2=v2` writes onto the player record. They:
1. Live on the player while it's heartbeating.
2. Propagate to the play's `labels[]` column via a `label_changed` control event (#487 fix).
3. Appear in `query plays` and the Sessions dashboard's Labels column as `info=<key>_<value>` chips.

Note the encoding: `test=rampup` on the player → `info=test_rampup` on the row. The filter form is `query plays --label-has info=test_rampup`.

## Common patterns

```sh
# Survey live players
harness --insecure --json players list | jq '.[] | {id, user_agent, state}'

# Show one player by short prefix
harness --insecure players show 3bff77d6

# Tail combined event/network/control SSE for one player
harness --insecure ts 3bff77d6 --streams events,network,control

# Find every play tagged by a characterization run
harness --insecure --json query plays \
    --label-has info=test_rampup \
    --label-has info=platform_iphone \
    --limit 20

# Inspect one play's events + label histogram
harness --insecure --json query play <play_id>

# Apply a network cap
harness --insecure shape ipad --rate 2.5

# Set a player label (overnight workflow uses this)
harness --insecure labels set <player> test=rampup run_id=20260521T160000Z
```

## See also

- `tools/harness-cli/README.md` — full subcommand surface, install, examples.
- `.claude/skills/harness/SKILL.md` — Claude-discoverable wrapper that points here before invoking harness.
- `.claude/skills/CONVENTIONS.md § 1. Bash command discipline` — keep `harness` as the first token in every Bash invocation so the allowlist matches.
