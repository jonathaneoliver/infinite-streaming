#!/usr/bin/env python3
"""Startup characterization report — parts 2 (per-play metrics) + 3 (aggregate reps + render).

One script that turns the rep->play maps produced by the sweep driver (part 1)
into the self-contained HTML report. It pulls each play's events from the
archive via `harness query events`, derives the startup metrics, aggregates the
reps per cell, and injects the result into report.template.html.

Map dir layout (default: $CLAUDE_JOB_DIR/tmp if set, else CWD):
  rep{1..N}_seg_{s6,s2,s1}.tsv   limit_mbps<TAB>cap_mbps<TAB>play_id   (one file per rep)
  play_player_map.tsv            play_id<TAB>player_id                 (for session-viewer links)
n=1 fallback: if no rep*_seg_* files exist, startup_seg_{seg}.tsv is read as the single rep.

Queried events are cached to <maps>/.metrics_cache.json keyed by play_id, so
re-rendering (e.g. after a template tweak) is instant. Delete the cache to re-pull.

Usage:
  startup_report.py [--maps DIR] [--reps N] [--segs s6,s2,s1] [--limits 0,20,16,8,2]
                    [--caps 0,1,2,8,16] [--out FILE] [--host URL] [--no-cache]

Derived metrics (per play): video-start (playhead moving), TTFF (first frame
decoded), first-fetched rung, shown rung, variant@60s, time-to-settle, up-shifts,
stalls, startup-efficiency (bitrate-utilization %), and residency (fetched + shown).
See .claude/findings/startup-initial-cap-vs-network-limit-2026-07-12.md.
"""
import argparse, json, os, subprocess, statistics as st, sys
from collections import Counter
from datetime import datetime

TOP = 33.808  # top ladder rung (Mbps) for insane_newer_p200_h264

# ---------------------------------------------------------------- part 2: metrics
def _t(s):
    try:
        return datetime.fromisoformat(s.replace("Z", "+00:00"))
    except Exception:
        return None


