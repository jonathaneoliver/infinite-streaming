# docs/ — operator & reference documentation

Longer-form reference docs that don't belong in `CLAUDE.md` (the orientation
map and knowledge-routing table) or in `.claude/standards/` (terse
mid-investigation cheat sheets). Read these when you need depth on a
subsystem.

## Operator & subsystem reference

| Doc | What it covers |
|-----|----------------|
| [ARCHITECTURE.md](ARCHITECTURE.md) | Service topology — go-live / go-upload / go-proxy / nginx, and how a request flows through them |
| [API.md](API.md) | HTTP API reference across the services (endpoints, params, responses) |
| [DEPLOYMENT.md](DEPLOYMENT.md) | Deploy procedures and environment layout (Docker Compose, test-dev, k3d dev/release) |
| [FAULT_INJECTION.md](FAULT_INJECTION.md) | Fault-injection reference — failure types and how the proxy applies them. Pairs with [`.claude/standards/fault-injection-wire-contract.md`](../.claude/standards/fault-injection-wire-contract.md), which is the on-the-wire contract |
| [live-offset-testing.md](live-offset-testing.md) | The two independent distance-from-live-edge levers (manifest/proxy vs app override), which fields each moves, and how to validate a run |
| [TLS.md](TLS.md) | TLS & certificate setup |
| [TROUBLESHOOTING.md](TROUBLESHOOTING.md) | Common failure modes and how to diagnose them |

## Design notes & specs

These record intent rather than current behaviour. **Check the status line at
the top of each before treating it as a description of what ships** — several
are partially implemented or design-only.

| Doc | What it covers |
|-----|----------------|
| [EVENT_TAXONOMY.md](EVENT_TAXONOMY.md) | Proposed event reclassification replacing the 2-way cause/effect axis the session dashboards use. *Partially implemented.* |
| [characterization-results-design.md](characterization-results-design.md) | What characterization runs should produce and how the existing pieces relate. *Design captured; no build started.* |
| [sweep-design.md](sweep-design.md) | Automated fault-injection sweep design (issue #772). *Design approved; build landed — see QE Lab.* |

Also here: `network-log-demo.html` (a standalone Network Log demo page) and
`screenshots/`.

## Related doc sets elsewhere in the repo

- **`.claude/standards/`** — terse, operationally-relevant cheat sheets read
  mid-investigation, plus longer reference catalogues dual-consumed by the
  dashboard chat bot via `read_standard()`. Index:
  [`.claude/standards/README.md`](../.claude/standards/README.md).
- **`.claude/findings/`** — confirmed/suspected causes captured across
  sessions, one file per observed behaviour. Index:
  [`.claude/findings/README.md`](../.claude/findings/README.md).
- **`generate_abr/`** — encoding-pipeline docs (ABR ladder, packager
  comparisons, encoder validation/burn-in). Note the built-in encoder is the
  fallback, not the primary — start at
  [`generate_abr/README.md`](../generate_abr/README.md).
- **`android/InfiniteStreamPlayer/`** — Android client docs:
  [`README`](../android/InfiniteStreamPlayer/README.md),
  [`BEHAVIOR`](../android/InfiniteStreamPlayer/BEHAVIOR.md),
  [`TESTING`](../android/InfiniteStreamPlayer/TESTING.md),
  [`UI-LAYOUT`](../android/InfiniteStreamPlayer/UI-LAYOUT.md).
- **`tests/characterization/README.md`** — the player ABR characterization
  framework, its modes, and the launch-mode picker.
- **`analytics/README.md`** — analytics sidecar ops, ClickHouse schema, and
  the WAN-deploy auth runbook.
- **`PRD.md`** (repo root) — product-behaviour source of truth; read before
  any UI or product-behaviour change.
