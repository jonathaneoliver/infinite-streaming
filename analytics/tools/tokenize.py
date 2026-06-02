#!/usr/bin/env python3
"""Baseline request-ordering tokenizer for the #508 sequence-anomaly model.

Turns a play's `network_requests` stream into the delta-encoded token sequence the
variable-order back-off model (and the 1st-order baseline) will consume. This is the
shared substrate, not the model itself.

SCOPE (#508): READ-ONLY. This tool only reads the archive (`harness query`) and emits
tokens/scores locally. It must NOT write labels[], derived_labels, classification, or
anything the dashboard renders — surfacing interesting sessions in sessions.html is
#506's job, deliberately decoupled from the model so model logic never touches ingest.
Do not add a write path here.

It bakes in the Phase-0 reconciliation findings (GitHub #508, 2026-06-01):

  * STARTUP_RAMP   — opening low-rendition segment(s) before the sustained rendition.
                     A real request-visible ΔP transition that no player counts as a
                     shift; flagged so the model treats it as known-benign.
  * re-fetch excursion vs sustained switch — a single low-rendition segment at a
                     backward segment number that immediately reverts is a re-fetch /
                     backward-jump landing grab, NOT a displayed rendition switch (the
                     player typically re-downloads several segments after it). Emitted as
                     V_PROBE, kept DISTINCT from a sustained V_SEG ΔP move. This is a
                     re-labelling, NOT a suppression: a lone V_PROBE is benign, but a
                     cluster of them (e.g. around stalls) is exactly the structural
                     anomaly the surprise model should flag. Whether a given cluster is a
                     user scrub (benign) or an involuntary re-fetch (pathological) cannot
                     be decided from ordering alone — that needs playhead/seek/buffer
                     state (#445 HMM / enriched alphabet). See the 2026-06-01 finding.
  * LOOP_BOUNDARY  — segment number resets to a low value (looped content / rewind).

Token vocab (subset of the #442 alphabet, enough for the baseline):
  <S> <E>                       session sentinels
  V_SEG(dP,dS) / A_SEG(dP,dS)   segment request, profile/segnum deltas clamped [-2,+2]
  V_PROBE(dP)                   single-segment rendition excursion / backward-jump
                                re-fetch (distinct from a shift; score clusters, don't drop)
  STARTUP_RAMP                  opening ramp marker
  LOOP_BOUNDARY                 segnum reset / large backward jump
  V_PL(profile) / A_PL          playlist refresh
  M_PL                          master/multivariant playlist
  FAULT(surface, class)         a faulted/aborted request. class ∈ {4xx, 404, auth,
                                5xx, client_abandon, server_partial, injected_reset,
                                corruption, other}, mapped from fault_category/fault_type
                                (or HTTP status). client_abandon is player-initiated
                                (behaviour grammar, not a reaction antecedent — agency
                                caveat); the rest are server/transport-imposed. See
                                CORPUS_PLAN.md "Fault-class taxonomy".

Usage:
  tokenize.py --play <uuid>                 # pulls via harness CLI
  tokenize.py --from-file net_<uuid>.json   # pre-pulled `query network --json`
  tokenize.py --play <uuid> --json          # emit token list as JSON
  tokenize.py --from-file f.json --episodes --anchor FAULT --lead 4 --horizon 8
                                            # fixed-length windows around each anchor token
                                            # (episode length becomes a deliberate knob,
                                            #  not an artifact of the play boundary)
"""
import argparse
import json
import re
import subprocess
import sys

# Rendition ladder → ordinal. Extend if new heights appear.
RENDITION_ORDINAL = {"360p": 0, "540p": 1, "720p": 2, "1080p": 3, "1440p": 4, "2160p": 5}

SEG_RE = re.compile(r"/(\d{3,4}p)/segment_(\d+)\.(?:m4s|ts|mp4)")
AUDIO_SEG_RE = re.compile(r"/audio/segment_(\d+)\.(?:m4s|ts|mp4)")
VIDEO_PL_RE = re.compile(r"/playlist[^/]*_(\d{3,4}p)\.m3u8")
AUDIO_PL_RE = re.compile(r"/playlist[^/]*_audio\.m3u8")
MASTER_RE = re.compile(r"\.(?:m3u8|mpd)$")  # fallback master/MVP/DASH manifest

