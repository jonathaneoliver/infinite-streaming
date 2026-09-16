#!/usr/bin/env python3
"""#445 HMM verification harness — the analysis mode behind the production deriver.

`hmm_deriver.py` is the live path (train → artifact → derived_labels). THIS tool is how you
decide the deriver is trustworthy before turning it on, and re-validate after the alphabet
or corpus shifts. It reads ClickHouse directly (same substrate as the deriver), trains on a
clean 80% split, scores the held-out clean 20% vs the failed plays, and writes a report
covering the issue's verification checklist:

  * K-sweep over {3,5,7,9} with aggregate BIC — pick the K the deriver should default to.
  * Per-state top-emission tables — are the K states labelable (STARTUP/STEADY/…)?
  * Per-token log-likelihood, clean-holdout vs failed — does the HMM separate them?
  * State ribbons for the most anomalous failed plays — does a known-bad play actually
    show meaningful RECOVERING/STALLED time?

Clean vs failed is read from the token stream itself (no extra columns): a play is FAILED
if it has any FAULT(...) / STALL_START / SEGMENT_STALL token, CLEAN if it reached
FIRST_FRAME with none of those. Trains ONE global HMM (per-bucket is a deriver follow-up).

  python3 analytics/tools/hmm_scorer.py --days 7 --k 5 --out /tmp/hmm_report
  python3 analytics/tools/hmm_scorer.py --days 7 --k-sweep 3,5,7,9 --out /tmp/hmm_sweep
"""
import argparse
import importlib.util
import json
import os
import sys
from pathlib import Path

HERE = os.path.dirname(os.path.abspath(__file__))
# Remove our dir from sys.path and load siblings by file path so our tokenize.py can't
# shadow the stdlib `tokenize` that scipy (via hmmlearn) needs — see hmm_deriver.py's note.
sys.path[:] = [p for p in sys.path if os.path.abspath(p or os.getcwd()) != HERE]


def _load(name, filename):
    spec = importlib.util.spec_from_file_location(name, os.path.join(HERE, filename))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


hm = _load("hmm_model", "hmm_model.py")
hd = _load("hmm_deriver", "hmm_deriver.py")    # build_plays() + CH reads (whole-play seqs)


def classify(toks):
    """(is_clean, is_failed) from the token stream alone — see module docstring."""
    failed = any(t.startswith("FAULT(") for t in toks) or any(
        t in ("STALL_START", "SEGMENT_STALL") for t in toks)
    clean = ("FIRST_FRAME" in toks) and not failed
    return clean, failed


def fold(play_id):
    """Deterministic 0..4 bucket from the play UUID — every 5th clean play is held out."""
    try:
        return int(play_id[:8], 16) % 5
    except ValueError:
        return 0


def gather(ch_url, days, player, min_tokens=5):
    """[(play_id, toks, is_clean, is_failed)] over a CH window."""
    builds = hd.build_plays(ch_url, days, 0, player)
    out = []
    for play, b in builds.items():
        toks = [x[3] for x in b["full"]]
        if len(toks) < min_tokens:
            continue
        c, f = classify(toks)
        out.append((play, toks, c, f))
    return out


def _stats(v):
    if not v:
        return "n=0"
    s = sorted(v)
    p = lambda f: s[min(len(s) - 1, int(f * len(s)))]
    return f"n={len(s)} p10={p(0.1):.3f} p50={p(0.5):.3f} p90={p(0.9):.3f}"


def train_and_score(samples, K, restarts):
    """Train on the clean 80% split; Viterbi-score every play. Returns
    (model, vocab, labels, results, meta) or (None, ...) when the clean corpus is too thin."""
    clean = [(p, t) for p, t, c, _f in samples if c]
    train = [(p, t) for p, t in clean if fold(p) != 0]
    if len(train) < hm.MIN_TRAIN_SEQUENCES:
        return None, None, None, [], {"train_n": len(train)}

    vocab = hm.TokenVocab().fit([t for _p, t, _c, _f in samples])   # all tokens → stable indices
    model, train_logL = hm.train_hmm([t for _p, t in train], vocab, K=K, restarts=restarts)
    if model is None:
        return None, None, None, [], {"train_n": len(train)}
    labels = hm.auto_label_states(model.emissionprob_, vocab)
    n_obs = sum(len(t) for _p, t in train)

    results = []
    for play, toks, is_clean, is_failed in samples:
        ll, states = hm.decode(model, vocab, toks)
        if ll is None:
            continue
        held_out_clean = is_clean and fold(play) == 0
        results.append({
            "play_id": play, "n_tokens": len(toks),
            "is_clean": is_clean, "is_failed": is_failed, "held_out_clean": held_out_clean,
            "log_likelihood": ll, "per_token_ll": ll / max(len(toks), 1),
            "state_distribution": hm.state_distribution(states, labels),
            "state_intervals": [
                {"label": lbl, "count": cnt}
                for lbl, _s, _e, cnt, _i in hm.rle_intervals(states, labels)],
        })
    meta = {"train_n": len(train), "train_logL": train_logL, "n_observations": n_obs,
            "bic": hm.bic(model, train_logL, n_obs), "K": K, "vocab": vocab.n_tokens()}
    return model, vocab, labels, results, meta


