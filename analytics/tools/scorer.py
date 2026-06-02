#!/usr/bin/env python3
"""1st-order baseline scorer for #508 — the order-1 floor under tail/peak surprise.

Trains a smoothed 1st-order Markov P(next | prev) over the token alphabet on a CLEAN
reference corpus, then scores token sequences by SURPRISE:
  * peak surprise — the single most-improbable transition, max(−log P(next|prev));
  * count above threshold — # transitions whose surprise exceeds a clean-calibrated bar.
NOT whole-session avg-NLL (rejected — re-derives frequency, length-confounded; see
CORPUS_PLAN.md "Scoring statistic"). This is the back-off LEAF the VOMM extends, and the
clean order-1 ablation baseline.

READ-ONLY (#508): reads the archive via the harness CLI (reuses report.pull /
query_plays); writes nothing.

  python3 analytics/tools/scorer.py [--days 7] [--engine AVPlayer] [--alpha 0.5]

This is a baseline/floor — see the honest caveats it prints. The real test (does ordering
separate good vs bad beyond mere fault-token novelty?) needs more corpus / the VOMM.
"""
import argparse
import collections
import math
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import tokenize as tk   # noqa: E402
import report as rep    # noqa: E402  (reuse pull / query_plays / gi / play_labels)


# ---------- model ----------
def train(sequences, alpha=0.5):
    """1st-order transition model with add-alpha (Laplace) smoothing. #442 uses alpha=0.5."""
    trans = collections.defaultdict(collections.Counter)  # prev -> Counter(next)
    vocab = set()
    for seq in sequences:
        vocab.update(seq)
        for a, b in zip(seq, seq[1:]):
            trans[a][b] += 1
    return {"trans": trans, "V": max(len(vocab), 1), "alpha": alpha}


def _logp(model, prev, nxt):
    a, V = model["alpha"], model["V"]
    ctr = model["trans"].get(prev)
    if ctr is None:                       # unseen context → ~uniform over vocab
        return math.log(1.0 / V)
    return math.log((ctr.get(nxt, 0) + a) / (sum(ctr.values()) + a * V))


def transition_surprises(model, seq):
    return [(-_logp(model, a, b), a, b) for a, b in zip(seq, seq[1:])]


def score(model, seq, threshold):
    s = transition_surprises(model, seq)
    if not s:
        return {"peak": 0.0, "argmax": None, "n_above": 0, "n_trans": 0}
    pk = max(s, key=lambda x: x[0])
    return {"peak": pk[0], "argmax": f"{pk[1]} → {pk[2]}",
            "n_above": sum(1 for x in s if x[0] >= threshold), "n_trans": len(s)}


# ---------- corpus ----------
def seqs_for(play_ids, limit=5000):
    out = []
    for pid in play_ids:
        net = rep.pull("network", pid, limit)
        if net:
            out.append(tk.tokenize(net))
    return out


def select(plays, engine):
    """(clean, fault) iOS play-id lists. Clean = substantial, no faults/errors/shaping."""
    eng = [p for p in plays if p.get("player_tech") == engine]
    fault, clean = [], []
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


def pctiles(vals):
    if not vals:
        return (0.0, 0.0, 0.0)
    v = sorted(vals)
    q = lambda f: v[min(len(v) - 1, int(f * len(v)))]
    return (q(0.5), q(0.9), q(0.99))


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--days", type=int, default=7)
    ap.add_argument("--engine", default="AVPlayer")
    ap.add_argument("--alpha", type=float, default=0.5)
    ap.add_argument("--max-fault", type=int, default=60, help="cap fault test sequences")
    args = ap.parse_args()

    plays, frm = rep.query_plays(args.days)
    clean_ids, fault_ids = select(plays, args.engine)
    clean_seqs = seqs_for(clean_ids)
    fault_seqs = seqs_for(fault_ids[:args.max_fault])
    print(f"#508 baseline scorer (1st-order, α={args.alpha}) — {args.engine}, {args.days}d since {frm}")
    print(f"clean training corpus: {len(clean_seqs)} sessions ; fault test: {len(fault_seqs)} sessions\n")
    if len(clean_seqs) < 3:
        print("too few clean sessions to train — need more clean corpus (a known #508 gap).")
        return

    # Calibrate the count-above threshold from in-sample clean transition surprises (p99).
    full = train(clean_seqs, args.alpha)
    clean_surpr = [s for seq in clean_seqs for s, _, _ in transition_surprises(full, seq)]
    _, _, thr = pctiles(clean_surpr)
    print(f"clean transition-surprise p50/p90/p99 = {'/'.join(f'{x:.1f}' for x in pctiles(clean_surpr))} nats"
          f"  → count-above threshold = {thr:.1f}")

    # Clean baseline: leave-one-out (score each clean session under a model trained on the rest).
    clean_peaks = []
    for i, seq in enumerate(clean_seqs):
        m = train(clean_seqs[:i] + clean_seqs[i + 1:], args.alpha)
        clean_peaks.append(score(m, seq, thr)["peak"])
    # Fault sessions scored under the full clean model.
    fault_peaks, argmaxes = [], collections.Counter()
    for seq in fault_seqs:
        r = score(full, seq, thr)
        fault_peaks.append(r["peak"])
        if r["argmax"]:
            argmaxes[r["argmax"]] += 1

    print(f"\npeak-surprise (nats)  median / p90 / max")
    print(f"  clean (leave-one-out): {'/'.join(f'{x:.1f}' for x in (pctiles(clean_peaks)[0], pctiles(clean_peaks)[1], max(clean_peaks) if clean_peaks else 0))}")
    print(f"  fault sessions:        {'/'.join(f'{x:.1f}' for x in (pctiles(fault_peaks)[0], pctiles(fault_peaks)[1], max(fault_peaks) if fault_peaks else 0))}")
    sep = (pctiles(fault_peaks)[0] - pctiles(clean_peaks)[0]) if clean_peaks and fault_peaks else 0
    print(f"  → median peak-surprise separation (fault − clean): {sep:+.1f} nats")
    print("\nmost-surprising transition driving each fault session's peak (top 8):")
    for k, c in argmaxes.most_common(8):
        print(f"  {c:3}  {k}")

    print("\nCAVEAT: this baseline mostly fires on FAULT tokens being novel to a clean-trained\n"
          "model — i.e. it largely re-detects 'a fault happened', ≈ the trivial fault counter.\n"
          "The real question (does ORDERING separate good vs bad beyond fault-presence?) needs\n"
          "more clean corpus + the VOMM + a contrastive setup. This establishes the machinery\n"
          "and the order-1 floor, nothing more.")


if __name__ == "__main__":
    main()
