#!/usr/bin/env python3
"""#445/#608 HMM latent-regime LABEL deriver (PER ROW) — TRAIN + SCORE split.

Sibling of derive_labels.py (the VOMM deriver). Where the VOMM emits point
`unexpected_<condition>` surprise on the row whose token transition was improbable, the HMM
emits a *regime* family decoded from the latent-state timeline:

  * regime_<STATE>             — one row at the FIRST token of each Viterbi state interval
                                 (STARTUP / STEADY / RECOVERING / STALLED / ENDING …),
                                 severity=info. The session's regime ribbon, on the rows.
  * unexpected_regime_transition — a Viterbi state→state move whose −log P(transition) under
                                 the learned transmat exceeds the train-calibrated p99
                                 threshold. severity from the magnitude. The "this regime
                                 change is abnormal" signal the point-surprise model can't see.

Same #608 contract as the VOMM deriver:

  --mode train (SLOW, nightly): fit a CategoricalHMM over the last --train-days of plays
                (whole cross-stream token sequences — regimes incl. failure modes emerge
                because the corpus is bulk-normal, not hand-split), auto-label its states,
                calibrate the transition-surprise p99 threshold, serialize the fitted model
                to a gzipped-JSON artifact at --model-path, and write a derived_models
                manifest row. The only place hmmlearn's Baum-Welch runs.

  --mode score (FAST): load the artifact, Viterbi-decode plays whose max_ts POST-DATES the
                model's trained_at (OUT-OF-SAMPLE by construction — same guarantee as the
                VOMM deriver), and write derived_labels rows. No training.

Reads ClickHouse directly (reuses derive_tokens.py's ch()/fetch_rows()); reuses tokenize.py's
cross-stream tokeniser and hmm_model.py's model. v1 trains ONE global HMM (the CH-direct read
path doesn't carry player_kind/variant; per-bucket models are a follow-up — see hmm-scorer.md).

  python3 analytics/tools/hmm_deriver.py --mode train --train-days 7 --model-path /models/hmm-model.json.gz
  python3 analytics/tools/hmm_deriver.py --mode score --score-hours 2 --model-path /models/hmm-model.json.gz [--dry-run]
"""
import argparse
import base64
import collections
import importlib.util
import json
import os
import sys
import urllib.parse
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
# Our sibling module is named tokenize.py, which SHADOWS the stdlib `tokenize`. The VOMM
# deriver gets away with `import tokenize` because its stack is pure-stdlib and never
# triggers a stdlib `import tokenize`. The HMM stack pulls in scipy (via hmmlearn), and
# scipy DOES `import tokenize` internally (inspect.signature over numpy builtins) — if our
# tokenize.py is found first, scipy explodes. Running this file as a script puts HERE at
# sys.path[0] automatically, so appending isn't enough: we REMOVE our dir from sys.path and
# load every sibling explicitly by file path. derive_tokens.py can't be imported for the
# same reason (it does insert(0, HERE) + `import tokenize`), so its small CH read helpers
# (ch/fetch_rows/group_by_play) are inlined below.
sys.path[:] = [p for p in sys.path if os.path.abspath(p or os.getcwd()) != HERE]


def _load(name, filename):
    spec = importlib.util.spec_from_file_location(name, os.path.join(HERE, filename))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


hm = _load("hmm_model", "hmm_model.py")     # CategoricalHMM core + artifact I/O
tk = _load("iss_tokenize", "tokenize.py")   # cross-stream tokeniser (_token_seq)

DB = "infinite_streaming"
NET_COLS = "ts, player_id, play_id, entry_fingerprint, url, status, fault_type, fault_category"
EVENT_COLS = "ts, player_id, play_id, last_event"
CONDITION = "regime"                       # single global condition for v1
DEFAULT_MODEL_PATH = os.environ.get("HMM_MODEL_PATH", "/models/hmm-model.json.gz")


# ---------- ClickHouse reads (inlined from derive_tokens.py; see import note above) ----------
def _auth_header():
    u, p = os.environ.get("FORWARDER_CLICKHOUSE_USER"), os.environ.get("FORWARDER_CLICKHOUSE_PASSWORD")
    if u:
        tok = base64.b64encode(f"{u}:{p or ''}".encode()).decode()
        return {"Authorization": f"Basic {tok}"}
    return {}


def ch(ch_url, query, body=None, params=None):
    q = {"query": query}
    if body is None:
        q["default_format"] = "JSONEachRow"
    for k, v in (params or {}).items():
        q[f"param_{k}"] = v
    url = ch_url.rstrip("/") + "/?" + urllib.parse.urlencode(q)
    headers = _auth_header()
    if body is not None:
        headers["Content-Type"] = "application/x-ndjson"
    req = urllib.request.Request(url, data=body, method="POST" if body is not None else "GET", headers=headers)
    with urllib.request.urlopen(req, timeout=120) as resp:
        return resp.read().decode()


