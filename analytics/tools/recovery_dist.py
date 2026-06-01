#!/usr/bin/env python3
"""Layer-1 fault-conditional reaction distribution for #508.

Descriptive recovery grammar: P(next | FAULT(surface,class)) over a set of plays —
"what does the player do immediately after a given fault?". This is the depth-1 leaf of
the VOMM back-off tree (see CORPUS_PLAN.md "Two modelling layers"); it is NOT a
session-level avg-NLL score (which would re-derive frequency), it's a targeted
conditional on a rare, meaningful antecedent.

READ-ONLY (#508): reads the archive via `harness query network --json`, writes nothing.

Two views per antecedent:
  * raw next token (immediate successor in the interleaved request stream), and
  * next VIDEO-surface token (skips audio interleaving) — more interpretable for the
    video recovery grammar: retry / downshift / playlist-refetch / resume / give-up.

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

VIDEO_KINDS = ("V_SEG", "V_PROBE", "V_PL")
CACHE_DIR = "/tmp"


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
    vid_next = collections.defaultdict(collections.Counter)    # video-fault antecedent -> Counter(next video label)
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
            # next-video view (skip audio/other interleaving) for video-surface faults
            if ant.startswith("video_seg"):
                for j in range(i + 1, len(toks)):
                    if toks[j].split("(")[0] in VIDEO_KINDS or toks[j] == "<E>":
                        vid_next[ant][next_label(toks[j])] += 1
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
    render("P(next VIDEO token | FAULT(video_seg,*)) — recovery grammar", vid_next)


if __name__ == "__main__":
    main()
