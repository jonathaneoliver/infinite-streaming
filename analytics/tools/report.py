#!/usr/bin/env python3
"""Streaming report generator (#508).

  python3 analytics/tools/report.py --kind conditions [--days 7] [--engine AVPlayer] [--out f.md]

One umbrella for streaming reports — pick a --kind. Today:
  conditions  what the player does around playback conditions (faults / stalls /
              play-ends): anchor -> episode -> grammar, per the CONDITIONS catalog.

Future kinds (throughput, qoe, the trained anomaly scorer, …) are entries in KINDS, not
new files. NOTE: `vomm` is RESERVED for the trained variable-order scorer (not built
yet) — this `conditions` report is its DESCRIPTIVE PRECURSOR, not the model.

READ-ONLY (#508): reads the archive via the harness CLI; writes only the report file.
"""
import argparse
import collections
import json
import os
import re
import subprocess
import sys
from datetime import datetime, timedelta, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import tokenize as tk  # noqa: E402

SEG = re.compile(r"/(\d{3,4}p)/segment_(\d+)\.m4s")
PL = re.compile(r"playlist[^/]*_(\d{3,4}p)\.m3u8")
ORD = tk.RENDITION_ORDINAL
SEGMENT_KINDS = ("V_SEG", "V_PROBE")


# ---------- shared helpers ----------
def gi(p, k):
    try:
        return int(float(p.get(k, 0)))
    except (TypeError, ValueError):
        return 0


def play_labels(p):
    return [l[0] for l in p.get("label_histogram", [])]


def parse_ts(ts):
    try:
        return datetime.strptime((ts or "").replace("T", " ").replace("Z", "")[:23], "%Y-%m-%d %H:%M:%S.%f")
    except ValueError:
        return None


def pull(kind, pid, limit=5000):
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


def bars(ctr, top=8):
    """Counter -> markdown lines '<label> <pct> (n)'."""
    tot = sum(ctr.values()) or 1
    out = []
    for k, c in ctr.most_common(top):
        out.append(f"- `{k}` — {c / tot:.0%} ({c})")
    return out, sum(ctr.values())


def seg_rows(net):
    """Sorted video-segment rows: (ts, rend, segnum, status)."""
    rows = []
    for r in net:
        m = SEG.search(r.get("url") or "")
        t = parse_ts(r.get("ts"))
        if m and t and m.group(1) in ORD:
            rows.append((t, m.group(1), int(m.group(2)), str(r.get("status"))))
    rows.sort()
    return rows


# ---------- condition: FAULT recovery ----------
def analyze_faults(play_ids):
    committed = collections.Counter()   # V_SEG arrow after FAULT(video_seg,*)
    stair = collections.Counter()       # true rendition Δrungs
    classes = collections.Counter()     # antecedent census
    n = 0
    for pid in play_ids:
        net = pull("network", pid)
        if not net:
            continue
        n += 1
        toks = tk.tokenize(net)
        for i, t in enumerate(toks):
            if not t.startswith("FAULT("):
                continue
            classes[t[t.index("(") + 1:t.rindex(")")]] += 1
            if t.startswith("FAULT(video_seg"):
                for j in range(i + 1, len(toks)):
                    if toks[j].split("(")[0] in SEGMENT_KINDS:
                        dp = toks[j][toks[j].index("(") + 1:].split(",")[0]
                        committed["downshift" if dp.startswith("-") else ("upshift" if dp != "+0" and dp.startswith("+") else "same")] += 1
                        break
        srows = seg_rows(net)
        for k, (_, _, _, st) in enumerate(srows):
            if st != "404":
                continue
            faulted = ORD[srows[k][1]]
            for _, rend2, _, st2 in srows[k + 1:]:
                if st2 != "404":
                    stair[rungs(ORD[rend2] - faulted)] += 1
                    break
    return {"plays": n, "classes": classes, "committed": committed, "staircase": stair}


