#!/usr/bin/env python3
"""Layer-1 fault-conditional reaction distribution for #508.

Descriptive recovery grammar: P(next | FAULT(surface,class)) over a set of plays —
"what does the player do immediately after a given fault?". This is the depth-1 leaf of
the VOMM back-off tree (see CORPUS_PLAN.md "Two modelling layers"); it is NOT a
session-level avg-NLL score (which would re-derive frequency), it's a targeted
conditional on a rare, meaningful antecedent.

READ-ONLY (#508): reads the archive via `harness query network --json`, writes nothing.

Views:
  * raw next token (immediate successor in the interleaved request stream);
  * next committed video SEGMENT (V_SEG/V_PROBE) — follows THROUGH the playlist refetch
    to the rendition the player actually commits to. NOTE: the immediate next token after
    a video 404 is usually V_PL (the player re-resolves via the playlist), so stopping at
    the first "video token" hides the real reaction — you must walk to the next segment.
    (This was a real depth-1 trap: it inverted "downshift every time" into "no downshift".)
  * rendition staircase — TRUE rendition delta (from URLs, not the clamped token ΔP, so
    it distinguishes −1 from −5) between a 404'd video segment and the next committed one.

The agency caveat (CORPUS_PLAN.md): client_abandon is player-INITIATED, so its
"reaction" is really the player's own switch behaviour, not a recovery from an imposed
fault. Reported, but read it differently from the server-imposed classes.

Usage:
  recovery_dist.py --plays-file ios_fault_plays.txt [--limit 5000] [--min-count 5]
  recovery_dist.py --play <uuid>
"""
import argparse
import collections
import json
import os
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import tokenize as tk  # noqa: E402

SEGMENT_KINDS = ("V_SEG", "V_PROBE")  # committed video renditions (NOT V_PL — that's the
#                                      intermediate playlist refetch, not a commit).
CACHE_DIR = "/tmp"


def seg_rendition(url):
    """Rendition string of a video segment URL (e.g. '2160p'), or None."""
    m = tk.SEG_RE.search(url or "")
    return m.group(1) if (m and m.group(1) in tk.RENDITION_ORDINAL) else None


def pull_network(uuid, limit):
    """Pull (and cache) a play's network rows via harness query network --json."""
    path = os.path.join(CACHE_DIR, f"cnet_{uuid[:8]}.json")
    if not os.path.exists(path):
        r = subprocess.run(
            ["harness", "--insecure", "--json", "query", "network", uuid, "--limit", str(limit)],
            capture_output=True, text=True,
        )
        if not r.stdout.lstrip().startswith(("{", "[")):
            return None
        with open(path, "w") as f:
            f.write(r.stdout)
    return tk.load_file(path)


def next_label(tok):
    """Collapse a successor token to an interpretable recovery-action label."""
    head = tok.split("(")[0]
    if head == "FAULT":
        # FAULT(surface,class) -> FAULT:class
        inner = tok[tok.index("(") + 1:tok.rindex(")")]
        cls = inner.split(",")[-1].strip()
        return f"FAULT:{cls}"
    if head == "V_SEG":
        dp = tok[tok.index("(") + 1:].split(",")[0]
        arrow = "↓" if dp.startswith("-") else ("↑" if dp.startswith("+") and dp != "+0" else "=")
        return f"V_SEG{arrow}"
    return head  # V_PROBE, V_PL, A_SEG, A_PL, M_PL, STARTUP_RAMP, LOOP_BOUNDARY, <E>