# A backward segnum jump larger than this is treated as a loop boundary, not a probe.
LOOP_BACKWARD_THRESHOLD = 5
# Minimum consecutive segments at a new rendition to count it as a sustained switch.
SUSTAIN_MIN = 2


def clamp(v, lo=-2, hi=2):
    return max(lo, min(hi, v))


def fetch_network(play_uuid):
    """Pull `query network --json` for a play via the harness CLI."""
    out = subprocess.run(
        ["harness", "--insecure", "--json", "query", "network", play_uuid, "--limit", "5000"],
        capture_output=True, text=True,
    )
    if out.returncode != 0 or not out.stdout.lstrip().startswith(("{", "[")):
        sys.exit(f"harness query network failed: {out.stdout.strip() or out.stderr.strip()}")
    doc = json.loads(out.stdout)
    return doc["items"] if isinstance(doc, dict) and "items" in doc else doc


def load_file(path):
    doc = json.load(open(path))
    return doc["items"] if isinstance(doc, dict) and "items" in doc else doc


def classify_row(url):
    """Return (kind, rendition_or_None, segnum_or_None)."""
    m = SEG_RE.search(url)
    if m:
        return "V_SEG", m.group(1), int(m.group(2))
    m = AUDIO_SEG_RE.search(url)
    if m:
        return "A_SEG", None, int(m.group(1))
    m = VIDEO_PL_RE.search(url)
    if m:
        return "V_PL", m.group(1), None
    if AUDIO_PL_RE.search(url):
        return "A_PL", None, None
    if MASTER_RE.search(url):
        return "M_PL", None, None
    return "OTHER", None, None


# session_events lifecycle markers (player_metrics.last_event) → cross-stream tokens.
EVENT_TOKEN_MAP = {
    "stall_start": "STALL_START", "stall_end": "STALL_END",
    "buffering_start": "BUF_START", "buffering_end": "BUF_END",
    "rate_shift_up": "RATE_UP", "rate_shift_down": "RATE_DOWN",
    "video_first_frame": "FIRST_FRAME", "segment_stall": "SEGMENT_STALL",
    "timejump": "TIMEJUMP",
}


def event_tokens(event_rows):
    """[(ts, token)] from session_events lifecycle markers. Heartbeats/unknowns dropped."""
    out = []
    for r in event_rows:
        pm = r.get("player_metrics") or {}
        tok = EVENT_TOKEN_MAP.get(pm.get("last_event"))
        if tok:
            out.append((r.get("ts") or r.get("timestamp") or pm.get("event_time") or "", tok))
    return out


