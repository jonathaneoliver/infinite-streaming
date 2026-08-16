#!/usr/bin/env python3
"""Narration editor: scrub a recording, edit its captions, hear the voice, export.

点 The loop this exists to invert: every text change used to cost an ffmpeg pass
before you could see it. Here the caption is a DOM element positioned over the
video's caption strip, so an edit renders instantly and correctly placed, and
the generated speech plays against the video at its own cue time. Rendering
stops being part of editing and becomes a single Export at the end.

General over recordings: point it at any video + cues.json pair.

    python3 narrator_app.py --video demo.mp4 --cues cues.json [--port 8777]

Reuses the pipeline scripts rather than reimplementing them — sentence splitting,
pause insertion and clip caching all come from narrate_sentences, so what you
hear in the browser is what Export writes.
"""
import argparse, hashlib, json, mimetypes, os, re, shutil, subprocess, sys, threading, time, urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# Voices muxed into the output. The FIRST is the default track; the rest become
# alternate tracks a player can switch to.
#
# The engine is NOT a preference — the server enforces it from the profile type.
# A cloned voice runs on qwen (only a cloning model can speak from a reference
# sample); a preset voice runs on the engine that owns its voice id, and asking
# for the wrong one is a 400: "Preset profile X only supports engine 'kokoro'".
VOICE_CATALOG = [
    {"key": "jonathan", "title": "Jonathan", "kind": "cloned",
     "profile": "6e211c58-1fe8-4ddc-bae4-ec20165d44c4", "engine": None},
    {"key": "alice",  "title": "Alice (UK)",  "kind": "preset", "preset": "bf_alice",  "engine": "kokoro"},
    {"key": "emma",   "title": "Emma (UK)",   "kind": "preset", "preset": "bf_emma",   "engine": "kokoro"},
    {"key": "lily",   "title": "Lily (UK)",   "kind": "preset", "preset": "bf_lily",   "engine": "kokoro"},
    {"key": "george", "title": "George (UK)", "kind": "preset", "preset": "bm_george", "engine": "kokoro"},
]
# The clip-naming rule is anchored on this key, NOT on whatever is selected —
# so changing the selection never orphans clips already generated.
PRIMARY_KEY = "jonathan"
VB = "http://127.0.0.1:17493"


def _vb(path, payload=None, method="GET", timeout=120):
    req = urllib.request.Request(VB + path, method=method,
                                 data=json.dumps(payload).encode() if payload else None,
                                 headers={"Content-Type": "application/json"} if payload else {})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def ensure_profile(v):
    """Resolve a catalog entry to a Voicebox profile id, creating it if needed.

    Preset voices are cheap to create and there is no reason to pre-make five
    profiles the user may never select."""
    if v.get("profile"):
        return v["profile"]
    name = v["title"]
    try:
        for p in _vb("/profiles"):
            if p.get("name") == name:
                v["profile"] = p["id"]
                return p["id"]
    except Exception as e:
        print("[voices] list failed: %s" % e)
    p = _vb("/profiles", {"name": name, "language": "en", "voice_type": "preset",
                          "preset_engine": v["engine"], "preset_voice_id": v["preset"],
                          "default_engine": v["engine"]}, method="POST")
    v["profile"] = p["id"]
    return p["id"]


def selected_voices():
    """Voices to render, in order. The first becomes the default audio track."""
    keys = STATE["cues"].get("voices") or [PRIMARY_KEY]
    out = []
    for k in keys:
        v = next((x for x in VOICE_CATALOG if x["key"] == k), None)
        if v:
            out.append(v)
    return out or [VOICE_CATALOG[0]]


PRIMARY = VOICE_CATALOG[0]

# Hover sample. One line, same for every voice, so they are comparable.
SAMPLE_TEXT = "The quick brown fox jumped over the lazy dog."


def sample_path(v):
    os.makedirs(os.path.join(HERE, "samples"), exist_ok=True)
    return os.path.join(HERE, "samples", "voice-%s.wav" % v["key"])


def build_sample(v):
    out = sample_path(v)
    if os.path.exists(out) and ns.dur(out) > 0.2:
        return out
    ensure_profile(v)
    ns.generate(SAMPLE_TEXT, out, profile=v["profile"], engine=v["engine"])
    return out


