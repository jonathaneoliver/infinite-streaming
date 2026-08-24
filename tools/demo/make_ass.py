#!/usr/bin/env python3
"""cues.json -> ASS subtitles positioned inside the caption strip.

An SRT handed to libass is laid out against a DEFAULT script resolution
(~384x288), so a FontSize of 22 renders at ~82px on a 1080-high frame and a
MarginV of 48 lifts the text ~180px off the bottom — far above the 130px strip
the page reserved for it. Declaring PlayResX/PlayResY makes every unit below an
actual pixel, which is the only way to land text in a box of known geometry.

Styling mirrors the in-page caption: same family, size, colour, left inset, and
vertical centring within the strip.
"""
import json, os, sys, textwrap

DEMO_DIR = os.environ.get("DEMO_DIR", os.path.expanduser("~/Desktop/encoder-demo"))
CUES = os.environ.get("CUES", os.path.join(DEMO_DIR, "cues.json"))
OUT = os.environ.get("OUT", os.path.join(DEMO_DIR, "captions.ass"))
W = int(os.environ.get("W", "1680"))
H = int(os.environ.get("H", "1080"))
STRIP = int(os.environ.get("STRIP", "130"))     # the reserved strip height
FONT = os.environ.get("FONT", "Helvetica Neue")
SIZE = int(os.environ.get("SIZE", "27"))
LEFT = int(os.environ.get("LEFT", "32"))
# Alignment 1 = bottom-left; MarginV is measured from the bottom edge, so this
# centres a single line of SIZE within the strip.
MARGV = int(os.environ.get("MARGV", str(max(8, (STRIP - SIZE) // 2))))
# Line box height. 1.2em is the usual leading and matches what libass lays out
# closely enough to place a two- or three-line block inside the strip.
LINE_H = int(os.environ.get("LINE_H", str(int(SIZE * 1.2))))
# How many characters fit on a line at this size. Estimated from an average
# glyph width of 0.52em; override when the font makes that wrong.
CHARS_PER_LINE = int(os.environ.get("CHARS_PER_LINE",
                                   str(max(20, int((W - 2 * LEFT) / (SIZE * 0.52))))))
# Cap how long a single caption can linger. 0 = no cap (hold until replaced).
# The FFWD run is the case that matters: one caption over a 60s stretch.
PERSIST_MAX = float(os.environ.get("PERSIST_MAX", "0"))


def ts(sec):
    if sec < 0:
        sec = 0
    h = int(sec // 3600); m = int(sec % 3600 // 60); s = sec % 60
    return "%d:%02d:%05.2f" % (h, m, s)


def esc(t):
    return t.replace("\\", "\\\\").replace("{", "(").replace("}", ")").replace("\n", "\\N")


def wrap_caption(t):
    r"""Break a caption into lines that fit the frame, joined with ASS's \N.

    Wrapped HERE rather than left to the renderer, because the number of lines
    has to be known to place the block: each caption's MarginV is computed from
    its own line count so the block centres in the strip instead of growing up
    into the picture.

    The width is estimated from the font size — 0.52em is a fair average for
    Helvetica Neue at these sizes — so it is approximate by design. CHARS_PER_LINE
    overrides it when a different face makes the estimate wrong.

    Wraps the RAW text and escapes each line afterwards. Escaping first would
    put a literal "\\N" into the string for any embedded newline, which
    textwrap would then be free to break in half.
    """
    parts = textwrap.wrap(t, CHARS_PER_LINE) or [t]
    return "\\N".join(esc(p) for p in parts)


def main():
    data = json.load(open(CUES))
    cues = data["cues"]
    multi = 0
    tall = []
    head = f"""[Script Info]
ScriptType: v4.00+
PlayResX: {W}
PlayResY: {H}
WrapStyle: 0
ScaledBorderAndShadow: yes

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Cap,{FONT},{SIZE},&H00F7EDE6,&H00F7EDE6,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,0,0,1,{LEFT},{LEFT},{MARGV},1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
"""
    lines = []
    for i, c in enumerate(cues):
        start = c["at"]
        # A caption stays up until the NEXT one replaces it — which is what the
        # page did while recording. Ending at holdMs instead made the text
        # disappear exactly as the action it described was performed, because
        # the recorder holds the caption for its full duration BEFORE clicking.
        if i + 1 < len(cues):
            end = cues[i + 1]["at"] - 0.05
        else:
            end = start + (c.get("holdMs") or 4000) / 1000.0 + 2.0
        if PERSIST_MAX > 0:
            end = min(end, start + PERSIST_MAX)
        if end <= start or not c["text"].strip():
            continue          # a cleared cue is a DELETED cue, not a blank caption
        wrapped = wrap_caption(c["text"])
        n = wrapped.count("\\N") + 1
        if n > 1:
            multi += 1
        # Centre THIS caption's block in the strip. A style-level MarginV can
        # only centre one line; a two-line caption sitting on that baseline
        # grows upward into the picture.
        mv = max(8, int((STRIP - n * LINE_H) // 2))
        if n * LINE_H > STRIP:
            tall.append((n, c["text"][:50]))
        lines.append("Dialogue: 0,%s,%s,Cap,,0,0,%d,,%s" % (ts(start), ts(end), mv, wrapped))
    open(OUT, "w").write(head + "\n".join(lines) + "\n")
    print("wrote %s — %d events, %dx%d, size %d, marginV %d" % (OUT, len(lines), W, H, SIZE, MARGV))
    print("  wrapped to %d chars/line; %d caption(s) run to more than one line"
          % (CHARS_PER_LINE, multi))
    for n, t in tall:
        print("  ⚠ %d lines does not fit the %dpx strip: %s…" % (n, STRIP, t))
    return 0


if __name__ == "__main__":
    sys.exit(main())