def tokenize(rows, event_rows=None):
    """rows: network_requests dicts. event_rows (optional): session_events dicts to
    interleave by timestamp (cross-stream). Returns a list of token strings."""
    rows = [r for r in rows if r.get("url")]
    rows.sort(key=lambda r: r.get("ts") or r.get("timestamp") or "")

    # First pass: pull the ordered video-segment (rendition, segnum) stream so we can
    # decide sustained-vs-transient with one segment of lookahead.
    vseg_idx = []  # (row_index, rendition, segnum)
    for i, r in enumerate(rows):
        kind, rend, seg = classify_row(r["url"])
        # Faulted/abandoned segment fetches did not deliver a rendition, so they
        # must not advance the rendition baseline (they emit a FAULT token instead).
        if kind == "V_SEG" and rend in RENDITION_ORDINAL and not _is_faulted(r):
            vseg_idx.append((i, rend, seg))

    # Mark the startup ramp: leading run before the first sustained rendition settles.
    startup_until = -1
    if vseg_idx:
        first_rend = vseg_idx[0][1]
        # ramp = leading segments whose rendition differs from the first rendition that
        # then persists for >= SUSTAIN_MIN segments.
        k = 0
        while k < len(vseg_idx) and vseg_idx[k][1] != _first_sustained(vseg_idx):
            k += 1
        startup_until = vseg_idx[k - 1][0] if k > 0 else -1

    # Build per-video-segment classification: probe (transient) vs sustained ΔP.
    probe_rows = set()
    for j, (ridx, rend, seg) in enumerate(vseg_idx):
        if j == 0:
            continue
        prev_rend, prev_seg = vseg_idx[j - 1][1], vseg_idx[j - 1][2]
        nxt_rend = vseg_idx[j + 1][1] if j + 1 < len(vseg_idx) else None
        backward = prev_seg is not None and seg is not None and seg < prev_seg - 1
        reverts = nxt_rend == prev_rend
        if rend != prev_rend and reverts and backward:
            probe_rows.add(ridx)  # single-segment excursion at a backward segnum

    # ts-tagged emission so session_events can be interleaved by timestamp.
    seq = []          # (ts, token)
    state = {"ts": ""}
    def emit(tok):
        seq.append((state["ts"], tok))

    last_vrend = None
    last_vseg = None
    last_aseg = None
    ramp_emitted = False

    for i, r in enumerate(rows):
        state["ts"] = r.get("ts") or r.get("timestamp") or ""
        kind, rend, seg = classify_row(r["url"])
        if _is_faulted(r):
            emit(f"FAULT({_surface(kind)},{_fault_class(r)})")
            continue

        if kind == "V_SEG" and rend in RENDITION_ORDINAL:
            if not ramp_emitted and i <= startup_until and startup_until >= 0:
                emit("STARTUP_RAMP")
                ramp_emitted = True
            # loop boundary: large backward segnum reset
            if last_vseg is not None and seg is not None and seg < last_vseg - LOOP_BACKWARD_THRESHOLD:
                emit("LOOP_BOUNDARY")
            dP = clamp(RENDITION_ORDINAL[rend] - (RENDITION_ORDINAL.get(last_vrend, RENDITION_ORDINAL[rend])))
            dS = clamp((seg - last_vseg) if (last_vseg is not None and seg is not None) else 1)
            if i in probe_rows:
                emit(f"V_PROBE({dP:+d})")
            else:
                emit(f"V_SEG({dP:+d},{dS:+d})")
                last_vrend = rend  # only sustained moves update the baseline rendition
            last_vseg = seg
        elif kind == "A_SEG":
            dS = clamp((seg - last_aseg) if (last_aseg is not None and seg is not None) else 1)
            emit(f"A_SEG(+0,{dS:+d})")
            last_aseg = seg
        elif kind == "V_PL":
            emit(f"V_PL({rend})")
        elif kind == "A_PL":
            emit("A_PL")
        elif kind == "M_PL":
            emit("M_PL")
        # OTHER (key, init, etc.) intentionally dropped from the baseline alphabet.

    # Cross-stream: interleave session_events lifecycle tokens by timestamp. Stable sort
    # keeps request-token order within equal timestamps.
    if event_rows:
        seq.extend(event_tokens(event_rows))
    seq.sort(key=lambda x: x[0] or "")
    return ["<S>"] + [t for _, t in seq] + ["<E>"]


def episodes(tokens, anchor="FAULT", lead=4, horizon=8):
    """Slice fixed-length windows around each anchor token within ONE play's sequence.

    Makes episode length a deliberate knob instead of an artifact of the play boundary
    (which under fault injection is cut short by restart). Each anchor token whose text
    starts with `anchor` (a prefix, e.g. "FAULT", "FAULT(video_seg", "V_PROBE") yields a
    window of [lead tokens before] + the anchor + [horizon tokens after], clipped at the
    sequence bounds. Windows are per-play (callers tokenize one play at a time) so they
    never span a <S>/<E> from a different play.

    Per #442: the `lead` is the context the back-off model conditions on; the `horizon`
    is the bounded outcome attribution. For PREDICTIVE training, condition on `lead`+anchor
    only and don't leak the `horizon` outcome into the context — both are returned
    separately so the caller decides.
    """
    out = []
    for i, t in enumerate(tokens):
        if t.startswith(anchor):
            lo = max(0, i - lead)
            hi = min(len(tokens), i + horizon + 1)
            out.append({
                "anchor_index": i,
                "anchor": t,
                "lead": tokens[lo:i],
                "horizon": tokens[i + 1:hi],
                "window": tokens[lo:hi],
            })
    return out


