#!/usr/bin/env python3
"""VOMM scorer for #508 — variable-order back-off, tail/peak + count-above surprise.

Trains a variable-order back-off model (PPM-C escape) P(next | longest-supported context)
over the token alphabet on a CLEAN reference corpus, then scores sequences by SURPRISE.

Scoring statistic (NOT whole-session avg-NLL — rejected; see CORPUS_PLAN):
  * surprise rate — fraction of transitions whose −log P exceeds a clean-calibrated (p99)
    threshold. Length-normalised and graded (doesn't saturate like raw peak). PRIMARY.
  * peak — the single most-improbable transition (secondary; saturates on novel tokens).

`max_order` is the only model knob: max_order=1 IS the 1st-order Markov (the ablation
floor); max_order=K is the VOMM. Same code, one parameter — the order-1 case is just the
back-off leaf. The run compares the two to answer "does variable-order beat order-1?".

READ-ONLY (#508): reads via the harness CLI (reuses report.pull / query_plays); writes nothing.

  python3 analytics/tools/scorer.py [--days 7] [--engine AVPlayer] [--max-order 4] [--alpha 0.5]
"""
import argparse
import collections
import math
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import tokenize as tk   # noqa: E402
import report as rep    # noqa: E402


# ---------- VOMM model (PPM-C back-off) ----------
def train(sequences, max_order=4, alpha=0.5):
    """counts[m][ctx_tuple] = Counter(next) for context length m in 0..max_order."""
    counts = [collections.defaultdict(collections.Counter) for _ in range(max_order + 1)]
    vocab = set()
    for seq in sequences:
        vocab.update(seq)
        for i in range(1, len(seq)):
            nxt = seq[i]
            for m in range(0, min(max_order, i) + 1):
                ctx = tuple(seq[i - m:i])      # m tokens before i (m=0 → () = unigram)
                counts[m][ctx][nxt] += 1
    return {"counts": counts, "max_order": max_order, "V": max(len(vocab), 1)}


def _ppm_prob(model, history, nxt, m):
    """PPM-C: P(nxt | last m tokens), backing off to shorter context, base = uniform 1/V."""
    if m < 0:
        return 1.0 / model["V"]
    ctx = tuple(history[len(history) - m:]) if m > 0 else ()
    ctr = model["counts"][m].get(ctx)
    if not ctr:                                # unseen context → drop a level
        return _ppm_prob(model, history, nxt, m - 1)
    T = sum(ctr.values())
    D = len(ctr)                               # PPM-C escape mass = distinct types
    if nxt in ctr:
        return ctr[nxt] / (T + D)
    return (D / (T + D)) * _ppm_prob(model, history, nxt, m - 1)


def transition_surprises(model, seq):
    K = model["max_order"]
    out = []
    for i in range(1, len(seq)):
        p = _ppm_prob(model, seq[:i], seq[i], min(K, i))
        out.append((-math.log(p), seq[i - 1], seq[i]))
    return out


def score(model, seq, threshold):
    s = transition_surprises(model, seq)
    if not s:
        return {"rate": 0.0, "n_above": 0, "n_trans": 0, "peak": 0.0, "argmax": None}
    pk = max(s, key=lambda x: x[0])
    n_above = sum(1 for x in s if x[0] >= threshold)
    return {"rate": n_above / len(s), "n_above": n_above, "n_trans": len(s),
            "peak": pk[0], "argmax": f"{pk[1]} → {pk[2]}"}


# ---------- corpus (reused harness) ----------
def seqs_for(play_ids, limit=5000):
    return [tk.tokenize(net) for pid in play_ids if (net := rep.pull("network", pid, limit))]


def select(plays, engine):
    eng = [p for p in plays if p.get("player_tech") == engine]
    clean, fault = [], []
    for p in eng:
        if rep.gi(p, "playing_time_ms") < 30000:
            continue
        lbls = rep.play_labels(p)
        faulted = rep.gi(p, "net_faults") > 0 or any(
            x.startswith(("error=", "warning=*fault", "warning=fault", "warning=http")) for x in lbls)
        shaped = any(any(h in x for h in ("pattern", "shaper", "test_state_residency", "run_id")) for x in lbls)
        if faulted:
            fault.append(p["play_id"])
        elif not shaped and any("first_frame" in x for x in lbls):
            clean.append(p["play_id"])
    return clean, fault


