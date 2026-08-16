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

PRESETS = ("web-full", "phone-full", "side-by-side")


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


def place(preset, web, phone):
    """Resolve a preset to [(input_index, x, y, w, h), ...] on the stage.

    web/phone are (w, h) source sizes; phone may be None when there is no phone
    recording, in which case every preset degrades to web-full rather than
    rendering a black frame."""
    if phone is None or preset == "web-full":
        w, h = fit(web[0], web[1], W, STAGE_H)
        return [(0, (W - w) // 2, (STAGE_H - h) // 2, w, h)]

    if preset == "phone-full":
        w, h = fit(phone[0], phone[1], W, STAGE_H)
        return [(1, (W - w) // 2, (STAGE_H - h) // 2, w, h)]

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
        ww, wh = fit(web[0], web[1], left, STAGE_H)
        pw, ph = fit(phone[0], phone[1], right, STAGE_H)
        return [
            (0, (left - ww) // 2, (STAGE_H - wh) // 2, ww, wh),
            (1, left + (right - pw) // 2, (STAGE_H - ph) // 2, pw, ph),
        ]

    raise SystemExit("unknown layout preset %r (known: %s)" % (preset, ", ".join(PRESETS)))


def render_span(web_path, phone_path, preset, start, dur, phone_offset, dest):
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
        parts.append("[%d:v]scale=%d:%d,setsar=1[s%d]" % (idx, w, h, n))
        parts.append("[%s][s%d]overlay=x=%d:y=%d:shortest=0[o%d]" % (prev, n, x, y, n))
        prev = "o%d" % n

    cmd += ["-filter_complex", ";".join(parts), "-map", "[%s]" % prev,
            "-an", "-c:v", "libx264", "-crf", CRF, "-preset", PRESET,
            "-pix_fmt", "yuv420p", "-r", "30", dest]
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
            bounds.append((a, b, m["preset"]))

    print("compositing %d spans, %dx%d (stage %d + strip %d)"
          % (len(bounds), W, H, STAGE_H, STRIP))

    tmp = tempfile.mkdtemp(prefix="layout-")
    spans, durs = [], []
    try:
        for i, (a, b, preset) in enumerate(bounds):
            dur = b - a
            dest = os.path.join(tmp, "span%03d.mp4" % i)
            print("  [%2d] %-13s %7.2f → %7.2f  (%5.2fs)" % (i, preset, a, b, dur))
            render_span(web_path, phone_path, preset, a, dur, phone_offset, dest)
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
    tb = [(a, b) for (a, b, _p) in bounds]
    data = dict(data)
    data["video"] = out
    data["sourceVideo"] = web_path
    data["cues"] = [dict(c, at=round(remap(c["at"], tb), 3)) for c in data.get("cues", [])]
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