def presample_all():
    """Generate every voice's sample in the background at startup, so hovering
    plays immediately instead of waiting 7-25s for a first generation."""
    for v in VOICE_CATALOG:
        try:
            if not (os.path.exists(sample_path(v)) and ns.dur(sample_path(v)) > 0.2):
                build_sample(v)
                print("[samples] %s ready" % v["title"], flush=True)
        except Exception as e:
            print("[samples] %s failed: %s" % (v["title"], e), flush=True)

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import narrate_sentences as ns          # for_speech, split_sentences, is_heading, generate, build_cue

STATE = {"video": None, "cues_path": None, "cues": None, "export": {"running": False, "log": [], "phase": "", "pct": 0.0, "detail": ""},
         "queue": [], "current": None, "worker": None, "pending": {}}

# Seconds of quiet after the last edit to a cue before its voice is generated.
# Without this every blur starts a ~25s take, and a line reworked four times
# burns four takes to reach the version that matters.
DEBOUNCE_S = float(os.environ.get("DEBOUNCE_S", "30"))
LOCK = threading.Lock()

# Generation runs in ONE background worker, and the queue holds cue INDICES
# rather than text. Two consequences, both wanted: editing the same line five
# times queues it once, and the worker reads the cue's CURRENT text when it gets
# there — so a superseded edit is never synthesised. Voicebox also serialises
# badly (concurrent requests 500), so one worker is the right shape anyway.
def touch(i):
    """An edit does not generate — it restarts this cue's quiet timer."""
    with LOCK:
        STATE["pending"][i] = time.time()
    start_debouncer()


def start_debouncer():
    t = STATE.get("debouncer")
    if t and t.is_alive():
        return
    t = threading.Thread(target=_debouncer, daemon=True)
    STATE["debouncer"] = t
    t.start()


def _debouncer():
    while True:
        time.sleep(2)
        now = time.time()
        ready = []
        with LOCK:
            for i, at in list(STATE["pending"].items()):
                if now - at >= DEBOUNCE_S:
                    ready.append(i)
                    del STATE["pending"][i]
            more = bool(STATE["pending"])
        for i in ready:
            enqueue(i)
        if not more and not ready:
            return


def enqueue(i):
    with LOCK:
        if i not in STATE["queue"] and STATE["current"] != i:
            STATE["queue"].append(i)
    start_worker()


def start_worker():
    w = STATE.get("worker")
    if w and w.is_alive():
        return
    t = threading.Thread(target=_worker, daemon=True)
    STATE["worker"] = t
    t.start()


def _worker():
    while True:
        with LOCK:
            if not STATE["queue"]:
                STATE["current"] = None
                return
            i = STATE["queue"].pop(0)
            STATE["current"] = i
        try:
            c = STATE["cues"]["cues"][i]
            if c["text"].strip():
                build_cue_audio(c["text"])
        except Exception as e:
            print("[worker] cue %d failed: %s" % (i, e), flush=True)
        finally:
            with LOCK:
                STATE["current"] = None


def load_cues():
    with open(STATE["cues_path"]) as f:
        data = json.load(f)
    STATE["cues"] = data
    return data


def save_cues():
    p = STATE["cues_path"]
    shutil.copy(p, p + ".bak")
    with open(p, "w") as f:
        json.dump(STATE["cues"], f, indent=2)


_DUR = {}


def dur_cached(path):
    """ffprobe is a process spawn; /api/project asked for 42 of them on every
    poll, which timed out whenever the TTS had the CPU. Keyed on (path, mtime)
    so a regenerated clip still re-measures."""
    try:
        st = os.stat(path)
    except OSError:
        return 0.0
    key = (path, st.st_mtime_ns, st.st_size)
    if key not in _DUR:
        _DUR[key] = ns.dur(path)
    return _DUR[key]


def cue_audio_path(text, voice=None):
    """Per-cue audio, keyed on the CUE's text AND the voice.

    Without the voice in the key a second narrator would collide with the first
    — same text, same hash, wrong person. The default voice keeps the original
    naming so the clips already generated are not orphaned."""
    voice = voice or PRIMARY
    h = hashlib.sha1(text.encode()).hexdigest()[:12]
    os.makedirs(ns.CUEW, exist_ok=True)
    if voice["key"] == PRIMARY["key"]:
        return os.path.join(ns.CUEW, "app-%s.wav" % h)
    return os.path.join(ns.CUEW, "app-%s-%s.wav" % (voice["key"], h))