def median(v):
    return sorted(v)[len(v) // 2] if v else 0.0


def p99(v):
    return sorted(v)[min(len(v) - 1, int(0.99 * len(v)))] if v else 0.0


def run_order(order, train_seqs, holdout_seqs, fault_seqs, alpha):
    """Train at this max_order on train_seqs; return clean-holdout vs fault surprise rates."""
    model = train(train_seqs, max_order=order, alpha=alpha)
    thr = p99([s for seq in train_seqs for s, _, _ in transition_surprises(model, seq)])
    clean_rate = median([score(model, s, thr)["rate"] for s in holdout_seqs])
    fr = [score(model, s, thr) for s in fault_seqs]
    fault_rate = median([r["rate"] for r in fr])
    argmaxes = collections.Counter(r["argmax"] for r in fr if r["argmax"])
    return {"order": order, "thr": thr, "clean_rate": clean_rate,
            "fault_rate": fault_rate, "sep": fault_rate - clean_rate, "argmaxes": argmaxes}


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--days", type=int, default=7)
    ap.add_argument("--engine", default="AVPlayer")
    ap.add_argument("--max-order", type=int, default=4)
    ap.add_argument("--alpha", type=float, default=0.5)
    ap.add_argument("--max-fault", type=int, default=60)
    args = ap.parse_args()

    plays, frm = rep.query_plays(args.days)
    clean_ids, fault_ids = select(plays, args.engine)
    clean_seqs = seqs_for(clean_ids)
    fault_seqs = seqs_for(fault_ids[:args.max_fault])
    # deterministic 80/20 clean split: every 5th session is the held-out baseline.
    holdout = [s for i, s in enumerate(clean_seqs) if i % 5 == 0]
    train_seqs = [s for i, s in enumerate(clean_seqs) if i % 5 != 0]
    print(f"#508 VOMM scorer — {args.engine}, {args.days}d since {frm}")
    print(f"clean: train {len(train_seqs)} / holdout {len(holdout)} ; fault test {len(fault_seqs)}\n")
    if len(train_seqs) < 5 or not holdout or not fault_seqs:
        print("corpus too thin to run the ablation (known #508 gap).")
        return

    print("surprise RATE (frac of transitions above clean-p99 bar) — median over sessions:")
    print(f"  {'model':12} {'thr(nats)':>9} {'clean':>8} {'fault':>8} {'separation':>11}")
    results = {}
    for order in sorted({1, args.max_order}):
        r = run_order(order, train_seqs, holdout, fault_seqs, args.alpha)
        results[order] = r
        name = "order-1" if order == 1 else f"VOMM(K={order})"
        print(f"  {name:12} {r['thr']:9.1f} {r['clean_rate']:8.1%} {r['fault_rate']:8.1%} {r['sep']:+11.1%}")

    if args.max_order != 1 and 1 in results:
        d = results[args.max_order]["sep"] - results[1]["sep"]
        verdict = "VOMM improves separation" if d > 0.005 else ("≈ no gain over order-1" if abs(d) <= 0.005 else "VOMM WORSE")
        print(f"\n  → does depth help? Δseparation (VOMM − order-1) = {d:+.1%}  [{verdict}]")

    print(f"\ntop most-surprising transitions in fault sessions (VOMM K={args.max_order}):")
    for k, c in results[args.max_order]["argmaxes"].most_common(8):
        print(f"  {c:3}  {k}")

    print("\nNote: clean corpus is steady-2160p-heavy, so much of the fault separation is\n"
          "fault/rendition-switch NOVELTY against a low-diversity clean model (≈ the trivial\n"
          "fault counter). The honest model win is the Δseparation line: does variable-order\n"
          "buy anything over order-1 here? Episode-anchored scoring + more diverse clean\n"
          "corpus are the next levers.")


if __name__ == "__main__":
    main()
