#!/usr/bin/env python3
"""Composite the two recordings into one video, following the `layout` track.

    python3 render_layout.py [cues.json] [-o composite.mp4]

The recorder produces a browser webm and a phone mov that were started
independently. This joins them on a single canvas whose arrangement CHANGES over
time — full-frame browser while controls are being driven, both side by side
while the cap walks down, full-frame phone at the trough.

Why span-render + xfade rather than one big filtergraph
-------------------------------------------------------
`scale` parameters cannot be animated, so every size change needs its own
filter branch regardless; a ten-span graph with `enable='between(t,a,b)'` gates
becomes unreadable and re-renders everything when one cut moves. Rendering each
span separately and chaining them with xfade keeps each graph trivial, lets a
single span be re-rendered on its own, and gives real crossfades between
layouts instead of hard cuts.

Cue times are remapped
----------------------
Each crossfade overlaps two spans, so the output is shorter than the sum of its
parts and everything after a transition moves earlier. A time in span i maps to
`t - TRANSITION*i`. Cues and segments are rewritten accordingly and a new
cues.json is written next to the composite — the timings are the whole trick,
so they cannot be left pointing at the un-composited timeline.

The caption strip is reserved here, not in the page
---------------------------------------------------
The browser is a sub-rectangle of the final frame, so a caption burned into it
would shrink and move with the layout. The canvas is video area + STRIP, the
strip is left black, and make_ass.py burns into it at export.
"""
import argparse, json, os, shutil, subprocess, sys, tempfile

# Final canvas. The strip is the bottom band make_ass.py writes into; the video
# area is what the two sources are arranged inside.
W = int(os.environ.get("W", "1920"))
H = int(os.environ.get("H", "1080"))
STRIP = int(os.environ.get("STRIP", "130"))
STAGE_H = H - STRIP

TRANSITION = float(os.environ.get("TRANSITION", "0.4"))
CRF = os.environ.get("CRF", "18")
PRESET = os.environ.get("PRESET", "medium")

# Span encoding. These files are intermediates — the crossfade chain re-encodes
# every frame of them — so they want SPEED at a bitrate high enough not to show,
# not the careful rate control the final pass deserves.
# SPAN_HW=0 falls back to libx264 if VideoToolbox is unavailable or suspect.
SPAN_HW = os.environ.get("SPAN_HW", "1") == "1"
SPAN_BITRATE = os.environ.get("SPAN_BITRATE", "60M")


def span_codec_args():
    if SPAN_HW:
        return ["-c:v", "h264_videotoolbox", "-b:v", SPAN_BITRATE,
                "-pix_fmt", "yuv420p", "-r", "30"]
    return ["-c:v", "libx264", "-crf", CRF, "-preset", "veryfast",
            "-pix_fmt", "yuv420p", "-r", "30"]

PRESETS = ("web-full", "web-pip", "phone-full", "side-by-side")

# PiP size as a fraction of stage width, and its inset from the edges.
PIP_FRAC = float(os.environ.get("PIP_FRAC", "0.24"))
PIP_PAD = int(os.environ.get("PIP_PAD", "40"))


def run(cmd, what):
    p = subprocess.run(cmd, capture_output=True, text=True)
    if p.returncode != 0:
        sys.stderr.write("\n%s FAILED\n%s\n" % (what, p.stderr[-2500:]))
        raise SystemExit(1)
    return p


def probe(path):
    """(width, height, duration) for a media file."""
    out = run(["ffprobe", "-v", "error", "-select_streams", "v:0",
               "-show_entries", "stream=width,height:format=duration",
               "-of", "json", path], "ffprobe %s" % path).stdout
    d = json.loads(out)
    st = d["streams"][0]
    return int(st["width"]), int(st["height"]), float(d["format"]["duration"])


def fit(sw, sh, bw, bh):
    """Largest (w,h) with the source aspect that fits in the box. Even numbers —
    libx264 rejects odd dimensions on yuv420p."""
    scale = min(bw / sw, bh / sh)
    w = int(sw * scale) // 2 * 2
    h = int(sh * scale) // 2 * 2
    return w, h


def tall_capture(web):
    """Is the browser source a full-height page capture rather than a viewport?

    It changes how the box is computed. A viewport-shaped source is LETTERBOXED
    into its box (fit); a full-page source is CROPPED to the box's aspect and
    panned, so it fills the box exactly. Using fit() on a 3200x6800 page would
    hand the dashboard a narrow column and waste most of the frame."""
    return web[1] > web[0] * 1.6


