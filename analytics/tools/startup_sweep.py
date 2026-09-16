#!/usr/bin/env python3
"""startup_sweep — run a forward-buffer-cap sweep and emit one cross-comparison.

Sweeps the startup forward-buffer-cap knobs (is.flag.startup_forward_buffer_s +
is.flag.startup_fwd_release, driven via CHAR_FWD_BUFFER_S / CHAR_FWD_RELEASE) over
a segment × value × release matrix, runs each config through `harness char matrix`
(pyramid-2sim-{s6,s2}), then pulls the per-play metrics and prints a table:

  TTFF        — video_first_frame_time_ms (first frame rendered)
  keepup/start— video_start_time_ms (playback start; ≈ AVMetric InitialLikelyToKeepUp)
  shifts      — profile_shift_count (ABR variant switches)
  stalls      — stalling_count + stall_duration_ms (mid-play rebuffers)
  quality     — time-weighted mean delivered bitrate (from time_per_variant_s)

One config = one `char matrix` run = a control+slave pair (two replicate arms).

Modes:
  --run            install the app on the sims, run the matrix, then report.
  (default)        report-only over a saved sidecar (--in plays.json).

Examples:
  analytics/tools/startup_sweep.py --insecure --run \
     --configs s6:6 s6:12 s6:18 s2:2 s2:4 s2:6 --release ttff keepup \
     --duration-s 120 --out /tmp/sweep.md

  analytics/tools/startup_sweep.py --insecure --in /tmp/sweep_plays.json
"""
import argparse, json, os, re, subprocess, sys

DEFAULT_BASE = os.environ.get("HARNESS_BASE_URL", "https://dev.jeoliver.com:21000")
HARNESS = os.environ.get("HARNESS_BIN", "harness")
APP_DEFAULT = "apple/InfiniteStreamPlayer/build/Build/Products/Debug-iphonesimulator/InfiniteStreamPlayer.app"
BUNDLE_ID = "com.jeoliver.InfiniteStreamPlayer"
SIMS_DEFAULT = [
    "4D62CB39-BAB7-4294-99D7-8E28FBCD0FF0",
    "0EA208D3-6E04-48BF-8309-F6ACAF383A59",
    "7C6110A4-754C-47DA-B225-E95ED11F9F60",
    "B3A40CBF-87F4-414C-9C01-6A756060DBDF",
]
MATRIX = {"s6": "tests/characterization/matrix/pyramid-2sim-s6.yaml",
          "s2": "tests/characterization/matrix/pyramid-2sim-s2.yaml",
          "ll": "tests/characterization/matrix/pyramid-2sim-ll.yaml"}
RESULT_RE = re.compile(r"ARM \d+ RESULT player_id=(\S+) play_id=(\S+)")


def harness(base, insecure, args, asjson=True, retries=5):
    cmd = [HARNESS, "--base", base] + (["--insecure"] if insecure else [])
    if asjson:
        cmd.append("--json")
    cmd += args
    for _ in range(retries):
        p = subprocess.run(cmd, capture_output=True, text=True)
        if p.stdout.strip():
            return p.stdout
    return ""


def kbps_from_variant_key(k):
    m = re.search(r"@(\d+)kbps", k)
    return int(m.group(1)) if m else None


def play_metrics(base, insecure, pid):
    """All five metrics for one play, or None if no rows."""
    out = harness(base, insecure, ["query", "events", pid, "--limit", "1000"])
    if not out:
        return None
    items = json.loads(out).get("items", [])
    pms = [it["player_metrics"] for it in items if it.get("player_metrics")]
    if not pms:
        return None
    pms.sort(key=lambda m: m.get("event_time", ""))

    def first_nonzero(field):
        for m in pms:
            v = m.get(field)
            if v:
                return v
        return None

    def maxint(field):
        vals = [m.get(field) for m in pms if isinstance(m.get(field), (int, float))]
        return max(vals) if vals else 0

    ttff = first_nonzero("video_first_frame_time_ms")
    vstart = first_nonzero("video_start_time_ms")
    # quality: last row carries the cumulative time_per_variant_s map.
    tpv = {}
    for m in reversed(pms):
        raw = m.get("time_per_variant_s")
        if raw:
            try:
                tpv = json.loads(raw) if isinstance(raw, str) else raw
            except json.JSONDecodeError:
                tpv = {}
            if tpv:
                break
    num = den = 0.0
    for k, secs in tpv.items():
        kb = kbps_from_variant_key(k)
        if kb and secs:
            num += secs * kb
            den += secs
    quality_mbps = (num / den / 1000.0) if den else None
    return {
        "ttff_s": (ttff / 1000.0) if ttff else None,
        "start_s": (vstart / 1000.0) if vstart else None,
        "shifts": maxint("profile_shift_count"),
        "stalls": maxint("stalling_count"),
        "stall_ms": maxint("stall_duration_ms"),
        "play_s": round(den, 1),
        "quality_mbps": quality_mbps,
    }


