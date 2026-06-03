#!/usr/bin/env python3
"""#506 batch anomaly-LABEL writer — the sibling of derive_tokens.py.

Trains per-condition VOMMs (PPM-C back-off, from scorer.py) on OLDER plays and scores
RECENT plays OUT-OF-SAMPLE (a time split: train on plays whose data predates the scoring
window, score plays inside it — no play is scored by a model trained on itself, which is
why production needs no k-fold). For each (play, condition) whose worst episode is notably
more surprising than the typical episode for that condition, writes one row to the
`derived_labels` table. The read API unions these into labels[] → filterable in
sessions.html, markable in session-viewer.

Reads ClickHouse directly over HTTP (reuses derive_tokens.py's ch()/fetch_rows()), reusing
scorer.py's model + tokenize.py's cross-stream tokeniser/episode windows — the SINGLE
sources of truth. No hand-built reaction taxonomy: the tag is the condition
(vomm_<condition>_surprise) + a severity from the surprise rate; the model's peak
transition rides in the `peak` column as detail.

  python3 analytics/tools/derive_labels.py --train-days 7 --score-hours 6 [--dry-run]
  # CH endpoint via --ch-url or FORWARDER_CLICKHOUSE_URL (default http://localhost:8123)
"""
import argparse
import collections
import os
import sys
from datetime import datetime, timedelta, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import tokenize as tk          # noqa: E402  cross-stream tokeniser + episode windows
import scorer as sc            # noqa: E402  VOMM model (train / transition_surprises / score / p99)
import derive_tokens as dt     # noqa: E402  ch() / fetch_rows() / group_by_play() / DB

NET_COLS = "ts, player_id, play_id, entry_fingerprint, url, status, fault_type, fault_category"
EVENT_COLS = "ts, player_id, play_id, last_event"


def play_tokens_with_ts(net_rows, event_rows):
    """(tokens, ts_parallel) for one play — cross-stream, ts-sorted, with <S>/<E> sentinels
    whose ts borrow the first/last real token (so an <E>-anchored episode marks the tail)."""
    seq = tk._token_seq(net_rows, event_rows)  # [(ts, fp, surface, token)] ts-sorted
    if not seq:
        return [], []
    toks = [s[3] for s in seq]
    tss = [s[0] for s in seq]
    return ["<S>"] + toks + ["<E>"], [tss[0]] + tss + [tss[-1]]


def ts_episodes(toks, tss, anchor, lead, horizon):
    """[(anchor_ts, window_tokens)] — tk.episodes() windows + the anchor token's ts."""
    return [(tss[e["anchor_index"]], e["window"])
            for e in tk.episodes(toks, anchor=anchor, lead=lead, horizon=horizon)]


