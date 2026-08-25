#!/usr/bin/env python3
"""#445 — where do the VOMM and HMM derivers DISAGREE? (the most informative plays).

Both derivers write to the same derived_labels table with distinct model_version
(vomm-tok-1 vs hmm-tok-1), so this reads CH directly and contrasts the two families PER
PLAY — no scores.json files, no changes to the VOMM tool:

  * VOMM anomaly score = Σ surprise (nats) over a play's unexpected_<condition> rows.
  * HMM  anomaly score = Σ surprise over its unexpected_regime_transition rows;
    its regime ribbon comes from the regime_<STATE> rows (ordered by ts).

Reports Spearman ρ between the two per-play scores and the plays where their ranks diverge
most — the cases that reveal what each model captures that the other can't. ρ in [0.4, 0.7]
is the sweet spot (correlated on the obvious anomalies, each adding signal); ρ > 0.9 means
the HMM is duplicating the VOMM, ρ < 0.2 means one is broken.

Caveat: derived_labels only holds FLAGGED rows — plays both models found clean are absent,
so ρ measures agreement over the flagged population, not all plays. The disagreement tables
(one flags, the other is silent) are the payload.

  python3 analytics/tools/compare_markov_hmm.py --days 7 --out /tmp/compare_markov_hmm.md
"""
import argparse
import base64
import collections
import json
import os
import sys
import urllib.parse
import urllib.request
from pathlib import Path

DB = "infinite_streaming"


def _auth_header():
    u, p = os.environ.get("FORWARDER_CLICKHOUSE_USER"), os.environ.get("FORWARDER_CLICKHOUSE_PASSWORD")
    if u:
        return {"Authorization": "Basic " + base64.b64encode(f"{u}:{p or ''}".encode()).decode()}
    return {}


def ch_rows(ch_url, sql, params):
    q = {"query": sql, "default_format": "JSONEachRow"}
    for k, v in params.items():
        q[f"param_{k}"] = v
    url = ch_url.rstrip("/") + "/?" + urllib.parse.urlencode(q)
    req = urllib.request.Request(url, headers=_auth_header())
    with urllib.request.urlopen(req, timeout=120) as resp:
        return [json.loads(l) for l in resp.read().decode().splitlines() if l.strip()]


def _ranks(xs):
    """Average-rank ranking (ties share the mean rank — what Spearman expects)."""
    indexed = sorted(enumerate(xs), key=lambda p: p[1])
    ranks = [0.0] * len(xs)
    i = 0
    while i < len(indexed):
        j = i
        while j + 1 < len(indexed) and indexed[j + 1][1] == indexed[i][1]:
            j += 1
        avg = (i + j) / 2 + 1
        for k in range(i, j + 1):
            ranks[indexed[k][0]] = avg
        i = j + 1
    return ranks