def metrics(play, host):
    """All startup metrics for one play_id, via `harness query events`.

    Returns a dict of derived values, or None if the play couldn't be read.
    """
    env = dict(os.environ, HARNESS_BASE_URL=host)
    try:
        out = subprocess.run(["harness", "query", "events", play, "--limit", "500"],
                             capture_output=True, text=True, env=env, timeout=40).stdout
        items = json.loads(out).get("items", [])
    except Exception:
        return None
    if not items:
        return None
    ttff = vstart = None
    stalls = 0
    startrung = None
    t0 = tmin = tmax = None
    series = []          # (elapsed_s, rendition_mbps)
    samples = []         # (elapsed_s, rendition_mbps, is_playing)
    active = []          # (elapsed_s, rendition_mbps) while actively fetching (not idle)
    for it in items:
        pm = it.get("player_metrics") or {}
        sm = it.get("server_metrics") or {}
        ts = _t(it.get("ts", ""))
        if ts:
            if t0 is None:
                t0 = ts
            if tmin is None or ts < tmin:
                tmin = ts
            if tmax is None or ts > tmax:
                tmax = ts
        f = pm.get("video_first_frame_time_ms")
        if f and ttff is None:
            ttff = f
        v = pm.get("video_start_time_ms")
        if v and vstart is None:
            vstart = v
        sc = pm.get("stalling_count")
        if isinstance(sc, (int, float)):
            stalls = max(stalls, sc)
        r = sm.get("rendition_mbps")
        state = pm.get("state")
        if state == "playing" and isinstance(r, (int, float)) and startrung is None:
            startrung = r
        if ts and t0 and isinstance(r, (int, float)):
            e = (ts - t0).total_seconds()
            series.append((e, r))
            samples.append((e, r, state == "playing"))
            if r > 0.5 and state not in (None, "idle"):
                active.append((e, r))
    samples.sort()
    var60 = None
    if series:
        best = min(series, key=lambda x: abs(x[0] - 60))
        if abs(best[0] - 60) <= 15:
            var60 = best[1]
    vstart_s = (vstart / 1000.0) if vstart else None
    # residency over the first 60 s: dwell (s) per rung while PLAYING + a 'buffer'
    # bucket for non-playing (startup + stalls). startup-eff = time-weighted
    # min(rung, ceiling) / ceiling — bitrate utilization, network drops out.
    dwell = {}
    integ = 0.0
    for i, (e, r, playing) in enumerate(samples):
        if e >= 60:
            break
        nxt = min(samples[i + 1][0], 60.0) if i + 1 < len(samples) else 60.0
        dt = nxt - e
        if dt <= 0:
            continue
        if playing:
            dwell[r] = dwell.get(r, 0.0) + dt
            if var60:
                integ += min(r, var60) * dt
        else:
            dwell['buffer'] = dwell.get('buffer', 0.0) + dt
    quality_pct = 100.0 * integ / (60.0 * var60) if var60 else None
    residency = []
    if dwell.get('buffer'):
        residency.append({"rung": "buffer", "pct": round(100.0 * dwell['buffer'] / 60.0, 1)})
    for r in sorted(k for k in dwell if k != 'buffer'):
        residency.append({"rung": round(r, 3), "pct": round(100.0 * dwell[r] / 60.0, 1)})
    # fetched residency: dwell per rung during ACTIVE fetching (excludes idle default),
    # normalized to the active window so the bar fills. The variant ABR REQUESTED.
    fdwell = {}
    act = [s for s in active if s[0] < 60]
    for i, (e, r) in enumerate(act):
        nxt = min(act[i + 1][0], 60.0) if i + 1 < len(act) else 60.0
        if nxt > e:
            fdwell[r] = fdwell.get(r, 0.0) + (nxt - e)
    ftot = sum(fdwell.values())
    fetched_residency = ([{"rung": round(r, 3), "pct": round(100.0 * val / ftot, 1)}
                          for r, val in sorted(fdwell.items())] if ftot > 0 else [])
    # first-fetched: first rung ABR fetches for a SUSTAINED period (>=0.5s),
    # skipping momentary default-carryover blips.
    first_fetched = None
    for i, (e, r) in enumerate(act):
        nxt = act[i + 1][0] if i + 1 < len(act) else e + 1.0
        if nxt - e >= 0.5:
            first_fetched = r
            break
    if first_fetched is None and fdwell:
        first_fetched = max(fdwell, key=fdwell.get)
    # time-to-settle FROM PLAYBACK: first PLAYING sample at var60, minus video-start.
    t_settle = None
    if var60 is not None and vstart_s is not None:
        hits = [e for (e, r, pl) in samples if pl and abs(r - var60) < 1e-6 and e >= vstart_s - 0.5]
        if hits:
            t_settle = max(0.0, min(hits) - vstart_s)
    # shifts during the climb [video-start, settle], split up / down.
    up = dn = 0
    if var60 is not None and vstart_s is not None:
        end = (vstart_s + t_settle) if t_settle is not None else 60.0
        seq = [r for (e, r, pl) in samples if pl and vstart_s - 0.5 <= e <= end + 1e-6]
        for a, b in zip(seq, seq[1:]):
            if b > a + 1e-6:
                up += 1
            elif b < a - 1e-6:
                dn += 1
    return dict(ttff=ttff, vstart=vstart, stalls=stalls, startrung=startrung,
                first_fetched=first_fetched, var60=var60, t_settle=t_settle,
                quality_pct=quality_pct, shifts_up=up, shifts_down=dn,
                residency=residency, fetched_residency=fetched_residency,
                t_start=(tmin.strftime("%Y%m%dT%H%M%SZ") if tmin else None),
                t_end=(tmax.strftime("%Y%m%dT%H%M%SZ") if tmax else None))


# ------------------------------------------------------- part 3: aggregate + render
# continuous metrics -> mean/min/max/cov ; categorical rungs -> mode/agree/vals
CONT = [("video_start", "vstart", 0.001), ("ttff", "ttff", 0.001),
        ("startup_eff", "quality_pct", 1.0), ("settle", "t_settle", 1.0),
        ("shifts_up", "shifts_up", 1.0), ("stalls", "stalls", 1.0)]
CAT = [("fetched", "first_fetched"), ("shown", "startrung"), ("var60", "var60")]


def avg_resid(lists):
    keys = set()
    for lst in lists:
        keys |= {x["rung"] for x in lst}
    out = []
    for k in keys:
        vals = [{x["rung"]: x["pct"] for x in lst}.get(k, 0.0) for lst in lists]
        out.append({"rung": k, "pct": round(st.mean(vals), 1)})
    out.sort(key=lambda x: (-1e9 if x["rung"] == "buffer" else x["rung"]))
    return out


def load_rep_map(path):
    out = {}
    if os.path.exists(path):
        for line in open(path).read().splitlines()[1:]:
            p = line.split("\t")
            if len(p) == 3:
                out[(p[0], p[1])] = p[2]
    return out


