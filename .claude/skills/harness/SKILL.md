---
name: harness
description: Wrapper for the harness CLI. Read before invoking any `harness` command — surfaces flag-name traps, output-contract gotchas, and the canonical operator-label propagation flow. Sibling skills (triage, investigate, forensics, fault, shape, finding) all shell out to harness; this is the source of truth for HOW.
last_reviewed: 2026-05-21
---

# Skill: using the `harness` CLI

`harness` is the operator surface for the test rig — every mutation, every archive read, every live SSE stream goes through it. Sibling skills assume it works. This file is what to read before guessing flag names or pipe shapes.

## Before invoking harness

1. **First-token rule** — every `Bash` invocation MUST start with `harness` (see [CONVENTIONS.md § 1](../CONVENTIONS.md)). Prefixing with `VAR=…`, `cd …`, or `echo "header"; harness …` bypasses the allowlist and re-prompts the operator. Inline target values; one command per Bash call.
2. **`--insecure` is required against test-dev** (self-signed cert). The user usually has `HARNESS_INSECURE=1` exported, but be explicit — defensive programming against unset env.
3. **`--json` for parseable output**, but read the next bullet first.
4. **`--json` errors come out as plain text on stdout, NOT stderr.** Piping `--json | jq` will explode on errors with `parse error: Invalid numeric literal`. Either capture the output first and check for `error:`, or stick to `--json` only when you've already verified the command shape works.

If you don't know a flag name, read [`tools/harness-cli/README.md`](../../../tools/harness-cli/README.md) BEFORE retrying with a guess. The README has a per-subcommand quick reference; `harness <sub>` with no args prints per-subcommand usage. The errors `harness` emits on bad flags are loud but truncated — they list ALL flags for that subcommand, which is the source of truth.

## Don't get caught by these

These have all bitten real conversations; the operational reference is at [`.claude/standards/harness-cli.md`](../../standards/harness-cli.md):

- **Label filters are `--label-has` / `--label-not`** (repeatable, AND semantics), NOT `--label`.
- **No `harness archive …` subcommand exists.** Forwarder archive reads live under `harness query …` (alias `q`).
- **`harness query control <play_id>` is broken in current main** — the underlying endpoint requires `player_id`. Workaround: `harness raw GET "/analytics/api/v2/control_events?player_id=<PLR>&play_id=<PLY>&limit=N"`.
- **Operator labels round-trip through encoding.** `harness labels set <player> test=foo` lands as `info=test_foo` on the play's `labels[]`. The filter form is `--label-has info=test_foo`, NOT `--label-has test=foo`.

## When to use this vs sibling skills

`harness` is the CLI. The sibling skills wrap it for specific operator workflows:

| If the user asked … | Skill |
|---|---|
| "what's broken right now?" | `triage` |
| "why did X happen at time T?" | `investigate` |
| "why does this pattern keep recurring?" | `forensics` |
| "fail this player's manifests / segments" | `fault` |
| "throttle / shape this player's network" | `shape` |
| "capture what we just learned" | `finding` |
| (anything else involving harness — e.g. raw queries, label setting, group management, checkpoints) | use harness directly, citing the standards file |

This skill exists for the "anything else" row, and as a cite-target for the sibling skills.

## Common patterns (cite the README for more)

```sh
# Survey live players
harness --insecure --json players list | jq -r '.[] | "\(.id[:8])  \(.user_agent[:40])"'

# Find runs of a characterization test
harness --insecure --json query plays \
    --label-has info=test_rampup \
    --label-has info=platform_iphone \
    --limit 20

# One play's events + label histogram
harness --insecure --json query play <play_id>

# Tail combined SSE for one player
harness --insecure ts <player> --streams events,network,control

# Mutate (snapshot is automatic — undo replays the most recent)
harness --insecure labels set <player> k=v
harness --insecure shape <player> --rate 2.5
harness --insecure undo
```

## See also

- [`tools/harness-cli/README.md`](../../../tools/harness-cli/README.md) — full subcommand surface + flags
- [`.claude/standards/harness-cli.md`](../../standards/harness-cli.md) — canonical gotchas, single source of truth
- [`.claude/skills/CONVENTIONS.md`](../CONVENTIONS.md) — first-token rule + no-guessing
