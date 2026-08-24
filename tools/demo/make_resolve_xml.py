#!/usr/bin/env python3
"""cues.json -> a DaVinci Resolve timeline (FCP7 XML).

    python3 make_resolve_xml.py [cues.json] [-o take.xml] [--fps 60]

Then in Resolve: File > Import > Timeline > Import AAF, EDL, XML…

Why FCP7 XML and not the alternatives
-------------------------------------
Resolve reads FCP7 XML (xmeml v5), FCPXML, AAF and EDL. FCP7 XML is the only
one of those that is plain text, fully specified, and carries BOTH multiple
video tracks AND timeline markers — so the narration can travel with the edit
instead of being retyped. AAF is structured-storage binary; EDL is one track
and no marker text.

What you get
------------
  V2  the browser recording   (dashboard: charts, controls)
  V1  the phone recording     (trimmed by phoneOffset so the two align)
  markers  one per cue, carrying the narration text, at its exact time
           plus the layout marks and segment boundaries the recorder wrote

The offset is the point
-----------------------
The two recordings were started independently, so they do not share a clock.
`phoneOffset` is how far into the PHONE recording the browser's t=0 falls, and
it is applied here as an in-point on the phone clip rather than a timeline
shift — that way both clips start at 00:00:00:00 and the sync survives moving
them around as a group.

It is a measured value, not an exact one. Nudge the phone clip a frame or two
if a visible reaction does not line up; everything else on the timeline is
anchored to the browser track, which is where the cue timings come from.
"""
import argparse
import json
import os
import sys
import urllib.parse
import xml.etree.ElementTree as ET


def transcode_for_nle(src, dest_dir, prores=False):
    """WebM/VP8 -> a codec an NLE will actually open.

    Playwright records VP8 in WebM, which Resolve cannot decode on macOS. The
    evidence: an XML referencing both recordings linked the phone's .mov fine
    and reported "File not found in search directories" for the .webm — same
    volume, same absolute path form. That message covers unsupported codecs as
    well as missing files, which is what makes it so misleading.

    The default path delegates to tools/demo/transcode_for_nle.sh rather than
    running its own ffmpeg. This used to be a second, independent transcoder
    here — libx264 where the shell used VideoToolbox, writing browser.mov where
    the shell wrote browser-nle.mov. Two implementations of one decision meant
    the codec-size rule (hardware H.264 dies over 4096px, so a tall capture has
    to be HEVC) could be learned in one and missing from the other. It was.

    --prores stays inline: intra-frame scrubbing means stepping frame by frame
    through a chart change never decodes a GOP. That is a real editing benefit
    and the reason to spend the disk — 78MB became 4.9GB at a quarter of these
    dimensions — but it should be a choice rather than a default.

    The output name also drops the `page@<hash>` form — one fewer oddity to
    suspect when a path fails to resolve.
    """
    import subprocess
    if not prores:
        script = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                              "transcode_for_nle.sh")
        r = subprocess.run([script, dest_dir], capture_output=False)
        dest = os.path.join(dest_dir, "browser-nle.mov")
        if r.returncode != 0 or not os.path.exists(dest):
            raise SystemExit("transcode failed")
        return dest

    dest = os.path.join(dest_dir, "browser-prores.mov")
    if os.path.exists(dest) and os.path.getsize(dest) > 0:
        print("  reusing %s" % dest)
        return dest
    print("  transcoding browser track to ProRes 422 LT…")
    r = subprocess.run(["ffmpeg", "-y", "-v", "error", "-stats", "-i", src,
                        "-c:v", "prores_ks", "-profile:v", "1",
                        "-pix_fmt", "yuv422p10le", "-an", dest],
                       capture_output=False)
    if r.returncode != 0 or not os.path.exists(dest):
        raise SystemExit("transcode failed")
    print("  %s  %.1f MB" % (dest, os.path.getsize(dest) / 1e6))
    return dest


