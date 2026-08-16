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
import json, os, sys

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


def main():
    data = json.load(open(CUES))
    cues = data["cues"]
    head = f"""[Script Info]
ScriptType: v4.00+
PlayResX: {W}
PlayResY: {H}
WrapStyle: 2
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
        lines.append("Dialogue: 0,%s,%s,Cap,,0,0,0,,%s" % (ts(start), ts(end), esc(c["text"])))
    open(OUT, "w").write(head + "\n".join(lines) + "\n")
    print("wrote %s — %d events, %dx%d, size %d, marginV %d" % (OUT, len(lines), W, H, SIZE, MARGV))
    return 0


if __name__ == "__main__":
    sys.exit(main())