def fetch_rows(ch_url, days, hours, player, table, cols, order):
    if hours:
        where, params = ["ts >= now64(3) - INTERVAL {hours:UInt32} HOUR"], {"hours": str(hours)}
    else:
        where, params = ["ts >= now64(3) - INTERVAL {days:UInt32} DAY"], {"days": str(days)}
    if player:
        where.append("player_id = {pid:String}")
        params["pid"] = player
    sql = f"SELECT {cols} FROM {DB}.{table} WHERE {' AND '.join(where)} ORDER BY {order}"
    return [json.loads(l) for l in ch(ch_url, sql, params=params).splitlines() if l.strip()]


def group_by_play(rows):
    by = collections.defaultdict(list)
    for r in rows:
        by[r.get("play_id")].append(r)
    return by


def play_seq_full(net_rows, event_rows):
    """[(ts, fp, surface, token)] cross-stream + <S>/<E> sentinels (fp=None). The per-token
    ts/fp/surface let us anchor a decoded regime back onto the row that emitted it."""
    seq = tk._token_seq(net_rows, event_rows)
    if not seq:
        return []
    return [(seq[0][0], None, "", "<S>")] + list(seq) + [(seq[-1][0], None, "", "<E>")]


def build_plays(ch_url, days, hours, player):
    """{play_id: {pid, full, max_ts}} — whole cross-stream token sequences over a CH window."""
    net_rows = fetch_rows(ch_url, days, hours, player, "network_requests",
                          NET_COLS, "player_id, ts, entry_fingerprint")
    event_rows = fetch_rows(ch_url, days, hours, player, "session_events",
                            EVENT_COLS, "player_id, ts")
    net_by_play, ev_by_play = group_by_play(net_rows), group_by_play(event_rows)
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


def severity_for(score, thr):
    return "critical" if score >= thr * 1.5 else "warning"


def _anchor(full, lo, hi):
    """First non-sentinel token in full[lo:hi] → (ts, entry_fingerprint, surface), or None
    if the interval is sentinels only. Event/sentinel tokens carry fp=None → land as
    (0, 'event') so the events read-path joins on (player_id, ts); network rows keep their
    real fingerprint + surface."""
    for ts, fp, surface, token in full[lo:hi]:
        if token in hm.SENTINELS:
            continue
        return (ts, 0, "event") if fp is None else (ts, fp, surface or "net")
    return None


# ---------- TRAIN ----------
def cmd_train(args):
    builds = build_plays(args.ch_url, args.train_days, 0, args.player)
    seqs = [[x[3] for x in b["full"]] for b in builds.values()]
    seqs = [s for s in seqs if len(s) >= 5]
    trained_at = hm.utc_now()
    print(f"#445 HMM train — {len(seqs)} plays over {args.train_days}d, K={args.k} "
          f"→ {args.model_path}")
    if len(seqs) < args.min_train_plays:
        print(f"  {len(seqs)} train plays < {args.min_train_plays}; artifact NOT written.")
        return

    vocab = hm.TokenVocab().fit(seqs)
    model, train_logL = hm.train_hmm(seqs, vocab, K=args.k, restarts=args.restarts)
    if model is None:
        print(f"  training failed (vocab={vocab.n_tokens()}, plays={len(seqs)}); artifact NOT written.")
        return
    labels = hm.auto_label_states(model.emissionprob_, vocab)

    # Transition-surprise threshold: p99 of −log P(state→state) across all training Viterbi
    # paths. The score pass flags moves above this as unexpected_regime_transition.
    surprises = []
    for s in seqs:
        _, states = hm.decode(model, vocab, s)
        if states:
            surprises += hm.transition_surprises(model.transmat_, states)
    trans_thr = hm.p99(surprises)
    n_obs = sum(len(s) for s in seqs)

    cond = hm.serialize_condition(model, vocab, train_logL, len(seqs), n_obs, trans_thr, labels)
    artifact = {"model_version": args.model_version, "trained_at": trained_at,
                "train_window_days": args.train_days, "K": args.k, "restarts": args.restarts,
                "conditions": {CONDITION: cond}}
    print(f"  states={labels} vocab={vocab.n_tokens()} logL={train_logL:.1f} "
          f"trans_thr={trans_thr:.2f} BIC={hm.bic(model, train_logL, n_obs):.1f}")
    if args.dry_run:
        print(f"[dry-run] would write artifact (trained_at={trained_at})")
        return

    size = hm.save_artifact(args.model_path, artifact)
    manifest = {"trained_at": trained_at, "model_version": args.model_version,
                "condition": CONDITION, "train_window_days": args.train_days,
                "n_train_episodes": len(seqs), "threshold": round(trans_thr, 4),
                "n_contexts": args.k, "vocab": vocab.n_tokens(), "max_order": args.k,
                "artifact_path": args.model_path, "artifact_bytes": size}
    body = (json.dumps(manifest) + "\n").encode()
    ch(args.ch_url, f"INSERT INTO {DB}.derived_models FORMAT JSONEachRow", body=body)
    print(f"wrote {size} B artifact (K={args.k}, states={labels}) + 1 manifest row.")