def build_cue_audio(text, voice=None):
    """Same construction Export uses: sentence takes joined by explicit pauses."""
    voice = voice or PRIMARY
    if not text.strip():
        return None, 0.0            # a cleared cue is silent by definition
    out = cue_audio_path(text, voice)
    if os.path.exists(out) and ns.dur(out) > 0.2:
        return out, ns.dur(out)
    spoken = ns.for_speech(text)
    sents = ns.split_sentences(spoken)
    paths = []
    for s in sents:
        h = hashlib.sha1(s.encode()).hexdigest()[:10]
        os.makedirs(ns.SENT, exist_ok=True)
        dest = os.path.join(ns.SENT, "s-%s.wav" % h if voice["key"] == PRIMARY["key"]
                            else "s-%s-%s.wav" % (voice["key"], h))
        ns.generate(s, dest, profile=voice["profile"], engine=voice["engine"])
        paths.append(dest)
    gaps = [ns.HEAD_GAP_MS if ns.is_heading(sents[j]) else ns.GAP_MS for j in range(len(sents) - 1)]
    d = ns.build_cue(0, paths, gaps, out)
    return out, d


def cue_status(i, c):
    if not c["text"].strip():
        cues = STATE["cues"]["cues"]
        slot = (cues[i + 1]["at"] - c["at"]) if i + 1 < len(cues) else None
        return {"i": i, "at": c["at"], "text": "", "spoken": "", "deleted": True,
                "holdMs": c.get("holdMs"), "slot": slot,
                "audio": False, "dur": None, "overrun": False}
    p = cue_audio_path(c["text"])
    have = dur_cached(p) > 0.2
    cues = STATE["cues"]["cues"]
    slot = (cues[i + 1]["at"] - c["at"]) if i + 1 < len(cues) else None
    return {"i": i, "at": c["at"], "text": c["text"], "spoken": ns.for_speech(c["text"]),
            "holdMs": c.get("holdMs"), "slot": slot,
            "audio": bool(have), "dur": dur_cached(p) if have else None,
            "overrun": bool(have and slot and dur_cached(p) > slot)}


def probe_seconds(path):
    o = subprocess.run(["ffprobe", "-v", "error", "-show_entries", "format=duration",
                        "-of", "default=nk=1:nw=1", path], capture_output=True, text=True).stdout.strip()
    try:
        return float(o)
    except ValueError:
        return 0.0


def set_progress(phase, frac, detail=""):
    """Overall percentage across weighted phases.

    Reported per phase rather than as one number because the phases differ by
    two orders of magnitude — audio is minutes, the caption burn is ~10, the
    mux is seconds — so a single bar would sit still and then jump.
    """
    weights = [("audio", 0.30), ("ffwd", 0.25), ("mux", 0.08), ("captions", 0.37)]
    base = 0.0
    for name, w in weights:
        if name == phase:
            STATE["export"]["pct"] = round((base + w * max(0.0, min(1.0, frac))) * 100, 1)
            STATE["export"]["phase"] = phase
            STATE["export"]["detail"] = detail
            return
        base += w
    STATE["export"]["phase"] = phase
    STATE["export"]["detail"] = detail


def run_ffmpeg(args, total_s, phase, detail=""):
    """Run ffmpeg, streaming its own progress rather than guessing."""
    p = subprocess.Popen(args + ["-progress", "pipe:1", "-nostats"],
                         stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, bufsize=1)
    for line in p.stdout:
        if line.startswith("out_time_ms=") and total_s > 0:
            try:
                done = int(line.split("=")[1]) / 1e6
                set_progress(phase, done / total_s, "%s  %.0fs/%.0fs" % (detail, done, total_s))
            except ValueError:
                pass
    p.wait()
    err = p.stderr.read() if p.stderr else ""
    return p.returncode, err