def _first_sustained(vseg_idx):
    """Rendition that first persists for >= SUSTAIN_MIN consecutive segments."""
    run_rend, run_len = None, 0
    for _, rend, _seg in vseg_idx:
        if rend == run_rend:
            run_len += 1
        else:
            run_rend, run_len = rend, 1
        if run_len >= SUSTAIN_MIN:
            return run_rend
    return vseg_idx[0][1] if vseg_idx else None


def _surface(kind):
    return {"V_SEG": "video_seg", "A_SEG": "audio_seg", "V_PL": "playlist",
            "A_PL": "playlist", "M_PL": "master"}.get(kind, "other")


def _is_faulted(r):
    """A row is faulted if the proxy stamped a fault, or the HTTP status is >=400.

    The read API stamps fault_type/fault_category on the body-copy path; transport
    aborts / client disconnects can carry status 200 (or 0), so status alone misses them.
    """
    if str(r.get("fault_type") or "").strip() or str(r.get("fault_category") or "").strip():
        return True
    return _int(r.get("status")) >= 400


def _fault_class(r):
    """Map fault_category/fault_type (or HTTP status) to a taxonomy class.

    Mirrors CORPUS_PLAN.md's FAULT(surface,class) taxonomy. Keep coarse; hierarchical
    back-off (FAULT(surface,5xx) -> FAULT(surface,*) -> FAULT(*,*)) handles sparsity, and
    a divergence test promotes/merges classes later.
    """
    cat = str(r.get("fault_category") or "").strip()
    ftype = str(r.get("fault_type") or "").strip()
    if cat == "client_disconnect":
        return "client_abandon"   # player-initiated; behaviour grammar, not a reaction
    if cat == "transfer_timeout":
        return "server_partial"
    if cat in ("socket", "transport"):
        return "injected_reset"
    if cat == "corruption":
        return "corruption"
    # http / status-derived. fault_type for http rows is the numeric code as a string.
    code = _int(r.get("status")) or _int(ftype)
    if code >= 500:
        return "5xx"
    if code in (401, 403):
        return "auth"
    if code == 404:
        return "404"
    if code >= 400:
        return "4xx"
    return "other"


def _int(v):
    try:
        return int(float(v))
    except (TypeError, ValueError):
        return 0


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    src = ap.add_mutually_exclusive_group(required=True)
    src.add_argument("--play", help="play UUID (pulls via harness CLI)")
    src.add_argument("--from-file", help="path to a `query network --json` dump")
    ap.add_argument("--json", action="store_true", help="emit as JSON")
    ap.add_argument("--episodes", action="store_true",
                    help="emit fixed-length windows around anchor tokens instead of the whole sequence")
    ap.add_argument("--anchor", default="FAULT",
                    help="anchor token prefix for --episodes (default FAULT; e.g. 'FAULT(video_seg', 'V_PROBE')")
    ap.add_argument("--lead", type=int, default=4, help="lead-in context tokens before the anchor (--episodes)")
    ap.add_argument("--horizon", type=int, default=8, help="outcome-horizon tokens after the anchor (--episodes)")
    args = ap.parse_args()

    rows = fetch_network(args.play) if args.play else load_file(args.from_file)
    tokens = tokenize(rows)

    if args.episodes:
        eps = episodes(tokens, anchor=args.anchor, lead=args.lead, horizon=args.horizon)
        if args.json:
            print(json.dumps(eps))
            return
        print(f"episodes: {len(eps)}  (anchor={args.anchor!r} lead={args.lead} horizon={args.horizon})")
        for e in eps[:20]:
            print(f"  @{e['anchor_index']:<4} {' '.join(e['lead'])}  [[ {e['anchor']} ]]  {' '.join(e['horizon'])}")
        return

    if args.json:
        print(json.dumps(tokens))
        return
    # human summary
    from collections import Counter
    head = [t.split("(")[0] for t in tokens]
    print(f"tokens: {len(tokens)}")
    print(f"distinct kinds: {dict(Counter(head))}")
    print(" ".join(tokens[:80]) + (" ..." if len(tokens) > 80 else ""))


if __name__ == "__main__":
    main()
