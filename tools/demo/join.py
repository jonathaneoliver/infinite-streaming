#!/usr/bin/env python3
"""Join the demo halves with a fade to black between them.

    python3 join.py partA.mp4 partB.mp4 out.mp4 [--fade 1.0] [--hold 0.5]

A hard cut between the local and cloud halves reads as a glitch — same UI, same
clip, but suddenly a different target. Fading out, holding black briefly and
fading back up marks it as a deliberate section change.

Handles MULTIPLE audio tracks: each voice track is faded and concatenated
independently, because a stream copy would drop all but the first and a naive
concat would mis-order them. Both inputs must carry the same number of audio
tracks in the same order — which is why the two projects share a voice list.
"""
import argparse, json, subprocess, sys

# Fallback only. The black hold is a generated source, so its size has to match
# the video it is concatenated with — ffmpeg refuses the concat outright when it
# does not. This was hardcoded to the size of the first recording ever made,
# which meant the tool worked on exactly one window geometry and failed with a
# filter error that named neither the cause nor this constant.
DEFAULT_SIZE = "1680x1080"


def probe(path):
    out = subprocess.run(
        ["ffprobe", "-v", "error", "-show_entries", "format=duration",
         "-show_entries", "stream=index,codec_type,width,height", "-of", "json", path],
        capture_output=True, text=True).stdout
    d = json.loads(out)
    dur = float(d["format"]["duration"])
    na = sum(1 for s in d["streams"] if s["codec_type"] == "audio")
    size = None
    for s in d["streams"]:
        if s["codec_type"] == "video" and s.get("width") and s.get("height"):
            size = "%dx%d" % (s["width"], s["height"])
            break
    return dur, na, size


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("a"); ap.add_argument("b"); ap.add_argument("out")
    ap.add_argument("--fade", type=float, default=1.0, help="fade seconds each side")
    ap.add_argument("--hold", type=float, default=0.5, help="seconds of black between")
    args = ap.parse_args()

    da, na, sa = probe(args.a)
    db, nb, sb = probe(args.b)
    print("  A: %.1fs, %d audio track(s), %s" % (da, na, sa or "?"))
    print("  B: %.1fs, %d audio track(s), %s" % (db, nb, sb or "?"))
    if na != nb:
        print("  REFUSING: track counts differ (%d vs %d) — the join would drop or "
              "mis-order voices. Re-export the shorter one with the same voices." % (na, nb))
        return 2
    n = na

    # Say it rather than letting the concat fail on a filter error. Differing
    # geometry means one half was recorded at a different window size, which is
    # a re-record, not something this tool should paper over by scaling.
    if sa and sb and sa != sb:
        print("  REFUSING: video sizes differ (%s vs %s) — concat cannot join them. "
              "Re-record the odd one at the same window size." % (sa, sb))
        return 2
    size = sa or sb or DEFAULT_SIZE

    fade, hold = args.fade, args.hold
    parts = [
        "[0:v]fade=t=out:st=%.3f:d=%.3f,setpts=PTS-STARTPTS[v0]" % (max(0, da - fade), fade),
        "[1:v]fade=t=in:st=0:d=%.3f,setpts=PTS-STARTPTS[v1]" % fade,
    ]
    vlabels = "[v0][v1]"
    vcount = 2
    if hold > 0:
        # A real black segment, not just a crossfade: the pause is what reads as
        # "new section" rather than "dropped frames".
        parts.append("color=c=black:s=%s:r=25:d=%.3f[blk]" % (size, hold))
        vlabels = "[v0][blk][v1]"
        vcount = 3
    parts.append("%sconcat=n=%d:v=1:a=0[v]" % (vlabels, vcount))

    amaps = []
    for i in range(n):
        parts.append("[0:a:%d]afade=t=out:st=%.3f:d=%.3f[a0_%d]" % (i, max(0, da - fade), fade, i))
        parts.append("[1:a:%d]afade=t=in:st=0:d=%.3f[a1_%d]" % (i, fade, i))
        if hold > 0:
            parts.append("anullsrc=r=48000:cl=stereo:d=%.3f[sil%d]" % (hold, i))
            parts.append("[a0_%d][sil%d][a1_%d]concat=n=3:v=0:a=1[at%d]" % (i, i, i, i))
        else:
            parts.append("[a0_%d][a1_%d]concat=n=2:v=0:a=1[at%d]" % (i, i, i))
        amaps.append("[at%d]" % i)

    cmd = ["ffmpeg", "-y", "-i", args.a, "-i", args.b,
           "-filter_complex", ";".join(parts), "-map", "[v]"]
    for m in amaps:
        cmd += ["-map", m]
    cmd += ["-c:v", "libx264", "-preset", "veryfast", "-crf", "21", "-pix_fmt", "yuv420p",
            "-c:a", "aac", "-b:a", "192k", "-movflags", "+faststart", args.out]
    print("  joining with %.1fs fades and %.1fs of black…" % (fade, hold))
    r = subprocess.run(cmd, capture_output=True, text=True)
    if r.returncode != 0:
        print(r.stderr[-1500:]); return 1
    d, a, _ = probe(args.out)
    print("  wrote %s — %.1f min, %d audio track(s)" % (args.out, d / 60, a))
    return 0


if __name__ == "__main__":
    sys.exit(main())