# ---------- condition: STALL recovery ----------
def analyze_stalls(play_ids, window=8.0, lookback=4.0):
    pl_delta = collections.Counter()
    seg_dir = collections.Counter()
    seg_delta = collections.Counter()
    trigger = collections.Counter()
    nstall = nplays = 0
    for pid in play_ids:
        ev = pull("events", pid)
        starts = sorted(t for t in (parse_ts(r.get("ts")) for r in ev
                        if (r.get("player_metrics") or {}).get("last_event") == "stall_start") if t)
        if not starts:
            continue
        nplays += 1
        net = pull("network", pid)
        srows = seg_rows(net)
        plrows = sorted((parse_ts(r.get("ts")), PL.search(r.get("url") or "").group(1))
                        for r in net if PL.search(r.get("url") or "") and parse_ts(r.get("ts"))
                        and PL.search(r.get("url")).group(1) in ORD)
        faults = [(t, r.get("fault_category") or st) for r, t, st in
                  ((r, parse_ts(r.get("ts")), str(r.get("status"))) for r in net)
                  if t and (SEG.search(r.get("url") or "")) and (st == "404" or st.startswith("5") or r.get("fault_type"))]
        for st in starts:
            nstall += 1
            pre = [s for s in srows if s[0] < st and s[3] == "200"]
            pr, ps = (pre[-1][1], pre[-1][2]) if pre else (None, None)
            hi = st + timedelta(seconds=window)
            fpl = next((p for p in plrows if st <= p[0] <= hi), None)
            if fpl and pr:
                pl_delta[rungs(ORD[fpl[1]] - ORD[pr])] += 1
            fseg = next((s for s in srows if st <= s[0] <= hi), None)
            if fseg:
                if ps is not None:
                    seg_dir["backward (re-fetch)" if fseg[2] <= ps else "forward"] += 1
                if pr:
                    seg_delta[rungs(ORD[fseg[1]] - ORD[pr])] += 1
            lo = st - timedelta(seconds=lookback)
            ftr = [f for f in faults if lo <= f[0] <= st]
            trigger[f"fault-triggered ({ftr[-1][1]})" if ftr else "no fault in window"] += 1
    return {"plays": nplays, "stalls": nstall, "pl_delta": pl_delta,
            "seg_dir": seg_dir, "seg_delta": seg_delta, "trigger": trigger}


# ---------- condition: PLAY-END lead-up ----------
def analyze_ends(plays, lead=20, gap=300, per_bucket=80):
    now = datetime.now(timezone.utc).replace(tzinfo=None)
    # Bucket ALL plays from their records first (free, exact counts); then pull network
    # for a per-bucket sample for the lead-up grammar. Avoids the earlier bug where a
    # global cap on first-N plays undersampled the `silent` bucket.
    by_bucket = collections.defaultdict(list)
    censored = 0
    for p in plays:
        status = (p.get("playback_status") or "").strip()
        last = parse_ts(p.get("last_seen_at"))
        age = (now - last).total_seconds() if last else 1e9
        if status in ("", "in_progress"):
            if age < gap:
                censored += 1
                continue
            b = "silent (no beacon)"
        else:
            reason = (p.get("playback_reason") or "").strip()
            b = status if reason in ("", "unknown") else f"{status}/{reason}"
        by_bucket[b].append(p)
    out = {}
    for b, ps in by_bucket.items():
        tails = []
        for p in ps[:per_bucket]:
            net = pull("network", p["play_id"])
            if not net:
                continue
            body = [t for t in tk.tokenize(net) if t not in ("<S>", "<E>")]
            tails.append(body[-lead:])
        ns = len(tails) or 1
        has = lambda pred: sum(1 for t in tails if any(pred(x) for x in t)) / ns
        out[b] = {"n": len(ps), "sampled": len(tails),
                  "fault": has(lambda x: x.startswith("FAULT(")),
                  "downshift": has(lambda x: x.startswith("V_SEG(-")),
                  "stall": has(lambda x: x.startswith("STALL"))}
    return {"censored": censored, "buckets": out}


# Catalog: adding startup / rate-shift later = an entry here, not a new file.
CONDITIONS = ["faults", "stalls", "play-end"]