def build_data(maps, reps, segs, limits, caps, host, use_cache):
    # play_id -> player_id (for session-viewer links)
    player = {}
    mp = os.path.join(maps, "play_player_map.tsv")
    if os.path.exists(mp):
        for line in open(mp):
            a = line.rstrip("\n").split("\t")
            if len(a) == 2:
                player[a[0]] = a[1]

    cache_path = os.path.join(maps, ".metrics_cache.json")
    cache = {}
    if use_cache and os.path.exists(cache_path):
        try:
            cache = json.load(open(cache_path))
        except Exception:
            cache = {}

    def metrics_cached(play):
        if play in cache:
            return cache[play]
        m = metrics(play, host)
        cache[play] = m           # cache None too — a dead play won't re-query every run
        return m

    def rep_files(rep, seg):
        # rep{n}_seg_{seg}.tsv, with startup_seg_{seg}.tsv as the n=1 fallback.
        cand = os.path.join(maps, "rep%d_seg_%s.tsv" % (rep, seg))
        if os.path.exists(cand):
            return cand
        return os.path.join(maps, "startup_seg_%s.tsv" % seg)

    out = {}
    for seg in segs:
        rep_maps = {r: load_rep_map(rep_files(r, seg)) for r in range(1, reps + 1)}
        out[seg] = {}
        for L in limits:
            for C in caps:
                plays = [rep_maps[r].get((L, C)) for r in range(1, reps + 1)]
                plays = [p for p in plays if p and p != "NONE"]
                ms = [(p, metrics_cached(p)) for p in plays]
                ms = [(p, m) for p, m in ms if m]
                if not ms:
                    out[seg]["%s|%s" % (L, C)] = None
                    continue
                cell = {"n": len(ms),
                        "plays": [{"play": p, "player": player.get(p, "")} for p, _ in ms]}
                for disp, key, sc in CONT:
                    vals = [m.get(key) * sc for _, m in ms if m.get(key) is not None]
                    if vals:
                        mean = st.mean(vals)
                        cov = 100 * st.pstdev(vals) / mean if mean else 0.0
                        cell[disp] = {"vals": [round(v, 2) for v in vals], "mean": round(mean, 2),
                                      "min": round(min(vals), 2), "max": round(max(vals), 2),
                                      "cov": round(cov, 1)}
                for disp, key in CAT:
                    vals = [m.get(key) for _, m in ms if m.get(key) is not None]
                    if vals:
                        cell[disp] = {"vals": vals, "mode": Counter(vals).most_common(1)[0][0],
                                      "agree": len(set(vals)) == 1}
                cell["residency"] = avg_resid([m.get("residency", []) for _, m in ms])
                cell["fetched_residency"] = avg_resid([m.get("fetched_residency", []) for _, m in ms])
                # compare URL: overlay all reps in session-viewer (?compare=player~play~idx,...)
                starts = [m.get("t_start") for _, m in ms if m.get("t_start")]
                ends = [m.get("t_end") for _, m in ms if m.get("t_end")]
                if len(ms) >= 2 and starts and ends and all(player.get(p) for p, _ in ms):
                    raw = ",".join("%s~%s~%d" % (player[p], p, i + 1) for i, (p, _) in enumerate(ms))
                    comp = raw.replace("~", "%7E").replace(",", "%2C")
                    p0 = ms[0][0]
                    cell["compare_url"] = ("%s/dashboard/session-viewer.html"
                        "?player_id=%s&play_id=%s&from=%s&to=%s&compare=%s"
                        % (host, player[p0], p0, min(starts), max(ends), comp))
                out[seg]["%s|%s" % (L, C)] = cell

    if use_cache:
        try:
            json.dump(cache, open(cache_path, "w"))
        except Exception:
            pass
    return out


def render(data, template_path, out_path):
    tmpl = open(template_path).read()
    html = tmpl.replace("__DATA__", json.dumps(data))
    open(out_path, "w").write(html)


def main():
    default_maps = os.path.join(os.environ["CLAUDE_JOB_DIR"], "tmp") \
        if os.environ.get("CLAUDE_JOB_DIR") else "."
    here = os.path.dirname(os.path.abspath(__file__))
    ap = argparse.ArgumentParser(description="Startup characterization report (parts 2+3).")
    ap.add_argument("--maps", default=default_maps, help="dir with rep{N}_seg_{seg}.tsv + play_player_map.tsv")
    ap.add_argument("--reps", type=int, default=3)
    ap.add_argument("--segs", default="s6,s2,s1")
    ap.add_argument("--limits", default="0,20,16,8,2")
    ap.add_argument("--caps", default="0,1,2,8,16")
    ap.add_argument("--host", default="https://dev.jeoliver.com:21000")
    ap.add_argument("--template", default=os.path.join(here, "report.template.html"))
    ap.add_argument("--out", default=os.path.join(here, "startup-report.html"))
    ap.add_argument("--no-cache", action="store_true", help="ignore + overwrite the metrics cache")
    a = ap.parse_args()

    data = build_data(a.maps, a.reps, a.segs.split(","), a.limits.split(","),
                      a.caps.split(","), a.host, not a.no_cache)
    n = sum(1 for seg in data.values() for c in seg.values() if c)
    if n == 0:
        sys.exit("no cells resolved — check --maps %s (rep*_seg_*.tsv present?)" % a.maps)
    render(data, a.template, a.out)
    print("wrote %s  (%d cells across %d segments, reps=%d)" % (a.out, n, len(data), a.reps))


if __name__ == "__main__":
    main()
