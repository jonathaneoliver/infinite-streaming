#!/usr/bin/env python3
"""Sentence-level narration with controlled pauses, then mux.

The engine will not take direction on pacing — measured: `instruct` changes the
output by 0.00s, and punctuation between sentences buys 0.05-0.16s. So the
pauses are inserted here instead of asked for: each sentence is generated as its
own take and the clips are concatenated with an explicit silence between them.

  - 300ms between ordinary sentences
  - 500ms after a HEADING sentence (a short declarative that introduces what
    follows, e.g. "The package opens into three areas.")

If a cue's audio would overrun its slot in the video, the GAPS shrink first,
down to a floor. Pacing degrades before lines are allowed to collide.
"""
import hashlib, json, os, re, subprocess, sys, time, urllib.request

BASE = "http://127.0.0.1:17493"
PROFILE = os.environ.get("VB_PROFILE", "6e211c58-1fe8-4ddc-bae4-ec20165d44c4")
HERE = os.path.dirname(os.path.abspath(__file__))
DEMO_DIR = os.environ.get("DEMO_DIR", os.path.expanduser("~/Desktop/encoder-demo"))
CUES = os.environ.get("CUES", os.path.join(DEMO_DIR, "cues.json"))
VIDEO = os.environ.get("VIDEO", os.path.join(DEMO_DIR, "encoder-demo.mp4"))
# Distinct output so the two pacing approaches can be compared on the
# same footage rather than one silently replacing the other.
OUTV = os.environ.get("OUTV", os.path.join(DEMO_DIR, "encoder-demo-narrated-paced.mp4"))
# The clip caches follow DEMO_DIR rather than living beside the script: this file
# is in the repo now, and `sentences/` reached 214 wavs on one project.
SENT = os.path.join(DEMO_DIR, "sentences")
CUEW = os.path.join(DEMO_DIR, "cue-audio")

GAP_MS = int(os.environ.get("GAP_MS", "300"))
HEAD_GAP_MS = int(os.environ.get("HEAD_GAP_MS", "500"))
MIN_GAP_MS = int(os.environ.get("MIN_GAP_MS", "120"))
HEAD_MAX_WORDS = 5
MIN_CLIP_S = 0.5


def api_post(path, payload, timeout=600):
    req = urllib.request.Request(BASE + path, data=json.dumps(payload).encode(),
                                 headers={"Content-Type": "application/json"}, method="POST")
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def api_get(path, timeout=60):
    with urllib.request.urlopen(BASE + path, timeout=timeout) as r:
        return json.loads(r.read().decode())


def dur(path):
    o = subprocess.run(["ffprobe", "-v", "error", "-show_entries", "format=duration",
                        "-of", "default=nk=1:nw=1", path], capture_output=True, text=True).stdout.strip()
    try:
        return float(o)
    except ValueError:
        return 0.0


# The pronunciation table lives in pronounce.py, which is the only piece of this
# tooling with a test. Re-exported rather than re-imported at each call site
# because narrator_app.py reaches it as `ns.for_speech`.
sys.path.insert(0, HERE)
from pronounce import SPEECH, for_speech  # noqa: E402,F401


def split_sentences(text):
    parts = [p.strip() for p in re.split(r"(?<=[.!?])\s+", text.strip()) if p.strip()]
    return parts or [text.strip()]


def is_heading(s):
    """A sentence that ANNOUNCES what follows rather than carrying detail.

    "short sentence" is not the same thing: "Local spreads it across the
    machines on this network." is short and is not a heading. Two narrow tests
    instead — a very short label ("Codec.", "The machine timeline.") and an
    explicit enumerator ("First, …", "Second, …") — because a 500ms pause after
    every brief sentence makes the whole track plod.
    """
    t = s.strip()
    if re.match(r"^(First|Second|Third|Fourth|Finally|Next)\b", t):
        return True
    return len(t.split()) <= HEAD_MAX_WORDS and t.endswith(".")


# How long to wait for one take. A healthy server returns a short sentence in
# under two seconds (measured: 47 consecutive takes, 1.5-1.9s each), so the
# default is not a pacing allowance — it is the ceiling for a LONG narration
# line on a busy machine.
#
# Callers auditioning one-second clips should pass something short. A wedged
# server keeps answering, keeps reporting "generating", and never finishes, so
# the only thing separating "slow" from "dead" is this number: at 600 it looked
# exactly like a slow success for ten minutes per clip.
GEN_TIMEOUT_S = 600