def place(preset, web, phone):
    """Resolve a preset to [(input_index, x, y, w, h), ...] on the stage.

    web/phone are (w, h) source sizes; phone may be None when there is no phone
    recording, in which case every preset degrades to web-full rather than
    rendering a black frame."""
    if phone is None or preset == "web-full":
        if tall_capture(web):
            return [(0, 0, 0, W, STAGE_H)]          # cropped+panned, fills the stage
        w, h = fit(web[0], web[1], W, STAGE_H)
        return [(0, (W - w) // 2, (STAGE_H - h) // 2, w, h)]

    if preset == "web-pip":
        # The browser is framed exactly as in web-full; the phone sits over it.
        # Deliberately NOT shrinking the browser to make room — the inset covers
        # a corner of a chart at worst, and rescaling the dashboard between
        # web-full and web-pip would make the cut jump, which is the very thing
        # the PiP exists to avoid.
        if tall_capture(web):
            base = [(0, 0, 0, W, STAGE_H)]
        else:
            w, h = fit(web[0], web[1], W, STAGE_H)
            base = [(0, (W - w) // 2, (STAGE_H - h) // 2, w, h)]
        pw, ph = fit(phone[0], phone[1], int(W * PIP_FRAC), int(STAGE_H * PIP_FRAC))
        return base + [(1, W - pw - PIP_PAD, PIP_PAD, pw, ph)]

    if preset == "phone-full":
        # Phone full-frame with the DASHBOARD inset top-right — the mirror of
        # web-pip, and for the same reason: neither source should ever leave the
        # frame. This preset runs at the trough, where the phone's picture
        # degrading is the point; keeping the chart in shot lets the viewer see
        # the cap sitting at its floor in the same moment, which is the pairing
        # the whole demo is about.
        w, h = fit(phone[0], phone[1], W, STAGE_H)
        base = [(1, (W - w) // 2, (STAGE_H - h) // 2, w, h)]
        if tall_capture(web):
            # A full-height page inset would be an unreadable ribbon, so the PiP
            # takes a window of it, cropped and panned like any other browser
            # box. pan_y decides which part — at the trough, the chart.
            pw = int(W * PIP_FRAC) // 2 * 2
            ph = int(pw * 0.55) // 2 * 2
        else:
            pw, ph = fit(web[0], web[1], int(W * PIP_FRAC), int(STAGE_H * PIP_FRAC))
        return base + [(0, W - pw - PIP_PAD, PIP_PAD, pw, ph)]

    if preset == "side-by-side":
        # Two columns, with the split derived from the phone's own aspect rather
        # than fixed — the operator picks the orientation at record time and the
        # renderer finds out afterwards by probing.
        #
        # Give the phone exactly the width it needs to fill the stage height,
        # capped at half the canvas. Portrait (~0.46) asks for ~440px and leaves
        # the browser a wide 1480px column; landscape (~2.17) asks for more than
        # half, takes the cap, and the two split evenly. Stacking the landscape
        # case instead was tried and is measurably worse — both sources end up
        # height-limited and lose more area than the even split costs them.
        want = int(STAGE_H * phone[0] / phone[1])
        right = min(want, W // 2) // 2 * 2
        left = W - right
        pw, ph = fit(phone[0], phone[1], right, STAGE_H)
        if tall_capture(web):
            ww, wh, wx, wy = left, STAGE_H, 0, 0
        else:
            ww, wh = fit(web[0], web[1], left, STAGE_H)
            wx, wy = (left - ww) // 2, (STAGE_H - wh) // 2
        return [
            (0, wx, wy, ww, wh),
            (1, left + (right - pw) // 2, (STAGE_H - ph) // 2, pw, ph),
        ]

    raise SystemExit("unknown layout preset %r (known: %s)" % (preset, ", ".join(PRESETS)))


PAN_SECS = float(os.environ.get("PAN_SECS", "1.2"))


def pan_expr(keys, src_h, win_h, default):
    """A crop-y expression from [(t_local, y_centre), ...] keyframes.

    y values are CENTRES; crop wants the window top, so each is shifted by half
    the window and clamped to the frame. Clamping per-keyframe rather than
    around the whole expression keeps the arithmetic inside ffmpeg simple and
    the result identical, since the ramp between two clamped values is itself
    within range."""
    lo, hi = 0, max(0, src_h - win_h)
    top = lambda c: min(max(int(round(c - win_h / 2.0)), lo), hi)

    if not keys:
        return str(top(default if default is not None else src_h / 2.0))
    ys = [top(y) for _t, y in keys]
    if len(keys) == 1 or len(set(ys)) == 1:
        return str(ys[0])

    terms = [str(ys[0])]
    for i in range(1, len(keys)):
        d = ys[i] - ys[i - 1]
        if d == 0:
            continue
        terms.append("%+d*clip((t-%.3f)/%.3f,0,1)" % (d, keys[i][0], PAN_SECS))
    return "".join(terms)


def render_span(web_path, phone_path, preset, start, dur, phone_offset, dest,
                pan_y=None, pan_keys=None):
    """One span, one fixed layout, rendered to its own file."""
    web = probe(web_path)[:2]
    phone = probe(phone_path)[:2] if phone_path else None
    boxes = place(preset, web, phone)

    cmd = ["ffmpeg", "-y", "-ss", "%.3f" % start, "-t", "%.3f" % dur, "-i", web_path]
    if phone_path:
        # The phone recording started at its own moment; phone_offset is how far
        # into it the browser's t=0 falls.
        cmd += ["-ss", "%.3f" % (start + phone_offset), "-t", "%.3f" % dur, "-i", phone_path]

    parts = ["color=c=black:s=%dx%d:d=%.3f,format=yuv420p[bg]" % (W, H, dur)]
    prev = "bg"
    for n, (idx, x, y, w, h) in enumerate(boxes):
        src_w, src_h = (web if idx == 0 else phone)
        chain = ""
        # A source much taller than its destination box is a full-height
        # capture. Crop a full-width window whose aspect matches the box, at
        # the requested vertical offset, instead of fitting the whole page in
        # and rendering it unreadable.
        if idx == 0 and tall_capture((src_w, src_h)):
            win_h = int(round(src_w * h / float(w)))
            win_h = min(win_h, src_h) // 2 * 2
            # pan values are CENTRES of interest, not window tops: the window
            # height depends on the destination box, which the recorder cannot
            # know, but it does know which element it wants in shot.
            # In phone-full the browser is a small INSET whose whole job is to
            # keep the bandwidth chart visible beside the degrading picture. Its
            # window is short (~1760px of a 6800px page), so following the
            # per-cue pan drifts it off the chart and clips the bottom. Hold the
            # span's own target — the chart centre — and ignore the cue moves.
            keys = [] if preset == "phone-full" else (pan_keys or [])
            expr = pan_expr(keys, src_h, win_h, pan_y)
            chain = "crop=%d:%d:0:'%s'," % (src_w, win_h, expr)
        parts.append("[%d:v]%sscale=%d:%d,setsar=1[s%d]" % (idx, chain, w, h, n))
        parts.append("[%s][s%d]overlay=x=%d:y=%d:shortest=0[o%d]" % (prev, n, x, y, n))
        prev = "o%d" % n

    cmd += ["-filter_complex", ";".join(parts), "-map", "[%s]" % prev,
            "-an"] + span_codec_args() + [dest]
    run(cmd, "render span %s" % preset)


def chain_xfade(spans, durs, dest):
    """Crossfade the spans together in one pass."""
    if len(spans) == 1:
        shutil.copy(spans[0], dest)
        return
    cmd = ["ffmpeg", "-y"]
    for s in spans:
        cmd += ["-i", s]
    parts, prev, acc = [], "0:v", durs[0]
    for i in range(1, len(spans)):
        off = acc - TRANSITION
        lbl = "x%d" % i
        parts.append("[%s][%d:v]xfade=transition=fade:duration=%.3f:offset=%.3f[%s]"
                     % (prev, i, TRANSITION, off, lbl))
        prev, acc = lbl, acc + durs[i] - TRANSITION
    cmd += ["-filter_complex", ";".join(parts), "-map", "[%s]" % prev,
            "-an", "-c:v", "libx264", "-crf", CRF, "-preset", PRESET,
            "-pix_fmt", "yuv420p", "-r", "30", dest]
    run(cmd, "xfade chain")


def remap(t, bounds):
    """Source time -> composite time. Span i loses TRANSITION*i to the crossfades
    that precede it."""
    for i, (a, b) in enumerate(bounds):
        if t < b or i == len(bounds) - 1:
            return max(0.0, t - TRANSITION * i)
    return t


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("cues", nargs="?",
                    default=os.path.join(os.environ.get(
                        "DEMO_DIR", os.path.expanduser("~/Desktop/smashing-demo")), "cues.json"))
    ap.add_argument("-o", "--out", default=None)
    ap.add_argument("--keep", action="store_true", help="keep the per-span files")
    args = ap.parse_args()

    data = json.load(open(args.cues))
    web_path = data["video"]
    phone_path = data.get("phone")
    phone_offset = float(data.get("phoneOffset") or 0)
    out = args.out or os.path.join(os.path.dirname(args.cues), "composite.mp4")

    if not os.path.exists(web_path):
        raise SystemExit("browser recording missing: %s" % web_path)
    if phone_path and not os.path.exists(phone_path):
        sys.stderr.write("phone recording missing (%s) — rendering browser only\n" % phone_path)
        phone_path = None

    total = probe(web_path)[2]
    if phone_path:
        _, _, pdur = probe(phone_path)
        # The phone must cover the browser's whole span once shifted, or the
        # tail spans composite against nothing.
        covered = pdur - phone_offset
        if covered < total - 1.0:
            sys.stderr.write(
                "⚠ phone recording covers %.1fs of the browser's %.1fs "
                "(offset %.2fs) — later spans will run short\n" % (covered, total, phone_offset))

    track = sorted(data.get("layout") or [], key=lambda x: x["at"])
    if not track or track[0]["at"] > 0:
        track = [{"at": 0.0, "preset": "web-full"}] + track

    # Span boundaries: each layout mark runs until the next one.
    bounds = []
    for i, m in enumerate(track):
        a = float(m["at"])
        b = float(track[i + 1]["at"]) if i + 1 < len(track) else total
        if b - a > 0.05:                      # drop marks that were superseded instantly
            # `y` is the top of the visible window in SOURCE pixels. The
            # recorder writes it from the element rects it measured, so a
            # framing decision refers to a real box on the page rather than a
            # magic number.
            bounds.append((a, b, m["preset"], m.get("y")))

    print("compositing %d spans, %dx%d (stage %d + strip %d)"
          % (len(bounds), W, H, STAGE_H, STRIP))

    tmp = tempfile.mkdtemp(prefix="layout-")
    spans, durs = [], []
    try:
        for i, (a, b, preset, pan) in enumerate(bounds):
            dur = b - a
            dest = os.path.join(tmp, "span%03d.mp4" % i)
            keyn = len([c for c in (data.get("cues") or [])
                        if c.get("pan") is not None and a <= c["at"] < b])
            print("  [%2d] %-13s %7.2f → %7.2f  (%5.2fs)%s%s"
                  % (i, preset, a, b, dur,
                     "" if pan is None else "  pan y=%d" % pan,
                     "  +%d moves" % keyn if keyn else ""))
            keys = [(max(0.0, c["at"] - a), c["pan"])
                    for c in (data.get("cues") or [])
                    if c.get("pan") is not None and a <= c["at"] < b]
            if keys and pan is not None and keys[0][0] > 0.01:
                keys.insert(0, (0.0, pan))     # hold the span's own framing until the first cue
            render_span(web_path, phone_path, preset, a, dur, phone_offset, dest,
                        pan_y=pan, pan_keys=keys)
            spans.append(dest)
            durs.append(dur)

        print("chaining with %.2fs crossfades…" % TRANSITION)
        chain_xfade(spans, durs, out)
    finally:
        if args.keep:
            print("span files kept in %s" % tmp)
        else:
            shutil.rmtree(tmp, ignore_errors=True)

    # Rewrite the timings onto the composite's timeline.
    tb = [(a, b) for (a, b, _p, _y) in bounds]
    data = dict(data)
    data["video"] = out
    data["sourceVideo"] = web_path
    # Remap, then enforce monotonic order.
    #
    # Each cue's mapping is individually correct, but two cues emitted a
    # fraction apart with a LAYOUT MARK between them land in different spans and
    # have different crossfade offsets subtracted — so the later cue can come
    # out earlier. Take 5 ended with its two wrap lines inverted that way, which
    # in the finished video means the summary is spoken before the sentence it
    # summarises.
    #
    # Clamping to the previous cue costs at most a few hundred milliseconds of
    # drift on the offending pair and keeps the script in the order it was
    # written.
    cues_out, prev = [], -1.0
    for c in data.get("cues", []):
        t = round(remap(c["at"], tb), 3)
        if t < prev:
            t = prev
        prev = t
        cues_out.append(dict(c, at=t))
    data["cues"] = cues_out
    data["segments"] = [dict(s, **{"from": round(remap(s["from"], tb), 3),
                                   "to": round(remap(s["to"], tb), 3)})
                        for s in data.get("segments", [])]
    dest_cues = os.path.join(os.path.dirname(out), "cues-composite.json")
    json.dump(data, open(dest_cues, "w"), indent=2)

    print("\nwrote %s" % out)
    print("wrote %s — %d cues remapped onto the composite timeline"
          % (dest_cues, len(data.get("cues", []))))
    print("\nNext:  CUES=%s python3 narrator_app.py" % dest_cues)
    return 0


if __name__ == "__main__":
    sys.exit(main())
