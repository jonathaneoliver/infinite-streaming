#!/usr/bin/env python3
"""#506 batch anomaly-LABEL writer (PER ROW) — the sibling of derive_tokens.py.

Trains per-condition VOMMs (PPM-C back-off, from scorer.py) on OLDER plays and scores
RECENT plays OUT-OF-SAMPLE (time split: train on plays predating the scoring window, score
plays inside it — no play is scored by a model trained on itself, so production needs no
k-fold). For each transition WITHIN a condition episode whose surprise (−log P) exceeds the
clean-calibrated bar, writes ONE row to `derived_labels`, anchored on the EXACT row whose
token was improbable. The read API merges these onto that row (→ a chip in NetworkLog /
PlayLog, as many per session as there are surprising rows) AND rolls them up by play_id (→ a
filterable play chip in sessions.html).

Condition-anchored + out-of-sample keeps it off the trivial "every FAULT token is rare"
counter: a typical fault-recovery transition (seen in other plays' fault episodes) is NOT
surprising under the fault-condition model — only atypical reactions clear the bar.

Reads ClickHouse directly (reuses derive_tokens.py's ch()/fetch_rows()); reuses scorer.py's
model + tokenize.py's cross-stream tokeniser/episodes. No hand-built reaction taxonomy: the
tag is the condition (unexpected_<condition>) + a severity from the surprise magnitude.

  python3 analytics/tools/derive_labels.py --train-days 7 --score-hours 12 [--dry-run]
"""
import argparse
import collections
import json
import os
import sys
from datetime import datetime, timedelta, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import tokenize as tk          # noqa: E402  cross-stream tokeniser (_token_seq / episodes)
import scorer as sc            # noqa: E402  VOMM model (train / transition_surprises / p99)
import derive_tokens as dt     # noqa: E402  ch() / fetch_rows() / group_by_play() / DB

NET_COLS = "ts, player_id, play_id, entry_fingerprint, url, status, fault_type, fault_category"
EVENT_COLS = "ts, player_id, play_id, last_event"
SENTINELS = ("<S>", "<E>")


def play_seq_full(net_rows, event_rows):
    """[(ts, fp, surface, token)] cross-stream + <S>/<E> sentinels (fp=None). The ts/fp/surface
    on each entry let us anchor a surprising transition back onto the row that emitted it."""
    seq = tk._token_seq(net_rows, event_rows)  # network tokens carry fp; event tokens fp=None
    if not seq:
        return []
    return [(seq[0][0], None, "", "<S>")] + list(seq) + [(seq[-1][0], None, "", "<E>")]


def episode_windows_full(full, anchor, lead, horizon):
    """[(window_full)] — tk.episodes() windows mapped back to the (ts,fp,surface,token) tuples."""
    toks = [x[3] for x in full]
    out = []
    for e in tk.episodes(toks, anchor=anchor, lead=lead, horizon=horizon):
        i = e["anchor_index"]
        out.append(full[max(0, i - lead):min(len(full), i + horizon + 1)])
    return out


