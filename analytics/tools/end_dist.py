#!/usr/bin/env python3
"""Play-end lead-up analysis for #508 — the outcome layer.

Anchors on play_id activity CESSATION, NOT the session_end event (the silent majority
emit none): a play has ended if it carries a terminal playback_status OR its last_seen is
stale beyond a heartbeat gap. Each ended play is bucketed by end-type
(playback_status[/reason], or `silent` when in_progress+stale); still-live plays
(recent last_seen) are right-censored and excluded. For each bucket we report the LEAD-UP
grammar — the tail of the token stream before the end — to ask "what behaviour preceded
this kind of ending?".

session_end's terminal status/reason is used as the label WHERE PRESENT (more info is
good); `silent` is its own bucket (cause unknown — crash / network / give-up). See
CORPUS_PLAN.md "Condition-anchored analysis".

READ-ONLY (#508): reads via harness; writes nothing.

Usage:
  end_dist.py --plays-json plays_7d.json [--lead 20] [--gap-seconds 300]
              [--limit 5000] [--max-plays 0] [--min-count 5]
"""
import argparse
import collections
import json
import os
import sys
from datetime import datetime, timezone

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import tokenize as tk          # noqa: E402
import recovery_dist as rd     # noqa: E402  (reuse pull_network + next_label)


def parse_ts(s):
    s = (s or "").replace("T", " ").replace("Z", "").strip()
    for fmt in ("%Y-%m-%d %H:%M:%S.%f", "%Y-%m-%d %H:%M:%S"):
        try:
            return datetime.strptime(s[:26], fmt)
        except ValueError:
            pass
    return None


def end_bucket(p, now, gap_s):
    """End-type label, or None if the play is still live (right-censored)."""
    status = (p.get("playback_status") or "").strip()
    last = parse_ts(p.get("last_seen_at"))
    age = (now - last).total_seconds() if last else 1e9
    if status in ("", "in_progress"):
        return None if age < gap_s else "silent (no beacon)"
    reason = (p.get("playback_reason") or "").strip()
    return status if reason in ("", "unknown") else f"{status}/{reason}"


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--plays-json", required=True, help="a `query plays --json` dump (carries playback_status/last_seen_at)")
    ap.add_argument("--lead", type=int, default=20, help="tail tokens before the end to inspect")
    ap.add_argument("--gap-seconds", type=int, default=300, help="last_seen staleness gap = ended (residency reaper ~5min)")
    ap.add_argument("--limit", type=int, default=5000, help="per-play network row cap")
    ap.add_argument("--max-plays", type=int, default=0, help="cap network pulls (0 = all)")
    ap.add_argument("--min-count", type=int, default=5, help="min plays per bucket to report")
    args = ap.parse_args()

    plays = json.load(open(args.plays_json))
    plays = plays.get("items", plays)
    now = datetime.now(timezone.utc).replace(tzinfo=None)  # naive UTC to match parsed last_seen

    buckets = collections.defaultdict(list)  # bucket -> list of tail token-lists
    censored = pulled = 0
    for p in plays:
        b = end_bucket(p, now, args.gap_seconds)
        if b is None:
            censored += 1
            continue
        if args.max_plays and pulled >= args.max_plays:
            break
        rows = rd.pull_network(p["play_id"], args.limit)
        pulled += 1
        if not rows:
            continue
        body = [t for t in tk.tokenize(rows) if t not in ("<S>", "<E>")]
        buckets[b].append(body[-args.lead:])

    print(f"plays: {len(plays)}  ended(analysed): {sum(len(v) for v in buckets.values())}  "
          f"live(censored): {censored}  network-pulls: {pulled}\n")

    for b, tails in sorted(buckets.items(), key=lambda kv: -len(kv[1])):
        n = len(tails)
        if n < args.min_count:
            continue
        has = lambda pred: sum(1 for t in tails if any(pred(x) for x in t)) / n
        fault = has(lambda x: x.startswith("FAULT("))
        probe = has(lambda x: x.startswith("V_PROBE"))
        down = has(lambda x: x.startswith("V_SEG(-"))
        stall = has(lambda x: x.startswith("STALL"))  # present once cross-stream lands
        last_tok = collections.Counter(rd.next_label(t[-1]) for t in tails if t)
        print(f"== end-type: {b}   n={n} ==")
        print(f"   lead-up (last {args.lead} tokens) contains:  "
              f"FAULT {fault:.0%} | V_PROBE {probe:.0%} | downshift {down:.0%} | STALL {stall:.0%}")
        print(f"   last token before end: {dict(last_tok.most_common(6))}\n")


if __name__ == "__main__":
    main()