def write_report(model, vocab, labels, results, meta, out_dir, K):
    out = Path(out_dir)
    out.mkdir(parents=True, exist_ok=True)
    (out / "scores.json").write_text(json.dumps(results, indent=2))

    held = sorted(r["per_token_ll"] for r in results if r["held_out_clean"])
    bad = sorted(r["per_token_ll"] for r in results if r["is_failed"])
    md = [f"# HMM verification report (K={K})\n",
          f"Trained on {meta['train_n']} clean plays (80% split), vocab={meta['vocab']}, "
          f"train logL={meta['train_logL']:.1f}, BIC={meta['bic']:.1f}.\n",
          "## Per-token log-likelihood — higher = more model-consistent\n",
          f"- Held-out clean: {_stats(held)}",
          f"- Failed: {_stats(bad)}\n",
          "Separation is healthy when failed p50 sits clearly BELOW held-out-clean p50.\n",
          "## Hidden states (top emissions) — are they labelable?\n",
          "| state | label | top emissions (tok, P) |", "|---|---|---|"]
    for s in range(model.n_components):
        cells = ", ".join(f"`{t}` {p:.2f}" for t, p in hm.top_emissions(model.emissionprob_, vocab, s)
                          if p > 0.01)
        md.append(f"| {s} | `{labels[s]}` | {cells} |")
    md.append("\n## Most anomalous failed plays (lowest per-token LL)\n")
    for r in sorted([r for r in results if r["is_failed"]], key=lambda r: r["per_token_ll"])[:15]:
        dist = ", ".join(f"`{lbl}` {p:.0%}" for lbl, p in r["state_distribution"].items())
        ribbon = ", ".join(f'{iv["label"]}×{iv["count"]}' for iv in r["state_intervals"][:10])
        md.append(f"### `{r['play_id'][:8]}` per_token_ll={r['per_token_ll']:.3f} "
                  f"(n_tokens={r['n_tokens']})")
        md.append(f"- regimes: {dist}")
        md.append(f"- ribbon: {ribbon}\n")
    (out / "report.md").write_text("\n".join(md))
    return out / "report.md"


def cmd_sweep(samples, k_list, restarts, out_dir):
    out = Path(out_dir)
    out.mkdir(parents=True, exist_ok=True)
    md = ["# HMM K-sweep\n", "| K | train_n | train_logL | aggregate_BIC |", "|---|---|---|---|"]
    for K in k_list:
        print(f"\n=== K={K} ===", file=sys.stderr)
        model, vocab, labels, results, meta = train_and_score(samples, K, restarts)
        if model is None:
            md.append(f"| {K} | {meta.get('train_n', 0)} | - | - |")
            continue
        md.append(f"| {K} | {meta['train_n']} | {meta['train_logL']:.1f} | {meta['bic']:.1f} |")
        write_report(model, vocab, labels, results, meta, out / f"K_{K}", K)
    md.append("\nLowest aggregate_BIC wins: it explains the data with the fewest free "
              "parameters. Set the deriver's --k to that value.\n")
    (out / "k_sweep.md").write_text("\n".join(md))
    print(f"\nwrote {out / 'k_sweep.md'}", file=sys.stderr)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--ch-url", default=os.environ.get("FORWARDER_CLICKHOUSE_URL", "http://localhost:8123"))
    ap.add_argument("--days", type=int, default=7)
    ap.add_argument("--player", default=None, help="limit to one player_id")
    ap.add_argument("--k", type=int, default=hm.DEFAULT_K, help="hidden states (single-run mode)")
    ap.add_argument("--restarts", type=int, default=hm.DEFAULT_RESTARTS)
    ap.add_argument("--k-sweep", default=None, help="comma-separated K values for a BIC sweep, e.g. 3,5,7,9")
    ap.add_argument("--out", default="/tmp/hmm_report")
    args = ap.parse_args()

    print(f"#445 HMM scorer — fetching {args.days}d from {args.ch_url} ...", file=sys.stderr)
    samples = gather(args.ch_url, args.days, args.player)
    nc = sum(1 for _p, _t, c, _f in samples if c)
    nf = sum(1 for _p, _t, _c, f in samples if f)
    print(f"  {len(samples)} plays ({nc} clean / {nf} failed)", file=sys.stderr)

    if args.k_sweep:
        try:
            k_list = [int(x) for x in args.k_sweep.split(",") if x.strip()]
        except ValueError:
            print(f"invalid --k-sweep {args.k_sweep!r}", file=sys.stderr)
            return 1
        cmd_sweep(samples, k_list, args.restarts, args.out)
        return 0

    print(f"\n=== K={args.k} ===", file=sys.stderr)
    model, vocab, labels, results, meta = train_and_score(samples, args.k, args.restarts)
    if model is None:
        print(f"clean corpus too thin to train (train_n={meta['train_n']}, "
              f"need >= {hm.MIN_TRAIN_SEQUENCES}). Widen --days.", file=sys.stderr)
        return 1
    path = write_report(model, vocab, labels, results, meta, args.out, args.k)
    print(f"\nwrote {path} (and scores.json)", file=sys.stderr)
    held = sorted(r["per_token_ll"] for r in results if r["held_out_clean"])
    bad = sorted(r["per_token_ll"] for r in results if r["is_failed"])
    print(f"\nstates: {labels}")
    print(f"held-out clean per-token LL: {_stats(held)}")
    print(f"failed       per-token LL: {_stats(bad)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