def query_plays(days):
    now = datetime.now(timezone.utc)
    frm = (now - timedelta(days=days)).strftime("%Y-%m-%dT%H:%M:%SZ")
    to = (now + timedelta(days=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
    r = subprocess.run(["harness", "--insecure", "--json", "query", "plays",
                        "--from", frm, "--to", to, "--limit", "5000"], capture_output=True, text=True)
    if "{" not in r.stdout:
        sys.exit(f"query plays failed: {r.stdout.strip() or r.stderr.strip()}")
    return json.loads(r.stdout[r.stdout.index("{"):]).get("items", []), frm


def build_report(days=7, engine="AVPlayer", max_plays=200, out="/tmp/vomm_report.md"):
    """The single entry point: run every condition over the window, write one report."""
    plays, frm = query_plays(days)
    eng = [p for p in plays if p.get("player_tech") == engine]
    fault = sorted([p for p in eng if gi(p, "net_faults") > 0 or any(
        x.startswith(("error=", "warning=*fault", "warning=fault", "warning=http")) for x in play_labels(p))],
        key=lambda p: -gi(p, "net_faults"))
    stall = [p for p in eng if gi(p, "segment_stall_count") > 0 or gi(p, "stalls") > 0]

    fr = analyze_faults([p["play_id"] for p in fault[:max_plays]])
    st = analyze_stalls([p["play_id"] for p in stall[:max_plays]])
    en = analyze_ends(eng)  # buckets all plays (exact counts); samples network per bucket

    L = []
    L.append(f"# #508 condition report — {engine}, last {days}d")
    L.append(f"_window since {frm} · plays={len(eng)} · fault-corpus={len(fault)} · stall-corpus={len(stall)} · read-only_\n")

    L.append(f"## Fault recovery  (plays={fr['plays']})")
    L.append("Antecedent census:")
    L += bars(fr["classes"])[0]
    L.append("\nCommitted-segment reaction to `FAULT(video_seg,*)` (through the playlist refetch):")
    L += bars(fr["committed"])[0]
    L.append("\nRendition staircase after a video 404 (true Δrungs):")
    L += bars(fr["staircase"])[0]

    L.append(f"\n## Stall recovery  (plays={st['plays']}, stalls={st['stalls']})")
    L.append("Stall trigger (fault in the 4s before?):")
    L += bars(st["trigger"])[0]
    L.append("\nPlaylist rendition shift after stall:")
    L += bars(st["pl_delta"])[0]
    L.append("\nFirst segment after stall — direction:")
    L += bars(st["seg_dir"])[0]
    L.append("\nFirst segment after stall — rendition Δ:")
    L += bars(st["seg_delta"])[0]

    L.append(f"\n## Play-end lead-up  (live/censored excluded: {en['censored']})")
    L.append("| end-type | n | lead-up sampled | FAULT | downshift | STALL |")
    L.append("|---|---|---|---|---|---|")
    for b, d in sorted(en["buckets"].items(), key=lambda kv: -kv[1]["n"]):
        if d["n"] >= 5:
            L.append(f"| {b} | {d['n']} | {d['sampled']} | {d['fault']:.0%} | {d['downshift']:.0%} | {d['stall']:.0%} |")

    L.append("\n_Caveats: single engine, test-rig-heavy corpus; descriptive (no trained surprise model yet); "
             "`STALL` in play-end lead-up is 0 until end-analysis pulls events; end-type labels contaminated (see #565)._")

    with open(out, "w") as f:
        f.write("\n".join(L) + "\n")
    return out


# Report kinds: name -> builder(days, engine, max_plays, out) -> path. New report types
# (throughput, qoe, vomm scorer output, …) are entries here, not new files.
KINDS = {"conditions": build_report}


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--kind", default="conditions", choices=sorted(KINDS), help="report kind (see KINDS)")
    ap.add_argument("--days", type=int, default=7)
    ap.add_argument("--engine", default="AVPlayer")
    ap.add_argument("--max-plays", type=int, default=200)
    ap.add_argument("--out", default=None, help="default /tmp/report-<kind>.md")
    args = ap.parse_args()
    out = args.out or f"/tmp/report-{args.kind}.md"
    path = KINDS[args.kind](args.days, args.engine, args.max_plays, out)
    print(f"report written: {path}")
    print(open(path).read())


if __name__ == "__main__":
    main()
