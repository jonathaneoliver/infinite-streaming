# Aberration crawl (#607)

Runs the machine-checkable invariants catalogue (`invariants.yaml`)
against the analytics ClickHouse archive, grouped by the #550 Phase 4
version taxonomy (`app_version × os_version_major × device_class ×
player_tech`). A behaviour regression shows up as "rule R holds
everywhere except app 2.3.1 on tvOS".

Operating manual (validity windows, census-before-assert, NON-rules):
[`.claude/standards/invariants.md`](../../.claude/standards/invariants.md).

## Run

ClickHouse on test-dev is loopback-bound — tunnel first:

```bash
ssh -f -N -L 21123:127.0.0.1:21123 $TEST_SSH   # from .env
cd tests/aberration_crawl
go test -run TestAberrationCensus -v -timeout 20m
```

The test **skips** (not fails) when CH is unreachable, so it's safe in CI.
`TestCatalogueValid` is the CH-free structural check.

## Env vars

| var | default | meaning |
|---|---|---|
| `ABERRATION_CH_URL` | `http://127.0.0.1:21123` | ClickHouse HTTP endpoint |
| `ABERRATION_DB` | `infinite_streaming` | database |
| `ABERRATION_CH_USER` / `_PASSWORD` | _(none)_ | basic auth if the CH user needs it |
| `ABERRATION_MODE` | `census` | `assert` fails on error-severity rules whose catalogue `mode: assert` |
| `ABERRATION_REPORT` | _(unset)_ | path to write the full JSON report |

## Reading the output

- One line per rule×table: `ok` / `N viol` / `SKIP` / `ERROR`, rows
  checked, effective `since` window.
- Per-group detail lines appear only where violations exist — the group
  string is `app|osMajor|class|tech`, or `unversioned` for pre-taxonomy
  rows (before 2026-05-29) and UA-unparsed external players.
- Up to 3 `exemplar` lines per violating rule give `(player_id, play_id,
  ts)` for drill-down via the `investigate` / `forensics` skills.
- `[pending]` = the producing change isn't deployed yet (e.g.
  `play-start-unique` until #604/#605/#606 land); violations expected.

## Adding a rule

Add an entry to `invariants.yaml` (kinds: `row`, `vocab`, `monotonic`,
`sequence`, `play`, `sql` — see the header comment there for the runner
contract). Then:

1. `go test -run TestCatalogueValid` — structural check.
2. Run the census and **look at the violations before believing the
   rule**. Phase 1/2 falsified five documented claims this way (vst/ttff
   ordering, quality_pct range, transfer_ms scope, total_ms formula,
   request_kind vocabulary) — when a predicate fires on >1% of rows,
   suspect the rule (or the doc it came from) before the data.
3. Census-only until a clean calibration pass; only then set
   `mode: assert`.
4. If the rule encodes a date-bound behaviour change, record the
   data-calibrated date in `applicable_since` — git merge dates are
   hints, hot-deploys make them wrong (taxonomy deployed 2 days before
   its PR merged).

`kind: sql` rules are sketch-only in Phase 2 (`sql_sketch` documents the
intended query); the runner logs and skips them. They're implemented in
Phase 3 along with QoE label recomputation.