def severity_for(rate, crit):
    return "critical" if rate >= crit else "warning"


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--ch-url", default=os.environ.get("FORWARDER_CLICKHOUSE_URL", "http://localhost:8123"))
    ap.add_argument("--train-days", type=int, default=7, help="corpus window for training the per-condition models")
    ap.add_argument("--score-hours", type=int, default=6, help="recent window whose plays get scored (out-of-sample vs train)")
    ap.add_argument("--player", default=None, help="limit to one player_id")
    ap.add_argument("--max-order", type=int, default=4)
    ap.add_argument("--alpha", type=float, default=0.5)
    ap.add_argument("--min-train-episodes", type=int, default=20, help="skip a condition with fewer training episodes")
    ap.add_argument("--emit-rate", type=float, default=0.34, help="episode surprise-rate at/above which a label is emitted")
    ap.add_argument("--crit-rate", type=float, default=0.60, help="surprise-rate at/above which severity=critical")
    ap.add_argument("--model-version", default="vomm-tok-1")
    ap.add_argument("--dry-run", action="store_true", help="compute + summarise, do NOT insert")
    args = ap.parse_args()

    # Fetch the whole train window (which contains the recent score window); split by ts.
    net_rows = dt.fetch_rows(args.ch_url, args.train_days, 0, args.player, "network_requests",
                             NET_COLS, "player_id, ts, entry_fingerprint")
    event_rows = dt.fetch_rows(args.ch_url, args.train_days, 0, args.player, "session_events",
                               EVENT_COLS, "player_id, ts")
    net_by_play = dt.group_by_play(net_rows)
    ev_by_play = dt.group_by_play(event_rows)
    plays = list(net_by_play.keys()) + [p for p in ev_by_play if p not in net_by_play]

    # Build per-play token seqs + recency; ClickHouse ts strings sort lexically.
    cutoff = (datetime.now(timezone.utc) - timedelta(hours=args.score_hours)).strftime("%Y-%m-%d %H:%M:%S.%f")[:-3]
    builds = {}  # play_id -> {pid, toks, tss, start_ts, max_ts}
    for play in plays:
        if not play:
            continue
        prows, erows = net_by_play.get(play, []), ev_by_play.get(play, [])
        toks, tss = play_tokens_with_ts(prows, erows)
        if not toks:
            continue
        pid = next((r.get("player_id") for r in (prows + erows) if r.get("player_id")), "")
        builds[play] = {"pid": pid, "toks": toks, "tss": tss, "start_ts": tss[0], "max_ts": tss[-1]}

    train_plays = [b for b in builds.values() if b["max_ts"] < cutoff]
    score_plays = [(p, b) for p, b in builds.items() if b["max_ts"] >= cutoff]
    scored_at = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M:%S.%f")[:-3]

    print(f"#506 derive_labels — {len(builds)} plays ({len(train_plays)} train / {len(score_plays)} score) "
          f"train={args.train_days}d score={args.score_hours}h emit>={args.emit_rate:.0%}")

    rows = []
    per_condition = collections.Counter()
    for c in sc.EPISODE_CONDITIONS:
        cond, anchor = c["name"], c["anchor"]
        train_eps = [w for b in train_plays for _, w in ts_episodes(b["toks"], b["tss"], anchor, c["lead"], c["horizon"])]
        if len(train_eps) < args.min_train_episodes:
            print(f"  {cond:8} — {len(train_eps)} train episodes < {args.min_train_episodes}; skipped")
            continue
        model = sc.train(train_eps, max_order=args.max_order, alpha=args.alpha)
        thr = sc.p99([s for w in train_eps for s, _, _ in sc.transition_surprises(model, w)])
        n_emit = 0
        for play, b in score_plays:
            eps = ts_episodes(b["toks"], b["tss"], anchor, c["lead"], c["horizon"])
            if not eps:
                continue
            worst = None  # (rate, peak, anchor_ts)
            for anchor_ts, w in eps:
                r = sc.score(model, w, thr)
                if worst is None or r["rate"] > worst[0]:
                    worst = (r["rate"], r["argmax"], anchor_ts)
            if not worst or worst[0] < args.emit_rate:
                continue
            rate, peak, peak_at = worst
            rows.append({"ts": b["start_ts"], "player_id": b["pid"], "play_id": play,
                         "condition": cond, "label": f"vomm_{cond}_surprise",
                         "severity": severity_for(rate, args.crit_rate), "score": round(rate, 4),
                         "peak": peak or "", "peak_at": peak_at,
                         "model_version": args.model_version, "scored_at": scored_at})
            n_emit += 1
        per_condition[cond] = n_emit
        print(f"  {cond:8} — train_eps={len(train_eps)} thr={thr:.1f} → {n_emit} plays labelled")

    print(f"emitted {len(rows)} labels: {dict(per_condition)}")
    if args.dry_run:
        for r in sorted(rows, key=lambda x: -x["score"])[:8]:
            print(f"   {r['severity']:8} {r['label']:24} score={r['score']:.0%} play={r['play_id'][:8]} peak: {r['peak']}")
        return
    if not rows:
        print("nothing to write.")
        return
    import json
    body = ("\n".join(json.dumps(r) for r in rows) + "\n").encode()
    dt.ch(args.ch_url, f"INSERT INTO {dt.DB}.derived_labels FORMAT JSONEachRow", body=body)
    print(f"inserted {len(rows)} rows into {dt.DB}.derived_labels.")


if __name__ == "__main__":
    main()
