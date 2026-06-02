#!/usr/bin/env python3
"""Stall-anchored condition distribution for #508.

For each STALL_START (from session_events) across a set of plays, characterize how the
PLAYLIST and SEGMENT fetches react:
  * playlist-rendition shift  — first media-playlist fetch after the stall vs the
    rendition the player was on when it stalled (Δrungs: down/same/up).
  * segment re-fetch direction — first video segment after the stall: backward segnum
    (re-grab buffer) vs forward; and its Δrungs vs the pre-stall rendition.
  * fault trigger             — was the stall preceded (within --fault-lookback s) by a
    faulted request, and of what class.
  * stall duration            — stall_start → next buffering_end.

READ-ONLY (#508). Renditions are read from URLs (absolute) since the token ΔP is clamped.

Usage: stall_dist.py --plays-file stall_plays.txt [--limit 4000] [--window 8]
"""
import argparse
import collections
import json
import os
import re
import subprocess
import sys
from datetime import datetime, timedelta

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import tokenize as tk  # noqa: E402

SEG = re.compile(r"/(\d{3,4}p)/segment_(\d+)\.m4s")
PL = re.compile(r"playlist[^/]*_(\d{3,4}p)\.m3u8")
ORD = tk.RENDITION_ORDINAL


def parse(ts):
    try:
        return datetime.strptime((ts or "").replace("T", " ").replace("Z", "")[:23], "%Y-%m-%d %H:%M:%S.%f")
    except ValueError:
        return None


def pull(kind, pid, limit):
    f = f"/tmp/{'cnet' if kind == 'network' else 'cev'}_{pid[:8]}.json"
    if os.path.exists(f):
        return json.load(open(f)).get("items", [])
    r = subprocess.run(["harness", "--insecure", "--json", "query", kind, pid, "--limit", str(limit)],
                       capture_output=True, text=True)
    its = json.loads(r.stdout[r.stdout.index("{"):]).get("items", []) if "{" in r.stdout else []
    json.dump({"items": its}, open(f, "w"))
    return its


def rungs(d):
    return f"down {-d}" if d < 0 else (f"up {d}" if d > 0 else "same")


def stall_starts(ev):
    return sorted(t for t in (parse(r.get("ts")) for r in ev
                  if (r.get("player_metrics") or {}).get("last_event") == "stall_start") if t)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--plays-file", required=True)
    ap.add_argument("--limit", type=int, default=4000)
    ap.add_argument("--window", type=float, default=8.0, help="seconds after stall to look for the reaction")
    ap.add_argument("--fault-lookback", type=float, default=4.0)
    ap.add_argument("--max-stall", type=float, default=60.0,
                    help="cap stall_start→buffering_end pairing (s); longer = mispair/abandoned, excluded")
    args = ap.parse_args()

    pl_delta = collections.Counter()
    seg_dir = collections.Counter()
    seg_delta = collections.Counter()
    trigger = collections.Counter()
    durations = []
    nstall = nplays = 0

    for pid in [l.strip() for l in open(args.plays_file) if l.strip()]:
        ev = pull("events", pid, args.limit)
        starts = stall_starts(ev)
        if not starts:
            continue
        nplays += 1
        net = pull("network", pid, args.limit)
        ends = sorted(t for t in (parse(r.get("ts")) for r in ev
                      if (r.get("player_metrics") or {}).get("last_event") == "buffering_end") if t)
        segs, pls, faults = [], [], []
        for r in net:
            u, t, st = r.get("url") or "", parse(r.get("ts")), str(r.get("status"))
            if not t:
                continue
            ms, mp = SEG.search(u), PL.search(u)
            if ms and ms.group(1) in ORD:
                segs.append((t, ms.group(1), int(ms.group(2)), st))
                if st == "404" or st.startswith("5") or r.get("fault_type"):
                    faults.append((t, r.get("fault_category") or st))
            elif mp and mp.group(1) in ORD:
                pls.append((t, mp.group(1)))
        segs.sort(); pls.sort()

        for st in starts:
            nstall += 1
            pre = [s for s in segs if s[0] < st and s[3] == "200"]
            pre_rend = pre[-1][1] if pre else None
            pre_seg = pre[-1][2] if pre else None
            hi = st + timedelta(seconds=args.window)
            fpl = next((p for p in pls if st <= p[0] <= hi), None)
            if fpl and pre_rend:
                pl_delta[rungs(ORD[fpl[1]] - ORD[pre_rend])] += 1
            fseg = next((s for s in segs if st <= s[0] <= hi), None)
            if fseg:
                if pre_seg is not None:
                    seg_dir["backward (re-fetch)" if fseg[2] <= pre_seg else "forward"] += 1
                if pre_rend:
                    seg_delta[rungs(ORD[fseg[1]] - ORD[pre_rend])] += 1
            lo = st - timedelta(seconds=args.fault_lookback)
            ftr = [f for f in faults if lo <= f[0] <= st]
            trigger[f"fault-triggered ({ftr[-1][1]})" if ftr else "no fault in window"] += 1
            fe = next((e for e in ends if e >= st), None)
            if fe and (fe - st).total_seconds() <= args.max_stall:
                durations.append((fe - st).total_seconds())

    def show(title, ctr):
        tot = sum(ctr.values())
        print(f"\n{title}  (n={tot})")
        for k, c in ctr.most_common():
            print(f"    {k:26} {c/tot:5.1%} {'█' * round(28 * c / tot)} ({c})")

    print(f"stall plays: {nplays}  STALL_START events: {nstall}")
    show("playlist rendition shift after stall (vs rendition at stall)", pl_delta)
    show("first segment after stall — direction", seg_dir)
    show("first segment after stall — rendition Δ", seg_delta)
    show("stall trigger", trigger)
    if durations:
        durations.sort()
        med = durations[len(durations) // 2]
        print(f"\nstall duration (stall_start→buffering_end): median {med:.2f}s  "
              f"min {durations[0]:.2f}s  max {durations[-1]:.2f}s  (n={len(durations)})")


if __name__ == "__main__":
    main()