# ---------- SCORE ----------
def cmd_score(args):
    art = hm.load_artifact(args.model_path)
    if not art:
        print(f"#445 HMM score — no artifact at {args.model_path} yet "
              f"(trainer hasn't run?); nothing to score.")
        return
    cond = art["conditions"][CONDITION]
    vocab = hm.TokenVocab(cond["idx2tok"])
    model = hm.model_from_artifact(cond)
    labels, thr = cond["state_labels"], cond["trans_thr"]
    trained_at = art["trained_at"]

    builds = build_plays(args.ch_url, 0, args.score_hours, args.player)
    # Out-of-sample by construction: only plays that finished AFTER the corpus closed.
    score_plays = [(p, b) for p, b in builds.items() if b["max_ts"] > trained_at]
    scored_at = hm.utc_now()
    print(f"#445 HMM score — model {art['model_version']} trained_at={trained_at}; "
          f"{len(builds)} plays in last {args.score_hours}h, {len(score_plays)} out-of-sample")

    rows, n_regime, n_trans = [], 0, 0
    for play, b in score_plays:
        full, pid = b["full"], b["pid"]
        toks = [x[3] for x in full]
        _, states = hm.decode(model, vocab, toks)
        if not states:
            continue

        # regime_<STATE> — one row at the first real token of each Viterbi interval.
        for lbl, _s_pos, _e_pos, count, start_i in hm.rle_intervals(states, labels):
            a = _anchor(full, start_i, start_i + count)
            if not a:
                continue
            ts, efp, surf = a
            rows.append({"ts": ts, "player_id": pid, "play_id": play,
                         "entry_fingerprint": efp, "surface": surf, "condition": CONDITION,
                         "label": f"regime_{lbl}", "severity": "info", "score": float(count),
                         "model_version": art["model_version"], "scored_at": scored_at})
            n_regime += 1

        # unexpected_regime_transition — surprises[j] is the move LANDING on states[j+1].
        for j, surprise in enumerate(hm.transition_surprises(model.transmat_, states)):
            if surprise < thr:
                continue
            a = _anchor(full, j + 1, j + 2)        # the landing token's own row
            if not a:
                continue
            ts, efp, surf = a
            rows.append({"ts": ts, "player_id": pid, "play_id": play,
                         "entry_fingerprint": efp, "surface": surf,
                         "condition": "regime_transition",
                         "label": "unexpected_regime_transition",
                         "severity": severity_for(surprise, thr), "score": round(surprise, 3),
                         "model_version": art["model_version"], "scored_at": scored_at})
            n_trans += 1

    # Dedup identical (row, condition) within this run; keep the most salient. Cross-run
    # dedup is the ReplacingMergeTree(scored_at)'s job.
    best = {}
    for r in rows:
        k = (r["player_id"], r["ts"], r["entry_fingerprint"], r["condition"])
        if k not in best or r["score"] > best[k]["score"]:
            best[k] = r
    rows = list(best.values())

    print(f"emitted {len(rows)} row labels: regime={n_regime} transition={n_trans}")
    if args.dry_run:
        for r in sorted(rows, key=lambda x: -x["score"])[:12]:
            print(f"   {r['severity']:8} {r['label']:28} score={r['score']:6.2f} "
                  f"surf={r['surface']:9} play={r['play_id'][:8]} @ {r['ts']}")
        return
    if not rows:
        print("nothing to write.")
        return
    body = ("\n".join(json.dumps(r) for r in rows) + "\n").encode()
    ch(args.ch_url, f"INSERT INTO {DB}.derived_labels FORMAT JSONEachRow", body=body)
    print(f"inserted {len(rows)} rows into {DB}.derived_labels.")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--mode", choices=["train", "score"], required=True)
    ap.add_argument("--ch-url", default=os.environ.get("FORWARDER_CLICKHOUSE_URL", "http://localhost:8123"))
    ap.add_argument("--model-path", default=DEFAULT_MODEL_PATH, help="gzipped-JSON artifact shared by train+score")
    ap.add_argument("--train-days", type=int, default=7, help="train: corpus window")
    ap.add_argument("--score-hours", type=int, default=2, help="score: how far back to look for out-of-sample plays")
    ap.add_argument("--player", default=None, help="limit to one player_id")
    ap.add_argument("--k", type=int, default=hm.DEFAULT_K, help="number of hidden states")
    ap.add_argument("--restarts", type=int, default=hm.DEFAULT_RESTARTS, help="random restarts to escape EM local optima")
    ap.add_argument("--min-train-plays", type=int, default=hm.MIN_TRAIN_SEQUENCES)
    ap.add_argument("--model-version", default="hmm-tok-1")
    ap.add_argument("--dry-run", action="store_true", help="compute + summarise, do NOT write")
    args = ap.parse_args()
    (cmd_train if args.mode == "train" else cmd_score)(args)


if __name__ == "__main__":
    main()