def run_export(burn_captions):
    log = STATE["export"]["log"]

    def say(m):
        log.append(m)
        print("[export]", m, flush=True)

    try:
        STATE["export"]["running"] = True
        log.clear()
        cues = STATE["cues"]["cues"]
        voices = selected_voices()
        for v in voices:
            ensure_profile(v)
        say("building audio for %d cues x %d voice(s): %s"
            % (len(cues), len(voices), ", ".join(v["title"] for v in voices)))
        # Alternate voices first: the mux falls back to the primary take when an
        # alternate clip is missing, which quietly produces two identical tracks
        # if this loop does not run.
        for v in voices[1:]:
            todo = [c for c in cues if c["text"].strip()]
            for n, c in enumerate(todo):
                set_progress("audio", n / max(1, len(todo)), "%s %d/%d" % (v["key"], n + 1, len(todo)))
                build_cue_audio(c["text"], v)
            for r in ffwd_ranges():
                if (r.get("text") or "").strip():
                    build_cue_audio(r["text"], v)
            say("  %s: %d clip(s) ready" % (v["title"], len(todo)))
        built = []
        for i, c in enumerate(cues):
            set_progress("audio", i / max(1, len(cues)), "cue %d/%d" % (i + 1, len(cues)))
            if not c["text"].strip():
                say("  [%02d] deleted — skipped" % i)
                continue
            p, d = build_cue_audio(c["text"])
            built.append((i, c, p, d))
            say("  [%02d] %.2fs" % (i, d))
        base = os.path.splitext(STATE["video"])[0]
        out = base + "-narrated.mp4"

        # --- fast-forward the video FIRST, then place audio at remapped times.
        # Cues that fall INSIDE a compressed range are dropped: at 30x a 6s line
        # would have 0.2s to play in, so it can only collide with what follows.
        ranges = ffwd_ranges()
        src = STATE["video"]
        if ranges:
            src = base + "-ffwd.mp4"
            say("fast-forwarding %d range(s)…" % len(ranges))
            parts, labels, prev, n = [], [], 0.0, 0
            for r in ranges:
                f, to, k = r["from"], r["to"], float(r.get("factor", 30))
                parts.append("[0:v]trim=%.3f:%.3f,setpts=PTS-STARTPTS[p%d]" % (prev, f, n))
                labels.append("[p%d]" % n); n += 1
                # One centred translucent watermark, and only one. The corner
                # badge said the same thing in the same frame; the middle of the
                # screen is where a viewer is already looking, and saying it
                # twice just meant two places for it to go stale.
                big = "FFWD x%d" % int(float(r.get("factor", 30)))
                dt = (",drawtext=text='%s':fontsize=140:fontcolor=0xFFFFFF@0.5:"
                      "shadowcolor=0x000000@0.35:shadowx=3:shadowy=3:"
                      "x=(w-tw)/2:y=(h-th)/2" % big)
                parts.append("[0:v]trim=%.3f:%.3f,setpts=(PTS-STARTPTS)/%.4f,fps=25%s[p%d]"
                             % (f, to, k, dt, n))
                labels.append("[p%d]" % n); n += 1
                prev = to
            parts.append("[0:v]trim=%.3f,setpts=PTS-STARTPTS[p%d]" % (prev, n))
            labels.append("[p%d]" % n); n += 1
            filt = ";".join(parts) + ";" + "".join(labels) + "concat=n=%d:v=1[v]" % n
            total_out = remap(probe_seconds(STATE["video"]), ranges)
            rc, err = run_ffmpeg(["ffmpeg", "-y", "-i", STATE["video"], "-filter_complex", filt,
                                  "-map", "[v]", "-c:v", "libx264", "-preset", "veryfast",
                                  "-crf", "21", "-pix_fmt", "yuv420p", "-an", src],
                                 total_out, "ffwd", "compressing")
            if rc != 0:
                say("ffwd FAILED: " + err[-600:]); return
            say("  wrote %s" % os.path.basename(src))
            dropped = [i for i, c, _p, _d in built if inside_ffwd(c["at"], ranges)]
            if dropped:
                say("  dropped %d cue(s) inside the compressed range: %s" % (len(dropped), dropped))
            built = [(i, c, p, d) for (i, c, p, d) in built if not inside_ffwd(c["at"], ranges)]

        # Narration that belongs to a COMPRESSED range: an ordinary cue inside
        # the range gets dropped (0.2s to speak in), so the line is attached to
        # the range and placed on the OUTPUT timeline, where the compressed run
        # is long enough to hold it.
        for r in ranges:
            t = (r.get("text") or "").strip()
            if not t:
                continue
            p, d = build_cue_audio(t)
            if not p:
                continue
            room = (r["to"] - r["from"]) / float(r.get("factor", 30))
            if d > room:
                say("  ffwd narration is %.1fs but the compressed run is only %.1fs" % (d, room))
            built.append((-1, {"at": remap(r["from"], ranges), "text": t, "_out": True}, p, d))
            say("  ffwd narration %.1fs at %.1fs" % (d, remap(r["from"], ranges)))

        # Overruns must be SAID. The standalone exporter checked this and the
        # app did not, so a line running past its slot was muxed on top of the
        # next one with nothing to show for it.
        overruns = []
        for n, (i, c, p, d) in enumerate(built):
            if c.get("_out"):
                continue
            nxt = next((b[1]["at"] for b in built[n + 1:] if not b[1].get("_out")), None)
            if nxt is None:
                continue
            slot = nxt - c["at"]
            if d > slot:
                overruns.append((i, d - slot))
        if overruns:
            say("WARNING: %d cue(s) overrun their slot and will overlap the next line:" % len(overruns))
            for i, by in overruns:
                say("  cue %d by %.2fs" % (i, by))

        say("muxing %d narration track(s)…" % len(voices))
        target = os.environ.get("LOUDNESS", "-16")
        args = ["ffmpeg", "-y", "-i", src]
        chains, outs, idx, missing_alt = [], [], 1, []
        for vn, v in enumerate(voices):
            labels = []
            for n, (i, c, p, d) in enumerate(built):
                # Same cue list at the same times, spoken by each voice. If an
                # alternate is missing a clip, fall back to the primary take so
                # the track has no silent holes.
                vp = cue_audio_path(c["text"], v)
                if not os.path.exists(vp):
                    if vn > 0:
                        missing_alt.append(i)
                    vp = p
                args += ["-i", vp]
                ms = int(round((c["at"] if c.get("_out") else remap(c["at"], ranges)) * 1000))
                chains.append("[%d:a]adelay=%d|%d[v%dc%d]" % (idx, ms, ms, vn, n))
                labels.append("[v%dc%d]" % (vn, n))
                idx += 1
            chains.append("".join(labels) +
                          ("amix=inputs=%d:normalize=0:dropout_transition=0,"
                           "loudnorm=I=%s:TP=-1.5:LRA=11[mix%d]" % (len(built), target, vn)))
            outs.append("[mix%d]" % vn)
        args += ["-filter_complex", ";".join(chains), "-map", "0:v"]
        for o in outs:
            args += ["-map", o]
        args += ["-c:v", "copy", "-c:a", "aac", "-b:a", "192k"]
        for vn, v in enumerate(voices):
            # MP4 has no per-stream `title` — it names tracks via handler_name,
            # which is why titles set only as `title` came back empty. Set both:
            # handler_name for MP4/MOV, title for anything else.
            args += ["-metadata:s:a:%d" % vn, "title=%s" % v["title"],
                     "-metadata:s:a:%d" % vn, "handler_name=%s" % v["title"]]
        if missing_alt:
            say("WARNING: %d cue(s) had no alternate-voice clip and fell back to "
                "the primary voice: %s" % (len(missing_alt), sorted(set(missing_alt))[:8]))
        args += ["-disposition:a:0", "default", "-shortest", out]
        rc, err = run_ffmpeg(args, probe_seconds(src), "mux", "muxing")
        if rc != 0:
            say("ffmpeg mux FAILED: " + err[-600:]); return
        say("wrote %s" % os.path.basename(out))

        if burn_captions:
            say("burning captions (re-encodes video, this is the slow part)…")
            ass = base + ".ass"
            cues_for_ass = STATE["cues_path"]
            if ranges:
                # A temp cue file on the OUTPUT timeline, so captions land with
                # the audio rather than where they were recorded.
                tmp = base + "-remapped-cues.json"
                d2 = json.loads(json.dumps(STATE["cues"]))
                d2["cues"] = [dict(c, at=remap(c["at"], ranges))
                              for c in d2["cues"] if not inside_ffwd(c["at"], ranges)]
                for r in ranges:
                    t = (r.get("text") or "").strip()
                    if not t:
                        continue
                    room = (r["to"] - r["from"]) / float(r.get("factor", 30))
                    d2["cues"].append({"at": remap(r["from"], ranges), "text": t,
                                       "holdMs": int(room * 1000)})
                d2["cues"].sort(key=lambda c: c["at"])
                json.dump(d2, open(tmp, "w"), indent=2)
                cues_for_ass = tmp
            env = dict(os.environ, CUES=cues_for_ass, OUT=ass)
            r = subprocess.run([sys.executable, os.path.join(HERE, "make_ass.py")],
                               capture_output=True, text=True, env=env)
            say(r.stdout.strip() or r.stderr.strip()[:200])
            final = base + "-narrated-captioned.mp4"
            vf = ("drawbox=x=0:y=ih-%d:w=iw:h=%d:color=0x0a1220@1:t=fill,ass=%s"
                  % (STRIP, STRIP, ass.replace(":", "\\:")))
            # -map is REQUIRED here: without it ffmpeg's default stream
            # selection keeps a single audio stream and silently drops every
            # alternate voice track the mux just built.
            rc, err = run_ffmpeg(["ffmpeg", "-y", "-i", out, "-vf", vf,
                                  "-map", "0:v", "-map", "0:a",
                                  "-c:v", "libx264", "-preset", "veryfast", "-crf", "20",
                                  "-pix_fmt", "yuv420p", "-c:a", "copy",
                                  "-map_metadata", "0", "-movflags", "+faststart", final],
                                 probe_seconds(out), "captions", "burning captions")
            if rc != 0:
                say("caption burn FAILED: " + err[-600:]); return
            say("wrote %s" % os.path.basename(final))
        set_progress("done", 1.0, "")
        STATE["export"]["pct"] = 100.0
        say("DONE")
    finally:
        STATE["export"]["running"] = False