def generate(text, dest, profile=None, engine=None, timeout_s=GEN_TIMEOUT_S):
    if os.path.exists(dest) and dur(dest) >= MIN_CLIP_S:
        return dur(dest)
    payload = {"profile_id": profile or PROFILE, "text": text}
    # Preset (non-cloned) voices run on a different engine and the server
    # REJECTS the mismatch rather than picking for you: "Preset profile X only
    # supports engine 'kokoro', not 'qwen'".
    if engine:
        payload["engine"] = engine
    r = api_post("/generate", payload)
    gid = r["id"]
    status = r.get("status")
    for _ in range(int(timeout_s)):
        if status in ("completed", "complete", "done", "ready", "failed", "error"):
            break
        time.sleep(1)
        try:
            status = api_get("/history/" + gid).get("status", status)
        except Exception as e:
            print("      poll: %s" % e)
    if status not in ("completed", "complete", "done", "ready"):
        print("      gave up at status=%s" % status)
        return 0.0
    with urllib.request.urlopen(BASE + "/audio/" + gid, timeout=300) as a:
        data = a.read()
    open(dest, "wb").write(data)
    d = dur(dest)
    if d < MIN_CLIP_S:                       # empty take -> one retry, then say so
        r2 = api_post("/generate", {"profile_id": PROFILE, "text": text, "seed": 4242})
        gid2 = r2["id"]
        for _ in range(600):
            st = api_get("/history/" + gid2).get("status")
            if st in ("completed", "complete", "done", "ready", "failed", "error"):
                break
            time.sleep(1)
        with urllib.request.urlopen(BASE + "/audio/" + gid2, timeout=300) as a:
            open(dest, "wb").write(a.read())
        d = dur(dest)
        if d < MIN_CLIP_S:
            print("      STILL EMPTY: %s" % text[:50])
    return d


def build_cue(idx, sentences, gaps_ms, out):
    """Concatenate sentence takes with explicit silences between them."""
    args = ["ffmpeg", "-y"]
    for s in sentences:
        args += ["-i", s]
    for g in gaps_ms:
        args += ["-f", "lavfi", "-t", "%.3f" % (g / 1000.0), "-i", "anullsrc=r=24000:cl=mono"]
    seq, n = [], len(sentences)
    for i in range(n):
        seq.append("[%d:a]" % i)
        if i < len(gaps_ms):
            seq.append("[%d:a]" % (n + i))
    filt = "".join(seq) + "concat=n=%d:v=0:a=1[out]" % (n + len(gaps_ms))
    args += ["-filter_complex", filt, "-map", "[out]", "-ar", "24000", "-ac", "1", out]
    p = subprocess.run(args, capture_output=True, text=True)
    if p.returncode != 0:
        print("      concat failed: %s" % p.stderr[-300:])
        return 0.0
    return dur(out)


def main():
    cues = json.load(open(CUES))["cues"]
    os.makedirs(SENT, exist_ok=True)
    os.makedirs(CUEW, exist_ok=True)
    print("%d cues, gaps %dms / %dms after headings" % (len(cues), GAP_MS, HEAD_GAP_MS))

    built = []
    for i, c in enumerate(cues):
        if not c["text"].strip():
            print("  [%02d] deleted — no caption, no voice" % i)
            continue
        text = for_speech(c["text"])
        sents = split_sentences(text)
        paths, total = [], 0.0
        for j, s in enumerate(sents):
            h = hashlib.sha1(s.encode()).hexdigest()[:10]
            dest = os.path.join(SENT, "s-%s.wav" % h)
            d = generate(s, dest)
            paths.append(dest)
            total += d
        gaps = [HEAD_GAP_MS if is_heading(sents[j]) else GAP_MS for j in range(len(sents) - 1)]
        # slot = distance to the next cue; shrink gaps before overlapping
        slot = (cues[i + 1]["at"] - c["at"]) if i + 1 < len(cues) else None
        if slot is not None and gaps:
            want = total + sum(gaps) / 1000.0
            if want > slot:
                room = max(0.0, slot - total)
                scale = room / (sum(gaps) / 1000.0) if sum(gaps) else 0
                gaps = [max(MIN_GAP_MS, int(g * scale)) for g in gaps]
                print("  [%02d] tightened gaps to fit (%.1fs of speech in a %.1fs slot)" % (i, total, slot))
        out = os.path.join(CUEW, "cue-%02d.wav" % i)
        d = build_cue(i, paths, gaps, out)
        built.append((i, c, out, d))
        flag = ""
        if slot is not None and d > slot:
            flag = "  OVERRUNS by %.1fs" % (d - slot)
        print("  [%02d] %d sentence(s)  %.2fs%s" % (i, len(sents), d, flag))

    print("\n=== mux ===")
    args = ["ffmpeg", "-y", "-i", VIDEO]
    for _, _, p, _ in built:
        args += ["-i", p]
    parts, labels = [], []
    for n, (i, c, p, d) in enumerate(built):
        ms = int(round(c["at"] * 1000))
        parts.append("[%d:a]adelay=%d|%d[a%d]" % (n + 1, ms, ms, n))
        labels.append("[a%d]" % n)
    filt = ";".join(parts) + ";" + "".join(labels) + \
        "amix=inputs=%d:normalize=0:dropout_transition=0[mix]" % len(built)
    args += ["-filter_complex", filt, "-map", "0:v", "-map", "[mix]",
             "-c:v", "copy", "-c:a", "aac", "-b:a", "192k", "-shortest", OUTV]
    p = subprocess.run(args, capture_output=True, text=True)
    if p.returncode != 0:
        print(p.stderr[-1200:]); return 2
    print("  wrote %s  %.1f MB" % (OUTV, os.path.getsize(OUTV) / 1e6))
    return 0


if __name__ == "__main__":
    sys.exit(main())