def probe(path):
    """(width, height, duration_seconds) via ffprobe."""
    import subprocess
    out = subprocess.run(
        ["ffprobe", "-v", "error", "-select_streams", "v:0",
         "-show_entries", "stream=width,height:format=duration",
         "-of", "json", path],
        capture_output=True, text=True, check=True).stdout
    d = json.loads(out)
    st = d["streams"][0]
    return int(st["width"]), int(st["height"]), float(d["format"]["duration"])


def el(parent, tag, text=None):
    e = ET.SubElement(parent, tag)
    if text is not None:
        e.text = str(text)
    return e


def rate(parent, fps):
    r = el(parent, "rate")
    el(r, "timebase", int(round(fps)))
    # NTSC FALSE keeps the timebase integer. Resolve conforms both sources to
    # the sequence rate on import either way.
    el(r, "ntsc", "FALSE")
    return r


def timecode(parent, fps):
    """A zero start timecode. Resolve's FCP7 importer wants one on the sequence
    and on each file; without it the import can be rejected outright rather
    than defaulting to zero."""
    tc = el(parent, "timecode")
    rate(tc, fps)
    el(tc, "string", "00:00:00:00")
    el(tc, "frame", 0)
    el(tc, "displayformat", "NDF")
    return tc


def file_el(parent, fid, path, w, h, dur_f, fps, seen):
    """A <file>. Repeat references must be id-only or Resolve creates duplicate
    media pool entries for the same clip."""
    f = el(parent, "file")
    f.set("id", fid)
    if fid in seen:
        return f
    seen.add(fid)
    el(f, "name", os.path.basename(path))
    # file:// URL. `safe` must include @ — Playwright names its recordings
    # page@<hash>.webm, and quote() escapes @ to %40 by default, after which
    # Resolve hunts for a file whose name literally contains "%40" and reports
    # "File not found in search directories".
    el(f, "pathurl", "file://" + urllib.parse.quote(os.path.abspath(path), safe="/@+,=:"))
    rate(f, fps)
    el(f, "duration", dur_f)
    timecode(f, fps)
    media = el(f, "media")
    v = el(media, "video")
    el(v, "duration", dur_f)
    sc = el(v, "samplecharacteristics")
    rate(sc, fps)
    el(sc, "width", w)
    el(sc, "height", h)
    # Square pixels, progressive — stated rather than left to be inferred.
    el(sc, "anamorphic", "FALSE")
    el(sc, "pixelaspectratio", "square")
    el(sc, "fielddominance", "none")
    return f


