---
name: shape
description: Set bandwidth cap, delay, packet loss, or pattern-based ramp on a player session via the harness CLI. Invoke when the user says "throttle to X Mbps", "add N ms delay", "X percent loss", "ramp up over 30s", "drop-and-recover pattern", "shape the network", or otherwise wants kernel-level traffic shaping. This skill handles unit normalisation and routes ramp-style asks to the `abr-sweep` procedure.
---

# Network shaping via `harness shape` + `harness procedure abr-sweep`

Defaults: target test-dev; snapshot-protected; `harness undo <target>` rolls back any shape change.

## Single static shape

```sh
harness --insecure shape <target> [--rate Mbps] [--delay ms] [--loss pct]
```

All three axes default to "don't change". Pass at least one. The CLI sends a merge-patch — fields you omit stay at their current value.

| User says | Command |
|---|---|
| "throttle to 1.5 Mbps" | `harness shape ipad --rate 1.5` |
| "add 100 ms delay" | `harness shape ipad --delay 100` |
| "5 percent packet loss" | `harness shape ipad --loss 5` |
| "1 Mbps with 50 ms delay and 1% loss" | `harness shape ipad --rate 1 --delay 50 --loss 1` |
| "show me what's shaped" | `harness shape ipad --show` |
| "clear shaping" | `harness shape ipad --clear` |

## Unit normalisation

Same table as `fault`'s — repeated here so this skill is self-contained:

| User says | `--rate` value |
|---|---|
| "5mb", "5 megabits", "5 Mbps", "5M" | `5` |
| "100k", "100 kbps", "100 Kbits" | `0.1` |
| "1 gig", "1 Gbps" | `1000` |

| User says | `--delay` value |
|---|---|
| "100ms" | `100` |
| "0.1s", "100 milliseconds" | `100` |
| "1 second" | `1000` |

| User says | `--loss` value |
|---|---|
| "5%", "5 percent" | `5` |
| "half a percent", "0.5%" | `0.5` |

For ambiguous units ("100k") pick the streaming-plausible value (kbps for bandwidth — 0.1 Mbps) and tell the user what you assumed.

## Ramp / pattern shaping → use `procedure abr-sweep`

The `harness shape` command sets one static value. For time-varying patterns ("ramp from 5 to 1 Mbps over 30 seconds", "square wave between 1 and 5 Mbps every 10 seconds"), use the procedure wrapper:

```sh
harness procedure abr-sweep <target> --rates 5,2,1,0.5 --hold 60s
```

Holds 5 Mbps for 60s, then 2 Mbps for 60s, then 1, then 0.5. Clears shape on exit / Ctrl-C.

| User says | Command |
|---|---|
| "ramp down from 5 to 1 Mbps over 30s" | `harness procedure abr-sweep ipad --rates 5,3,1 --hold 10s` |
| "step down through the ladder in 20s intervals" | `harness procedure abr-sweep ipad --rates 30,15,7,3,1 --hold 20s` |
| "drop to 0 then back up to 5 over 2 minutes" | `harness procedure abr-sweep ipad --rates 5,0,2,5 --hold 30s` |

For tighter timing or non-uniform durations, drop down to `harness raw PATCH` with the Shape's `pattern.steps` array directly — see the OpenAPI schema for the structure. This skill defers to `abr-sweep` for the common cases.

## Confirm before destructive

These bork the player visibly — always confirm before running:

- `--rate 0` (bandwidth = 0 → player stalls within seconds)
- `--loss 100` (100% loss → player sees connection timeouts)
- `--clear` while a soak / abr-sweep is mid-run (loses the in-flight pattern)

For one-off "I want to break it briefly", prefer `harness procedure abr-sweep --rates 0,5 --hold 10s` over a manual `--rate 0` + `--clear` — the procedure handles cleanup even on Ctrl-C.

## After every change

Verify with `--show`:

```sh
harness shape ipad --show
```

The CLI prints the current Shape including any `pattern_rate_runtime_mbps` (the value the kernel is enforcing *right now* if a pattern is active). If the operator's intent was a static rate but you see a non-zero `pattern_step_runtime`, an old pattern is still running — clear it first.

## Common pitfalls

- **Static `--rate` doesn't kill an existing pattern.** If a pattern is active, you have to either `shape --clear` first or send a PATCH with `{"shape":{"pattern":null,"rate_mbps":X}}`. The CLI's `shape --rate` alone is additive over current state.
- **`shape --show` doesn't include the player's chosen variant.** A 5 Mbps shape + a 30 Mbps player choice = the kernel chokes down to 5 Mbps and the player downshifts as a result. That's the normal interaction; if the operator's confused about "why is bandwidth low", check `harness players show <t>` for the player's perspective.
- **Transport faults aren't shape's job.** `harness fault add --type connection_refused` (HTTP layer) vs `transport_fault drop` (kernel layer) — different surfaces, different commands. Transport_fault is currently only reachable via `harness raw PATCH` with the shape body (see `fault` skill).

## What NOT to do here

- **Don't combine `--rate 0` with active fault rules.** Both will mask each other and the operator can't tell which is causing the stall. Either-or.
- **Don't run `abr-sweep` against a player that's also being used for a real test.** The sweep takes exclusive control of the shape; existing shape gets clobbered.
- **Don't suggest patterns when the user wants a one-off change.** "Throttle to 1 Mbps" = `shape --rate 1`. "Ramp through" = `procedure abr-sweep`. Don't escalate.

## See also

- `fault` — for HTTP/socket faults (shaping is kernel-level; fault is application-level)
- `triage` / `investigate` — measure what happened after shaping
- `.claude/standards/abr-decision-model.md` — how the player reacts to a sudden rate cap