STRIP = 130


def ffwd_ranges():
    """Sorted, non-overlapping fast-forward ranges from cues.json."""
    r = sorted(STATE["cues"].get("ffwd", []) or [], key=lambda x: x["from"])
    out = []
    for x in r:
        if out and x["from"] < out[-1]["to"]:
            continue                      # ignore overlaps rather than mangle the map
        out.append(x)
    return out


def remap(t, ranges=None):
    """Original timestamp -> timestamp in the fast-forwarded output.

    Audio is placed per cue by adelay and captions by their own timestamps, so
    once the video is compressed BOTH need this. Doing the video edit first and
    remapping is arithmetic; doing it the other way round means re-rendering.
    """
    ranges = ffwd_ranges() if ranges is None else ranges
    out = t
    for r in ranges:
        f, to, k = r["from"], r["to"], float(r.get("factor", 30))
        if t >= to:
            out -= (to - f) - (to - f) / k        # whole range collapsed
        elif t > f:
            out -= (t - f) - (t - f) / k          # partway into the range
    return out


def inside_ffwd(t, ranges=None):
    return any(r["from"] < t < r["to"] for r in (ffwd_ranges() if ranges is None else ranges))


def suggest_ffwd():
    """The longest stretch with no narration that is not already compressed.

    Skipping existing ranges is what lets Suggest be pressed repeatedly to build
    up several fast-forwards in one video, rather than proposing the same gap
    forever."""
    have = ffwd_ranges()
    cues = STATE["cues"]["cues"]
    pts = []
    for c in cues:
        if not c["text"].strip():
            continue
        pts.append((c["at"], c["at"] + (c.get("holdMs") or 4000) / 1000.0))
    pts.sort()
    best = None
    for (s1, e1), (s2, e2) in zip(pts, pts[1:]):
        gap = s2 - e1
        if any(not (s2 <= r["from"] or e1 >= r["to"]) for r in have):
            continue                       # already inside a compressed range
        if best is None or gap > best[2]:
            best = (e1, s2, gap)
    if not best or best[2] < 30:
        return None
    return {"from": round(best[0], 1), "to": round(best[1], 1), "factor": 30,
            "label": "x30  %d minutes compressed" % round(best[2] / 60)}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _json(self, obj, code=200):
        b = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def _file(self, path, ctype=None):
        """Range-aware, so the browser can scrub a 39-minute video."""
        if not os.path.exists(path):
            self.send_error(404); return
        size = os.path.getsize(path)
        ctype = ctype or mimetypes.guess_type(path)[0] or "application/octet-stream"
        rng = self.headers.get("Range")
        start, end = 0, size - 1
        code = 200
        if rng:
            m = re.match(r"bytes=(\d*)-(\d*)", rng)
            if m:
                if m.group(1):
                    start = int(m.group(1))
                if m.group(2):
                    end = int(m.group(2))
                code = 206
        length = end - start + 1
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Accept-Ranges", "bytes")
        self.send_header("Content-Length", str(length))
        if code == 206:
            self.send_header("Content-Range", "bytes %d-%d/%d" % (start, end, size))
        self.end_headers()
        with open(path, "rb") as f:
            f.seek(start)
            remaining = length
            while remaining > 0:
                chunk = f.read(min(262144, remaining))
                if not chunk:
                    break
                try:
                    self.wfile.write(chunk)
                except (BrokenPipeError, ConnectionResetError):
                    return
                remaining -= len(chunk)

    def do_GET(self):
        p = self.path.split("?")[0]
        if p == "/":
            return self._file(os.path.join(HERE, "narrator.html"), "text/html")
        if p == "/video":
            return self._file(STATE["video"])
        if p == "/api/project":
            cues = STATE["cues"]["cues"]
            base = os.path.splitext(STATE["video"])[0]
            return self._json({
                "video": os.path.basename(STATE["video"]),
                "videoPath": STATE["video"],
                "cuesPath": STATE["cues_path"],
                "outputs": {
                    "ffwd": base + "-ffwd.mp4",
                    "narrated": base + "-narrated.mp4",
                    "captioned": base + "-narrated-captioned.mp4",
                },
                "strip": STRIP,
                "gapMs": ns.GAP_MS, "headGapMs": ns.HEAD_GAP_MS,
                "segments": STATE["cues"].get("segments", []),
                "ffwd": ffwd_ranges(), "ffwdSuggest": suggest_ffwd(),
                "voices": [dict(v, selected=(v["key"] in [x["key"] for x in selected_voices()]))
                           for v in VOICE_CATALOG],
                "exporting": STATE["export"]["running"],
                "queue": list(STATE["queue"]), "generating": STATE["current"],
                "pending": {str(k): round(max(0, DEBOUNCE_S - (time.time() - v)))
                            for k, v in STATE["pending"].items()},
                "cues": [cue_status(i, c) for i, c in enumerate(cues)],
            })
        m = re.match(r"^/api/cues/(\d+)/audio$", p)
        if m:
            i = int(m.group(1))
            c = STATE["cues"]["cues"][i]
            path = cue_audio_path(c["text"])
            if not os.path.exists(path):
                self.send_error(404, "not generated"); return
            return self._file(path, "audio/wav")
        m = re.match(r"^/api/voices/([a-z0-9_]+)/sample$", p)
        if m:
            v = next((x for x in VOICE_CATALOG if x["key"] == m.group(1)), None)
            if not v:
                self.send_error(404); return
            try:
                path = build_sample(v)
            except Exception as e:
                return self._json({"error": str(e)}, 500)
            return self._file(path, "audio/wav")
        if p == "/api/ffwd/audio":
            from urllib.parse import parse_qs, urlparse
            t = (parse_qs(urlparse(self.path).query).get("t") or [""])[0]
            path = cue_audio_path(t)
            if not t.strip() or not os.path.exists(path):
                self.send_error(404); return
            return self._file(path, "audio/wav")
        if p == "/api/export/status":
            e = STATE["export"]
            return self._json({"running": e["running"], "log": e["log"][-40:],
                               "phase": e.get("phase", ""), "pct": e.get("pct", 0),
                               "detail": e.get("detail", "")})
        self.send_error(404)

    def do_PUT(self):
        m = re.match(r"^/api/cues/(\d+)$", self.path)
        if not m:
            return self.send_error(404)
        n = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(n) or b"{}")
        i = int(m.group(1))
        with LOCK:
            c = STATE["cues"]["cues"][i]
            if "text" in body:
                c["text"] = body.get("text", "")
                c.pop("spoken", None)
            # Moving a cue is often the better fix for an overrun: the neighbour
            # usually has slack, and shifting it preserves the wording.
            if "at" in body:
                c["at"] = float(body["at"])
            save_cues()
        return self._json(cue_status(i, STATE["cues"]["cues"][i]))

    def do_DELETE(self):
        """Clear a cue: no caption, no voice, and drop its cached audio so the
        clip cannot be resurrected by a later edit that happens to hash back."""
        m = re.match(r"^/api/cues/(\d+)$", self.path)
        if not m:
            return self.send_error(404)
        i = int(m.group(1))
        with LOCK:
            c = STATE["cues"]["cues"][i]
            old = c.get("text", "")
            if old.strip():
                p = cue_audio_path(old)
                if os.path.exists(p):
                    os.remove(p)
            c["text"] = ""
            c.pop("spoken", None)
            save_cues()
        if STATE["cues"]["cues"][i]["text"].strip():
            touch(i)            # starts/restarts the quiet timer, does not generate
        return self._json(cue_status(i, STATE["cues"]["cues"][i]))

    def do_POST(self):
        m = re.match(r"^/api/cues/(\d+)/voice$", self.path)
        if m:
            i = int(m.group(1))
            c = STATE["cues"]["cues"][i]
            try:
                _, d = build_cue_audio(c["text"])
            except Exception as e:
                return self._json({"error": str(e)}, 500)
            return self._json(cue_status(i, c))
        if self.path == "/api/ffwd/voice":
            n = int(self.headers.get("Content-Length", 0))
            t = (json.loads(self.rfile.read(n) or b"{}").get("text") or "").strip()
            if not t:
                return self._json({"error": "empty"}, 400)
            _p, d = build_cue_audio(t)
            return self._json({"dur": d})
        if self.path == "/api/voices":
            n = int(self.headers.get("Content-Length", 0))
            keys = json.loads(self.rfile.read(n) or b"{}").get("keys") or [PRIMARY_KEY]
            with LOCK:
                STATE["cues"]["voices"] = [k for k in keys
                                           if any(v["key"] == k for v in VOICE_CATALOG)]
                save_cues()
            return self._json({"voices": [v["key"] for v in selected_voices()]})
        if self.path == "/api/ffwd":
            n = int(self.headers.get("Content-Length", 0))
            body = json.loads(self.rfile.read(n) or b"{}")
            with LOCK:
                STATE["cues"]["ffwd"] = body.get("ranges", [])
                save_cues()
            return self._json({"ffwd": ffwd_ranges()})
        if self.path == "/api/generate-stale":
            cues = STATE["cues"]["cues"]
            n = 0
            for i, c in enumerate(cues):
                if not c["text"].strip():
                    continue
                p = cue_audio_path(c["text"])
                if not dur_cached(p) > 0.2:
                    with LOCK:
                        STATE["pending"].pop(i, None)   # button means now
                    enqueue(i); n += 1
            return self._json({"queued": n})
        if self.path == "/api/export":
            n = int(self.headers.get("Content-Length", 0))
            body = json.loads(self.rfile.read(n) or b"{}")
            if STATE["export"]["running"]:
                return self._json({"error": "already running"}, 409)
            threading.Thread(target=run_export, args=(bool(body.get("captions", True)),),
                             daemon=True).start()
            return self._json({"started": True})
        self.send_error(404)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--video", required=True)
    ap.add_argument("--cues", required=True)
    ap.add_argument("--port", type=int, default=8777)
    ap.add_argument("--strip", type=int, default=130, help="caption strip height in px")
    a = ap.parse_args()
    global STRIP
    STRIP = a.strip
    STATE["video"] = os.path.abspath(a.video)
    STATE["cues_path"] = os.path.abspath(a.cues)
    load_cues()
    threading.Thread(target=presample_all, daemon=True).start()
    print("narration editor: http://localhost:%d" % a.port)
    print("  video: %s" % STATE["video"])
    print("  cues : %s  (%d)" % (STATE["cues_path"], len(STATE["cues"]["cues"])))
    ThreadingHTTPServer(("127.0.0.1", a.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