def clipitem(track, cid, name, fid, path, w, h, fps,
             start_f, end_f, in_f, out_f, seen):
    ci = el(track, "clipitem")
    ci.set("id", cid)
    el(ci, "name", name)
    el(ci, "enabled", "TRUE")
    el(ci, "duration", out_f - in_f)
    rate(ci, fps)
    # start/end place it on the TIMELINE; in/out choose the region of the FILE.
    el(ci, "start", start_f)
    el(ci, "end", end_f)
    el(ci, "in", in_f)
    el(ci, "out", out_f)
    file_el(ci, fid, path, w, h, out_f, fps, seen)
    return ci


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("cues", nargs="?",
                    default=os.path.join(os.environ.get(
                        "DEMO_DIR", os.path.expanduser("~/Desktop/smashing-demo")), "cues.json"))
    ap.add_argument("-o", "--out", default=None)
    # 60 by default: the phone records at 60 and the browser at 25, and neither
    # divides the other. 60 keeps every phone frame; the browser track is a
    # screen recording where the uneven hold is imperceptible.
    ap.add_argument("--fps", type=float, default=60.0)
    ap.add_argument("--name", default="Valley demo")
    ap.add_argument("--no-transcode", action="store_true",
                    help="reference the .webm directly (Resolve will not decode it)")
    ap.add_argument("--prores", action="store_true",
                    help="ProRes 422 LT instead of H.264 — intra-frame scrubbing, ~30x the size")
    args = ap.parse_args()

    d = json.load(open(args.cues))
    fps = args.fps
    sec = lambda s: int(round(float(s) * fps))

    web = d["video"]
    phone = d.get("phone")
    offset = float(d.get("phoneOffset") or 0)
    out_path = args.out or os.path.join(os.path.dirname(args.cues), "take-resolve.xml")

    if not os.path.exists(web):
        raise SystemExit("browser recording missing: %s" % web)
    if not args.no_transcode and os.path.splitext(web)[1].lower() == ".webm":
        web = transcode_for_nle(web, os.path.dirname(args.cues), prores=args.prores)
    ww, wh, wdur = probe(web)
    print("browser  %dx%d  %.2fs" % (ww, wh, wdur))

    have_phone = bool(phone) and os.path.exists(phone)
    if phone and not have_phone:
        sys.stderr.write("phone recording missing (%s) — writing a browser-only timeline\n" % phone)
    if have_phone:
        pw, ph, pdur = probe(phone)
        print("phone    %dx%d  %.2fs  (offset %.3fs)" % (pw, ph, pdur, offset))

    xmeml = ET.Element("xmeml", {"version": "5"})
    seq = el(xmeml, "sequence")
    seq.set("id", "sequence-1")
    el(seq, "name", args.name)
    el(seq, "duration", sec(wdur))
    rate(seq, fps)
    media = el(seq, "media")
    video = el(media, "video")
    fmt = el(video, "format")
    sc = el(fmt, "samplecharacteristics")
    rate(sc, fps)
    # Sequence resolution follows the browser track; the phone is the taller
    # source but the dashboard is what the framing is built around.
    el(sc, "width", ww)
    el(sc, "height", wh)
    el(sc, "anamorphic", "FALSE")
    el(sc, "pixelaspectratio", "square")
    el(sc, "fielddominance", "none")
    timecode(seq, fps)

    seen = set()

    # V1 first in document order = bottom track in Resolve. Phone underneath,
    # browser above, so the dashboard is the one you key/crop against.
    t1 = el(video, "track")
    el(t1, "enabled", "TRUE")
    el(t1, "locked", "FALSE")
    if have_phone:
        in_f = sec(offset)
        avail = sec(pdur)
        out_f = avail
        clipitem(t1, "clipitem-phone", os.path.basename(phone), "file-phone",
                 phone, pw, ph, fps, 0, out_f - in_f, in_f, out_f, seen)

    t2 = el(video, "track")
    el(t2, "enabled", "TRUE")
    el(t2, "locked", "FALSE")
    clipitem(t2, "clipitem-web", os.path.basename(web), "file-web",
             web, ww, wh, fps, 0, sec(wdur), 0, sec(wdur), seen)

    # Markers. The narration is the reason to open this in an NLE at all — the
    # cue text riding at its exact frame is what makes the edit reviewable
    # without cross-referencing a JSON file.
    n = 0
    for m in d.get("layout") or []:
        mk = el(seq, "marker")
        el(mk, "name", "LAYOUT %s" % m["preset"])
        el(mk, "comment", "layout: %s" % m["preset"])
        el(mk, "in", sec(m["at"]))
        el(mk, "out", -1)
        n += 1
    for s in d.get("segments") or []:
        mk = el(seq, "marker")
        el(mk, "name", "== %s" % s["name"])
        el(mk, "comment", "segment %s" % s["name"])
        el(mk, "in", sec(s["from"]))
        el(mk, "out", -1)
        n += 1
    for i, c in enumerate(d.get("cues") or []):
        mk = el(seq, "marker")
        el(mk, "name", "%02d %s" % (i + 1, c["text"][:40]))
        el(mk, "comment", c["text"])
        el(mk, "in", sec(c["at"]))
        el(mk, "out", -1)
        n += 1

    ET.indent(xmeml, space="  ")
    xml = ET.tostring(xmeml, encoding="unicode")
    with open(out_path, "w") as f:
        f.write('<?xml version="1.0" encoding="UTF-8"?>\n<!DOCTYPE xmeml>\n')
        f.write(xml + "\n")

    print("\nwrote %s" % out_path)
    print("  %d fps · V2 browser · V1 phone · %d markers" % (int(fps), n))
    print("\nResolve:  File > Import > Timeline > Import AAF, EDL, XML…")
    return 0


if __name__ == "__main__":
    sys.exit(main())