def fault_antecedent(tok):
    """FAULT(surface,class) -> 'surface,class', else None."""
    if not tok.startswith("FAULT("):
        return None
    return tok[tok.index("(") + 1:tok.rindex(")")]


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    src = ap.add_mutually_exclusive_group(required=True)
    src.add_argument("--plays-file", help="newline-separated play UUIDs")
    src.add_argument("--play", help="single play UUID")
    ap.add_argument("--limit", type=int, default=5000, help="per-play network row cap")
    ap.add_argument("--min-count", type=int, default=5, help="min antecedent occurrences to report")
    args = ap.parse_args()

    uuids = [args.play] if args.play else [l.strip() for l in open(args.plays_file) if l.strip()]

    raw_next = collections.defaultdict(collections.Counter)    # antecedent -> Counter(next_label)
    seg_next = collections.defaultdict(collections.Counter)    # video-fault -> Counter(next COMMITTED-segment label)
    stair = collections.Counter()                              # true rendition delta after a video 404
    n_plays = 0
    for uuid in uuids:
        rows = pull_network(uuid, args.limit)
        if not rows:
            continue
        n_plays += 1
        toks = tk.tokenize(rows)
        for i, tok in enumerate(toks):
            ant = fault_antecedent(tok)
            if ant is None:
                continue
            nxt = toks[i + 1] if i + 1 < len(toks) else "<E>"
            raw_next[ant][next_label(nxt)] += 1
            # committed-segment view: walk THROUGH V_PL/audio to the next V_SEG/V_PROBE.
            if ant.startswith("video_seg"):
                for j in range(i + 1, len(toks)):
                    if toks[j].split("(")[0] in SEGMENT_KINDS or toks[j] == "<E>":
                        seg_next[ant][next_label(toks[j])] += 1
                        break

        # rendition staircase (row-based — true renditions, unclamped). For each 404'd
        # video segment, the ordinal delta to the next committed (non-404) video segment.
        vrows = [(r.get("ts") or r.get("timestamp") or "", r) for r in rows if seg_rendition(r.get("url"))]
        vrows.sort(key=lambda x: x[0])
        for k, (_, r) in enumerate(vrows):
            if str(r.get("status")) != "404":
                continue
            faulted = tk.RENDITION_ORDINAL[seg_rendition(r["url"])]
            for _, r2 in vrows[k + 1:]:
                if str(r2.get("status")) != "404":
                    stair[tk.RENDITION_ORDINAL[seg_rendition(r2["url"])] - faulted] += 1
                    break

    print(f"plays tokenized: {n_plays}/{len(uuids)}\n")

    def render(title, table):
        print(f"================ {title} ================")
        for ant, ctr in sorted(table.items(), key=lambda kv: -sum(kv[1].values())):
            total = sum(ctr.values())
            if total < args.min_count:
                continue
            agency = "  [player-initiated — behaviour, not reaction]" if "client_abandon" in ant else ""
            print(f"\nFAULT({ant})   n={total}{agency}")
            for lbl, c in ctr.most_common(8):
                bar = "█" * int(round(30 * c / total))
                print(f"    {lbl:18} {c/total:5.1%} {bar} ({c})")

    render("P(next token | FAULT) — raw immediate successor", raw_next)
    render("P(next committed video SEGMENT | FAULT(video_seg,*)) — recovery grammar "
           "(through the playlist refetch)", seg_next)

    # Rendition staircase — true unclamped deltas.
    total = sum(stair.values())
    if total:
        print(f"\n================ rendition staircase after a video 404 "
              f"(true Δrungs, n={total}) ================")
        labels = {0: "same rung", -1: "down 1 rung"}
        for d in sorted(stair, key=lambda d: (d != -1, d != 0, d)):
            c = stair[d]
            name = labels.get(d, f"{'down' if d < 0 else 'up'} {abs(d)} rungs")
            bar = "█" * int(round(30 * c / total))
            print(f"    Δ{d:+d}  {name:13} {c/total:5.1%} {bar} ({c})")
        down1 = stair.get(-1, 0)
        print(f"\n    → one-rung downshift dominates: {down1}/{total} ({down1/total:.0%}); "
              f"big drops (≤−4) = probe-to-top-then-crash; +Δ = optimistic re-probe from floor.")


if __name__ == "__main__":
    main()