def _spearman(xs, ys):
    n = len(xs)
    if n < 2:
        return float("nan")
    rx, ry = _ranks(xs), _ranks(ys)
    mx, my = sum(rx) / n, sum(ry) / n
    num = sum((rx[i] - mx) * (ry[i] - my) for i in range(n))
    dx = sum((rx[i] - mx) ** 2 for i in range(n)) ** 0.5
    dy = sum((ry[i] - my) ** 2 for i in range(n)) ** 0.5
    return num / (dx * dy) if dx and dy else float("nan")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--ch-url", default=os.environ.get("FORWARDER_CLICKHOUSE_URL", "http://localhost:8123"))
    ap.add_argument("--days", type=int, default=7)
    ap.add_argument("--vomm-version", default="vomm-tok-1", help="model_version prefix for VOMM rows")
    ap.add_argument("--hmm-version", default="hmm-tok-1", help="model_version prefix for HMM rows")
    ap.add_argument("--top", type=int, default=15)
    ap.add_argument("--out", default="/tmp/compare_markov_hmm.md")
    args = ap.parse_args()

    rows = ch_rows(args.ch_url,
                   f"SELECT play_id, model_version, condition, label, score, ts "
                   f"FROM {DB}.derived_labels "
                   f"WHERE ts >= now64(3) - INTERVAL {{days:UInt32}} DAY "
                   f"ORDER BY play_id, ts",
                   {"days": str(args.days)})
    if not rows:
        print(f"no derived_labels rows in the last {args.days}d.", file=sys.stderr)
        return 1

    # Per play: sum VOMM surprise, sum HMM transition surprise, collect the regime ribbon.
    vomm = collections.defaultdict(float)
    hmm = collections.defaultdict(float)
    ribbon = collections.defaultdict(list)
    for r in rows:
        play, mv, label = r.get("play_id"), r.get("model_version") or "", r.get("label") or ""
        score = float(r.get("score") or 0.0)
        if not play:
            continue
        if mv.startswith(args.vomm_version):
            vomm[play] += score
        elif mv.startswith(args.hmm_version):
            if label == "unexpected_regime_transition":
                hmm[play] += score
            elif label.startswith("regime_"):
                ribbon[play].append(label[len("regime_"):])

    common = sorted(set(vomm) | set(hmm))
    vs = [vomm.get(p, 0.0) for p in common]
    hs = [hmm.get(p, 0.0) for p in common]
    rho = _spearman(vs, hs)
    vr, hr = _ranks(vs), _ranks(hs)

    data = [{"play": p, "vomm": vomm.get(p, 0.0), "hmm": hmm.get(p, 0.0),
             "vrank": vr[i], "hrank": hr[i], "delta": abs(vr[i] - hr[i]),
             "ribbon": " ".join(ribbon.get(p, [])[:8])}
            for i, p in enumerate(common)]

    md = ["# VOMM vs HMM deriver comparison\n",
          f"- Plays flagged by either deriver (last {args.days}d): **{len(common)}** "
          f"(VOMM {len(vomm)}, HMM {len(hmm)}, both {len(set(vomm) & set(hmm))})",
          f"- Spearman ρ (per-play VOMM surprise vs HMM regime-transition surprise): **{rho:.3f}**",
          "",
          "ρ in [0.4, 0.7] = correlated but complementary. > 0.9 → HMM duplicates VOMM; "
          "< 0.2 → one is broken (or the flagged populations barely overlap).\n",
          f"## Top {args.top} plays by rank disagreement\n",
          "| play | VOMM rank | HMM rank | Δrank | VOMM Σsurprise | HMM Σsurprise | HMM regime ribbon |",
          "|---|---|---|---|---|---|---|"]
    for d in sorted(data, key=lambda x: -x["delta"])[:args.top]:
        md.append(f"| `{d['play'][:8]}` | {int(d['vrank']):>4} | {int(d['hrank']):>4} | "
                  f"**{d['delta']:.0f}** | {d['vomm']:.1f} | {d['hmm']:.1f} | `{d['ribbon'][:70]}` |")

    md.append("\n## HMM flags, VOMM silent (top 8) — regime anomalies invisible to point-surprise\n")
    md.append("| play | HMM Σsurprise | HMM regime ribbon |\n|---|---|---|")
    for d in sorted([x for x in data if x["hmm"] > 0 and x["vomm"] == 0],
                    key=lambda x: -x["hmm"])[:8]:
        md.append(f"| `{d['play'][:8]}` | {d['hmm']:.1f} | `{d['ribbon'][:80]}` |")

    md.append("\n## VOMM flags, HMM silent (top 8) — ordering anomalies with no abnormal regime move\n")
    md.append("| play | VOMM Σsurprise | HMM regime ribbon |\n|---|---|---|")
    for d in sorted([x for x in data if x["vomm"] > 0 and x["hmm"] == 0],
                    key=lambda x: -x["vomm"])[:8]:
        md.append(f"| `{d['play'][:8]}` | {d['vomm']:.1f} | `{d['ribbon'][:80] or '(no regime rows)'}` |")

    Path(args.out).write_text("\n".join(md) + "\n")
    print(f"wrote {args.out}", file=sys.stderr)
    print(f"Spearman ρ = {rho:.3f} ({len(common)} flagged plays)", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
