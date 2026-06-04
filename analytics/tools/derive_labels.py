#!/usr/bin/env python3
"""#506/#608 anomaly-LABEL deriver (PER ROW) — now split into TRAIN and SCORE modes.

#506 introduced per-row VOMM "surprise" labels (unexpected_<condition>). #608 decouples the
two operations that used to run together every tick:

  --mode train  (SLOW, nightly): build per-condition VOMMs (PPM-C back-off, from scorer.py)
                over the last --train-days of plays, calibrate each condition's p99 surprise
                threshold, and serialize the whole thing to a gzipped-JSON ARTIFACT at
                --model-path (a volume shared with the scorer). Also writes a small manifest
                row per condition to derived_models for observability. This is the only place
                the expensive sc.train runs.

  --mode score (FAST, ~1 min): load the latest artifact, score recent plays' transitions
                against it, and write one derived_labels row per above-threshold transition,
                anchored on the EXACT row whose token was improbable. NO training.

Why the split keeps the out-of-sample guarantee (in fact strengthens it): the artifact stamps
`trained_at` (= the moment the corpus closed). The scorer only scores plays whose max_ts
POST-DATES trained_at — those plays did not exist when the model trained, so they are
out-of-sample by construction. `trained_at` IS the cutoff; no `now − score_hours` arithmetic.

The model that changes slowly (the learned grammar of typical episodes) is rebuilt slowly;
scoring, which we want fresh, is cheap (dict lookups) and runs often. No hand-built reaction
taxonomy: the tag is the condition (unexpected_<condition>) + a severity from the surprise
magnitude. Reads ClickHouse directly (reuses derive_tokens.py's ch()/fetch_rows()); reuses
scorer.py's model + tokenize.py's cross-stream tokeniser/episodes.

  python3 analytics/tools/derive_labels.py --mode train --train-days 7 --model-path /models/labels-model.json.gz
  python3 analytics/tools/derive_labels.py --mode score --score-hours 2 --model-path /models/labels-model.json.gz [--dry-run]
"""
import argparse
import collections
import gzip
import json
import os
import sys
import tempfile
from datetime import datetime, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import tokenize as tk          # noqa: E402  cross-stream tokeniser (_token_seq / episodes)
import scorer as sc            # noqa: E402  VOMM model (train / transition_surprises / p99)
import derive_tokens as dt     # noqa: E402  ch() / fetch_rows() / group_by_play() / DB

NET_COLS = "ts, player_id, play_id, entry_fingerprint, url, status, fault_type, fault_category"
EVENT_COLS = "ts, player_id, play_id, last_event"
SENTINELS = ("<S>", "<E>")
DEFAULT_MODEL_PATH = os.environ.get("LABEL_MODEL_PATH", "/models/labels-model.json.gz")


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


def build_plays(ch_url, days, hours, player):
    """{play_id: {pid, full, max_ts}} — cross-stream full token sequences over a CH window."""
    net_rows = dt.fetch_rows(ch_url, days, hours, player, "network_requests",
                             NET_COLS, "player_id, ts, entry_fingerprint")
    event_rows = dt.fetch_rows(ch_url, days, hours, player, "session_events",
                               EVENT_COLS, "player_id, ts")
    net_by_play, ev_by_play = dt.group_by_play(net_rows), dt.group_by_play(event_rows)
    plays = list(net_by_play.keys()) + [p for p in ev_by_play if p not in net_by_play]
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
    return builds


# ---------- model artifact (gzipped JSON; counts have tuple keys → list form) ----------
def serialize_model(model):
    """sc model {counts:[{ctx_tuple: {tok:cnt}}], max_order, V} → JSON-safe nested lists."""
    return {
        "max_order": model["max_order"],
        "V": model["V"],
        "counts": [[[list(ctx), list(ctr.items())] for ctx, ctr in level.items()]
                   for level in model["counts"]],
    }


def deserialize_model(d):
    """Inverse of serialize_model. Plain dicts are enough for scorer._ppm_prob's reads."""
    counts = []
    for level in d["counts"]:
        counts.append({tuple(ctx): dict(items) for ctx, items in level})
    return {"counts": counts, "max_order": d["max_order"], "V": d["V"]}


def n_contexts(model):
    return sum(len(level) for level in model["counts"])


def save_artifact(path, artifact):
    os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path) or ".", suffix=".tmp")
    os.close(fd)
    with gzip.open(tmp, "wt", encoding="utf-8") as f:
        json.dump(artifact, f, separators=(",", ":"))
    os.replace(tmp, path)          # atomic swap so the scorer never reads a half-written file
    return os.path.getsize(path)


def load_artifact(path):
    if not os.path.exists(path):
        return None
    with gzip.open(path, "rt", encoding="utf-8") as f:
        return json.load(f)


def utc_now():
    return datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M:%S.%f")[:-3]