def keepup_s(base, insecure, pid):
    """AVMetric InitialLikelyToKeepUp, seconds from play start (or None)."""
    out = harness(base, insecure, ["query", "avmetrics", pid, "--event-type",
                                   "AVMetricPlayerItemInitialLikelyToKeepUpEvent", "--limit", "10"])
    if not out:
        return None
    out = out.strip()
    try:
        d = json.loads(out)
        rows = d.get("items", d) if isinstance(d, dict) else d
    except json.JSONDecodeError:
        rows = [json.loads(l) for l in out.splitlines() if l.strip()]
    # need play start: pull from the first event row's date vs ts is awkward;
    # video_start already approximates keepup, so this is a cross-check only.
    ts = sorted(int(r["event_ts_ms"]) for r in rows if r.get("event_ts_ms"))
    return ts[0] if ts else None


def run_matrix(args, seg, fwd_s, release):
    """One harness char matrix run; returns [(player_id, play_id), ...]."""
    env = dict(os.environ)
    env["CHAR_DEVICE_FARM"] = "1"
    env["CHAR_FWD_BUFFER_S"] = str(fwd_s)
    env["CHAR_FWD_RELEASE"] = release
    cmd = [HARNESS, "--base", args.base] + (["--insecure"] if args.insecure else []) + [
        "char", "matrix", MATRIX[seg], "--char-dir", "tests/characterization",
        "--duration-s", str(args.duration_s)]
    print(f"  -> RUN seg={seg} fwd={fwd_s}s release={release} ...", flush=True)
    p = subprocess.run(cmd, env=env, capture_output=True, text=True)
    pairs = RESULT_RE.findall(p.stdout + p.stderr)
    if "PASS" not in p.stdout and "PASS" not in p.stderr:
        print(f"     WARN: run may have failed (no PASS). pairs={len(pairs)}", flush=True)
    print(f"     got {len(pairs)} arm(s): {[pp[1][:8] for pp in pairs]}", flush=True)
    return pairs


def install_app(app, sims):
    for u in sims:
        subprocess.run(["xcrun", "simctl", "terminate", u, BUNDLE_ID],
                       capture_output=True)
        r = subprocess.run(["xcrun", "simctl", "install", u, app], capture_output=True)
        print(f"  installed {u[:8]} exit={r.returncode}", flush=True)


def terminate_app(sims):
    """Option 1: kill the app PROCESS on every sim (not the binary). Clears a
    wedged/leftover play session before the next config without a reinstall (the
    build is unchanged mid-sweep) or a reboot (which would cold-boot and confound
    startup timing). Fleet warmth is preserved, so back-to-back configs stay
    comparable."""
    for u in sims:
        subprocess.run(["xcrun", "simctl", "terminate", u, BUNDLE_ID], capture_output=True)


def config_health(metrics_list):
    """Option 2: is this config's run trustworthy? Returns (ok, reason).
    Rejected (→ retry) ONLY when an arm genuinely failed to play. A fast first
    frame is NOT a wedge — a sub-second TTFF on a fat/flat link is the ideal
    outcome, so we key on 'did the playhead actually move', not on TTFF:
      • missing metrics / no TTFF / no playback start
      • played < MIN_PLAY_S — never sustained playback (the real wedge: e.g. LL
        sitting in buffering at position 0)
      • stall > 20s — a startup wedge (real mid-play stalls are a few s)
    Fast TTFF + real playback, and large TTFF spread between arms, pass through
    as real data."""
    MIN_PLAY_S = 10.0
    ms = [m for m in metrics_list if m]
    if len(ms) < len(metrics_list) or not ms:
        return False, "missing metrics for an arm"
    for m in ms:
        if m.get("ttff_s") is None or m.get("start_s") is None:
            return False, "no TTFF / no playback start"
        if (m.get("play_s") or 0) < MIN_PLAY_S:
            return False, f"played {m.get('play_s', 0):.0f}s (<{MIN_PLAY_S:.0f}s — never sustained playback)"
        if (m.get("stall_ms") or 0) > 20000:
            return False, f"stall {m['stall_ms'] / 1000:.0f}s (startup wedge)"
    return True, "ok"


def fmt(v, prec=2, dash="-"):
    return (f"%.{prec}f" % v) if isinstance(v, (int, float)) else dash


