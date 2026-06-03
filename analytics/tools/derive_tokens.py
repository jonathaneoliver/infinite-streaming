#!/usr/bin/env python3
"""#506 batch derived-token writer — reads network_requests from ClickHouse, computes the
#508 per-row token (reusing tokenize.py — the single source of truth), and writes them to
the `derived_tokens` table. The read API LEFT-JOINs this onto rows so the token is
available everywhere.

Out-of-band / delayed by design (NOT the forwarder ingest path): batch sees the whole play
so the lookahead-dependent tokens (V_PROBE, STARTUP_RAMP) resolve correctly. Idempotent —
re-runs supersede via ReplacingMergeTree(scored_at).

Reads CH directly over HTTP (the read API doesn't project entry_fingerprint, which is the
join key). IDs are written verbatim (already canonical-lowercase in the archive).

  python3 analytics/tools/derive_tokens.py --days 7 [--player <id>] [--dry-run]
  # CH endpoint: --ch-url, or env FORWARDER_CLICKHOUSE_URL (default http://localhost:8123);
  # optional FORWARDER_CLICKHOUSE_USER / _PASSWORD.
"""
import argparse
import base64
import collections
import json
import os
import sys
import urllib.parse
import urllib.request
from datetime import datetime, timezone

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import tokenize as tk  # noqa: E402

DB = "infinite_streaming"
# Columns tokenize_rows needs (classify_row/_is_faulted/_fault_class) + identity + the join key.
SELECT_COLS = "ts, player_id, play_id, entry_fingerprint, url, status, fault_type, fault_category"


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


def fetch_rows(ch_url, days, player):
    where = ["ts >= now64(3) - INTERVAL {days:UInt32} DAY"]
    params = {"days": str(days)}
    if player:
        where.append("player_id = {pid:String}")
        params["pid"] = player
    sql = (f"SELECT {SELECT_COLS} FROM {DB}.network_requests "
           f"WHERE {' AND '.join(where)} ORDER BY player_id, ts, entry_fingerprint")
    out = ch(ch_url, sql, params=params)
    return [json.loads(l) for l in out.splitlines() if l.strip()]


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--ch-url", default=os.environ.get("FORWARDER_CLICKHOUSE_URL", "http://localhost:8123"))
    ap.add_argument("--days", type=int, default=7)
    ap.add_argument("--player", default=None, help="limit to one player_id")
    ap.add_argument("--model-version", default="vomm-tok-1")
    ap.add_argument("--dry-run", action="store_true", help="compute + summarise, do NOT insert")
    args = ap.parse_args()

    rows = fetch_rows(args.ch_url, args.days, args.player)
    by_play = collections.defaultdict(list)
    for r in rows:
        by_play[r.get("play_id")].append(r)
    scored_at = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M:%S.%f")[:-3]

    derived = []
    for play, prows in by_play.items():
        # A play has exactly one player, but the FIRST network row is often a
        # pre-stamp row (the forwarder hasn't resolved session→player yet) with
        # an empty player_id. Stamping prows[0] would blank the whole play and
        # break the read-path join (which matches on player_id). Take the first
        # non-empty player_id instead.
        pid = next((r.get("player_id") for r in prows if r.get("player_id")), "")
        for ts, fp, surface, token in tk.tokenize_rows(prows):
            derived.append({"ts": ts, "player_id": pid, "play_id": play,
                            "entry_fingerprint": fp, "surface": surface, "token": token,
                            "model_version": args.model_version, "scored_at": scored_at})

    print(f"#506 derive_tokens — {len(rows)} rows / {len(by_play)} plays / {args.days}d "
          f"→ {len(derived)} tokens (model={args.model_version})")
    kinds = collections.Counter(t["token"].split("(")[0] for t in derived)
    print("token kinds:", dict(kinds.most_common(10)))
    if args.dry_run:
        print("dry-run — not inserting. sample:")
        for d in derived[:5]:
            print("  ", d)
        return
    if not derived:
        print("nothing to write.")
        return
    body = ("\n".join(json.dumps(d) for d in derived) + "\n").encode()
    ch(args.ch_url, f"INSERT INTO {DB}.derived_tokens FORMAT JSONEachRow", body=body)
    print(f"inserted {len(derived)} rows into {DB}.derived_tokens.")


if __name__ == "__main__":
    main()