# ---------- TRAIN ----------
def cmd_train(args):
    builds = build_plays(args.ch_url, args.train_days, 0, args.player)
    trained_at = utc_now()
    print(f"#608 train — {len(builds)} plays over {args.train_days}d, K={args.max_order} "
          f"→ {args.model_path}")

    conditions, manifest = {}, []
    for c in sc.EPISODE_CONDITIONS:
        cond, anchor = c["name"], c["anchor"]
        train_eps = [[x[3] for x in w] for b in builds.values()
                     for w in episode_windows_full(b["full"], anchor, c["lead"], c["horizon"])]
        if len(train_eps) < args.min_train_episodes:
            print(f"  {cond:8} — {len(train_eps)} train episodes < {args.min_train_episodes}; skipped")
            continue
        model = sc.train(train_eps, max_order=args.max_order, alpha=args.alpha)
        thr = sc.p99([s for w in train_eps for s, _, _ in sc.transition_surprises(model, w)])
        conditions[cond] = {"anchor": anchor, "lead": c["lead"], "horizon": c["horizon"],
                            "thr": thr, "n_train_episodes": len(train_eps),
                            "model": serialize_model(model)}
        manifest.append({"trained_at": trained_at, "model_version": args.model_version,
                         "condition": cond, "train_window_days": args.train_days,
                         "n_train_episodes": len(train_eps), "threshold": round(thr, 4),
                         "n_contexts": n_contexts(model), "vocab": model["V"],
                         "max_order": args.max_order, "artifact_path": args.model_path})
        print(f"  {cond:8} — train_eps={len(train_eps)} thr={thr:.1f} "
              f"contexts={n_contexts(model)} vocab={model['V']}")

    if not conditions:
        print("no condition had enough episodes to train; artifact NOT written.")
        return
    artifact = {"model_version": args.model_version, "trained_at": trained_at,
                "train_window_days": args.train_days, "max_order": args.max_order,
                "alpha": args.alpha, "conditions": conditions}
    if args.dry_run:
        print(f"[dry-run] would write artifact for {list(conditions)} (trained_at={trained_at})")
        return
    size = save_artifact(args.model_path, artifact)
    for m in manifest:
        m["artifact_bytes"] = size
    body = ("\n".join(json.dumps(m) for m in manifest) + "\n").encode()
    dt.ch(args.ch_url, f"INSERT INTO {dt.DB}.derived_models FORMAT JSONEachRow", body=body)
    print(f"wrote {size} B artifact ({list(conditions)}) + {len(manifest)} manifest rows.")


# ---------- SCORE ----------
def cmd_score(args):
    art = load_artifact(args.model_path)
    if not art:
        print(f"#608 score — no model artifact at {args.model_path} yet "
              f"(trainer hasn't run?); nothing to score.")
        return
    trained_at = art["trained_at"]
    builds = build_plays(args.ch_url, 0, args.score_hours, args.player)
    # Out-of-sample by construction: only plays that finished AFTER the model's training
    # corpus closed. trained_at IS the cutoff (lexicographic == chronological on UTC strings).
    score_plays = [(p, b) for p, b in builds.items() if b["max_ts"] > trained_at]
    scored_at = utc_now()
    print(f"#608 score — model {art['model_version']} trained_at={trained_at}; "
          f"{len(builds)} plays in last {args.score_hours}h, {len(score_plays)} out-of-sample")

    rows = []
    per_condition = collections.Counter()
    for cond, cdata in art["conditions"].items():
        model = deserialize_model(cdata["model"])
        thr, anchor = cdata["thr"], cdata["anchor"]
        lead, horizon = cdata["lead"], cdata["horizon"]
        n = 0
        for play, b in score_plays:
            for w in episode_windows_full(b["full"], anchor, lead, horizon):
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
                                 "model_version": art["model_version"], "scored_at": scored_at})
                    n += 1
        per_condition[cond] = n
        print(f"  {cond:8} — thr={thr:.1f} → {n} row labels")

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


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--mode", choices=["train", "score"], required=True)
    ap.add_argument("--ch-url", default=os.environ.get("FORWARDER_CLICKHOUSE_URL", "http://localhost:8123"))
    ap.add_argument("--model-path", default=DEFAULT_MODEL_PATH, help="gzipped-JSON artifact shared by train+score")
    ap.add_argument("--train-days", type=int, default=7, help="train: corpus window")
    ap.add_argument("--score-hours", type=int, default=2, help="score: how far back to look for out-of-sample plays")
    ap.add_argument("--player", default=None, help="limit to one player_id")
    ap.add_argument("--max-order", type=int, default=4)
    ap.add_argument("--alpha", type=float, default=0.5)
    ap.add_argument("--min-train-episodes", type=int, default=20)
    ap.add_argument("--model-version", default="vomm-tok-1")
    ap.add_argument("--dry-run", action="store_true", help="compute + summarise, do NOT write")
    args = ap.parse_args()
    (cmd_train if args.mode == "train" else cmd_score)(args)


if __name__ == "__main__":
    main()