def report(rows, out_path):
    hdr = ("| config | release | play_id | TTFF (s) | start (s) | shifts | stalls | "
           "stall (s) | played (s) | quality (Mbps) | note |")
    sep = "|" + "|".join(["---"] * 11) + "|"
    lines = [hdr, sep]
    for r in rows:
        m = r["metrics"] or {}
        lines.append("| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |" % (
            r["config"], r["release"], r["play_id"][:8],
            fmt(m.get("ttff_s")), fmt(m.get("start_s")),
            m.get("shifts", "-"), m.get("stalls", "-"),
            fmt((m.get("stall_ms") or 0) / 1000.0),
            fmt(m.get("play_s"), 0), fmt(m.get("quality_mbps")),
            r.get("note", ""),
        ))
    table = "\n".join(lines)
    print("\n" + table + "\n")
    if out_path:
        with open(out_path, "w") as f:
            f.write(table + "\n")
        print(f"wrote {out_path}")


def run_config(args, seg, fwd_s, release, cfg):
    """Run one config to a healthy result: terminate (opt 1) → run → extract →
    health-check (opt 2) → retry on wedge/disagreement. Returns metric rows."""
    last = []
    for attempt in range(1, args.max_retries + 2):
        terminate_app(args.sims)
        pairs = run_matrix(args, seg, fwd_s, release)
        metrics = [play_metrics(args.base, args.insecure, pid) for _, pid in pairs]
        ok, reason = config_health(metrics) if pairs else (False, "no arms returned")
        rows = [{"config": cfg, "release": release, "player_id": pl, "play_id": pid,
                 "metrics": m, "note": ""} for (pl, pid), m in zip(pairs, metrics)]
        if ok:
            if attempt > 1:
                print(f"     OK on attempt {attempt}", flush=True)
            return rows
        print(f"     UNHEALTHY ({reason}) — attempt {attempt}/{args.max_retries + 1}", flush=True)
        last = rows
    print(f"     giving up after {args.max_retries + 1} attempts; keeping last (UNSTABLE)", flush=True)
    for r in last:
        r["note"] = "UNSTABLE: " + reason
    return last


def main():
    ap = argparse.ArgumentParser(description="Forward-buffer-cap sweep + report.")
    ap.add_argument("--base", default=DEFAULT_BASE)
    ap.add_argument("--insecure", action="store_true")
    ap.add_argument("--run", action="store_true", help="install + run the matrix, then report")
    ap.add_argument("--configs", nargs="+", default=["s6:6", "s6:12", "s6:18", "s2:2", "s2:4", "s2:6"],
                    help="seg:fwd_s tokens, e.g. s6:6 s2:4")
    ap.add_argument("--release", nargs="+", default=["ttff"], help="release triggers, e.g. ttff keepup")
    ap.add_argument("--duration-s", type=int, default=120)
    ap.add_argument("--app", default=APP_DEFAULT)
    ap.add_argument("--sims", nargs="+", default=SIMS_DEFAULT)
    ap.add_argument("--out", default=None, help="write markdown table here")
    ap.add_argument("--in", dest="infile", default=None, help="report-only: load saved plays.json")
    ap.add_argument("--save", default=None, help="save collected plays.json (run mode)")
    ap.add_argument("--reps", type=int, default=1, help="repeat each config N times (variance estimate)")
    ap.add_argument("--max-retries", type=int, default=2, help="retries when a config wedges/disagrees")
    args = ap.parse_args()

    rows = []
    if args.run:
        # Deploy the (fixed) build ONCE. Between configs we only terminate the app
        # process (opt 1) — no reinstall (binary unchanged) and no reboot (cold-boot
        # confounds startup timing). config health-checks + retries (opt 2) reject
        # wedged/disagreeing runs. Configs run back-to-back in one fleet state, so
        # the comparison is RELATIVE-valid even if absolute startup drifts.
        install_app(args.app, args.sims)
        for release in args.release:
            for tok in args.configs:
                seg, fwd_s = tok.split(":")
                cfg = f"{seg}:{fwd_s}s"
                for rep in range(args.reps):
                    if args.reps > 1:
                        print(f"  [{cfg} {release} rep {rep + 1}/{args.reps}]", flush=True)
                    rows.extend(run_config(args, seg, fwd_s, release, cfg))
        if args.save:
            json.dump([{k: r[k] for k in ("config", "release", "player_id", "play_id")} for r in rows],
                      open(args.save, "w"), indent=2)
            print(f"saved {args.save}")
    elif args.infile:
        for c in json.load(open(args.infile)):
            rows.append({**c, "metrics": play_metrics(args.base, args.insecure, c["play_id"]),
                         "note": c.get("note", "")})
    else:
        ap.error("nothing to do: pass --run or --in plays.json")

    report(rows, args.out)


if __name__ == "__main__":
    main()