def severity_for(score, thr):
    return "critical" if score >= thr * 1.5 else "warning"


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--ch-url", default=os.environ.get("FORWARDER_CLICKHOUSE_URL", "http://localhost:8123"))
    ap.add_argument("--train-days", type=int, default=7)
    ap.add_argument("--score-hours", type=int, default=12)
    ap.add_argument("--player", default=None, help="limit to one player_id")
    ap.add_argument("--max-order", type=int, default=4)
    ap.add_argument("--alpha", type=float, default=0.5)
    ap.add_argument("--min-train-episodes", type=int, default=20)
    ap.add_argument("--model-version", default="vomm-tok-1")
    ap.add_argument("--dry-run", action="store_true", help="compute + summarise, do NOT insert")
    args = ap.parse_args()

    net_rows = dt.fetch_rows(args.ch_url, args.train_days, 0, args.player, "network_requests",
                             NET_COLS, "player_id, ts, entry_fingerprint")
    event_rows = dt.fetch_rows(args.ch_url, args.train_days, 0, args.player, "session_events",
                               EVENT_COLS, "player_id, ts")
    net_by_play, ev_by_play = dt.group_by_play(net_rows), dt.group_by_play(event_rows)
    plays = list(net_by_play.keys()) + [p for p in ev_by_play if p not in net_by_play]

    cutoff = (datetime.now(timezone.utc) - timedelta(hours=args.score_hours)).strftime("%Y-%m-%d %H:%M:%S.%f")[:-3]
    builds = {}
    for play in plays:
        if not play:
            continue
        prows, erows = net_by_play.get(play, []), ev_by_play.get(play, [])
        full = play_seq_full(prows, erows)
        if not full:
            continue
        pid = next((r.get("player_id") for r in (prows + erows) if r.get("player_id")), "")
        builds[play] = {"pid": pid, "full": full, "max_ts": full[-1][0]}

    train_plays = [b for b in builds.values() if b["max_ts"] < cutoff]
    score_plays = [(p, b) for p, b in builds.items() if b["max_ts"] >= cutoff]
    scored_at = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M:%S.%f")[:-3]

    print(f"#506 derive_labels (per-row) — {len(builds)} plays "
          f"({len(train_plays)} train / {len(score_plays)} score) "
          f"train={args.train_days}d score={args.score_hours}h")

    rows = []
    per_condition = collections.Counter()
    for c in sc.EPISODE_CONDITIONS:
        cond, anchor = c["name"], c["anchor"]
        train_eps = [[x[3] for x in w] for b in train_plays
                     for w in episode_windows_full(b["full"], anchor, c["lead"], c["horizon"])]
        if len(train_eps) < args.min_train_episodes:
            print(f"  {cond:8} — {len(train_eps)} train episodes < {args.min_train_episodes}; skipped")
            continue
        model = sc.train(train_eps, max_order=args.max_order, alpha=args.alpha)
        thr = sc.p99([s for w in train_eps for s, _, _ in sc.transition_surprises(model, w)])
        n = 0
        for play, b in score_plays:
            for w in episode_windows_full(b["full"], anchor, c["lead"], c["horizon"]):
                toks = [x[3] for x in w]
                # transition_surprises[j] is the surprise of token at window position j+1.
                for j, (surprise, _prev, _cur) in enumerate(sc.transition_surprises(model, toks)):
                    if surprise < thr:
                        continue
                    ts, fp, surface, token = w[j + 1]
                    if token in SENTINELS:
                        continue
                    efp, surf = (0, "event") if fp is None else (fp, surface or "net")
                    rows.append({"ts": ts, "player_id": b["pid"], "play_id": play,
                                 "entry_fingerprint": efp, "surface": surf, "condition": cond,
                                 "label": f"unexpected_{cond}",
                                 "severity": severity_for(surprise, thr), "score": round(surprise, 3),
                                 "model_version": args.model_version, "scored_at": scored_at})
                    n += 1
        per_condition[cond] = n
        print(f"  {cond:8} — train_eps={len(train_eps)} thr={thr:.1f} → {n} row labels")

    # Dedup identical (row, condition) within this run (overlapping episodes can flag a row
    # twice); keep the most surprising. Cross-run dedup is the ReplacingMergeTree's job.
    best = {}
    for r in rows:
        k = (r["player_id"], r["ts"], r["entry_fingerprint"], r["condition"])
        if k not in best or r["score"] > best[k]["score"]:
            best[k] = r
    rows = list(best.values())

    print(f"emitted {len(rows)} row labels: {dict(per_condition)}")
    if args.dry_run:
        for r in sorted(rows, key=lambda x: -x["score"])[:10]:
            print(f"   {r['severity']:8} {r['label']:24} score={r['score']:5.1f} "
                  f"surf={r['surface']:5} play={r['play_id'][:8]} @ {r['ts']}")
        return
    if not rows:
        print("nothing to write.")
        return
    body = ("\n".join(json.dumps(r) for r in rows) + "\n").encode()
    dt.ch(args.ch_url, f"INSERT INTO {dt.DB}.derived_labels FORMAT JSONEachRow", body=body)
    print(f"inserted {len(rows)} rows into {dt.DB}.derived_labels.")


if __name__ == "__main__":
    main()
