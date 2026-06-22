#!/usr/bin/env python3
"""
ladder_audit.py — audit an HLS encode's ABR ladder.

Checks a master playlist + its variants against Apple's HLS Authoring
Specification + project rules, compares the advertised BANDWIDTH /
AVERAGE-BANDWIDTH to the *measured* segment bitrates, and (by default,
subsampled) measures VMAF per variant against a mezzanine reference. Emits a
structured ladder_audit.json (the logged ladder config + every check result)
and a human-readable report; optionally appends a section to ENCODING_REPORT.md.

Two input modes:
  --dir   <encode_output_dir>   read master.m3u8 + variant playlists + segments off disk
  --url   <master playlist URL> fetch from a live/origin server (https self-signed OK)

The ladder RULES mirror go-proxy/pkg/ladder/validate.go (MinPeakSpacing,
tight_spacing / inversion / duplicate_bandwidth). Keep this in parity with that
package's golden vectors (ladder_test.go) — same discipline as the JS mirror in
content/dashboard-v3/src/components/NetworkShapingPattern.vue.

Apple HLS Authoring Spec rules encoded here:
  - adjacent video bit rates should be 1.5x-2x apart (MIN/MAX_PEAK_SPACING)
  - a variant's peak should be <= 2x its average (MAX_PEAK_OVER_AVG)
  - measured peak within 10% of BANDWIDTH; measured avg within 10% of
    AVERAGE-BANDWIDTH (ACCURACY_TOL)
"""

import argparse
import json
import os
import re
import shutil
import ssl
import subprocess
import sys
import tempfile
import time
from urllib.parse import urljoin
from urllib.request import urlopen, Request

# ---- rule thresholds (mirror pkg/ladder + Apple spec) ----------------------
MIN_PEAK_SPACING = 1.5   # pkg/ladder.MinPeakSpacing — below this, tight_spacing
MAX_PEAK_SPACING = 2.0   # Apple: adjacent tiers no more than ~2x apart
MAX_PEAK_OVER_AVG = 2.0  # Apple: peak bit rate <= 200% of average
ACCURACY_TOL = 0.10      # Apple: measured within 10% of advertised
MEASURE_SEGMENTS = 15    # last N media segments to measure
VMAF_SUBSAMPLE = 5       # default libvmaf n_subsample
# VMAF advisory thresholds (project, not Apple)
VMAF_REDUNDANT_DELTA = 1.0   # adjacent tiers within this VMAF => redundant rung
VMAF_CLIFF_DELTA = 6.0       # adjacent tiers beyond this VMAF gap => quality cliff

_SSL = ssl.create_default_context()
_SSL.check_hostname = False
_SSL.verify_mode = ssl.CERT_NONE


# ---- IO: live URL or on-disk path ------------------------------------------
def is_url(s):
    return s.startswith("http://") or s.startswith("https://")


def load_text(uri):
    if is_url(uri):
        req = Request(uri, headers={"Cache-Control": "no-cache"})
        with urlopen(req, timeout=15, context=_SSL) as r:
            return r.read().decode("utf-8", errors="replace")
    with open(uri, "r", encoding="utf-8", errors="replace") as f:
        return f.read()


def resolve(base, ref):
    """Resolve a playlist-relative URI against its base (URL or filesystem)."""
    if is_url(base):
        return urljoin(base, ref)
    if ref.startswith("/"):
        # Absolute server path in an on-disk playlist — strip to a basename join.
        ref = ref.lstrip("/")
    return os.path.normpath(os.path.join(os.path.dirname(base), ref))


def seg_bytes(base, seg_ref):
    """Size of a whole-file segment (no byterange) — disk stat or HTTP length."""
    target = resolve(base, seg_ref)
    if is_url(target):
        req = Request(target, headers={"Cache-Control": "no-cache"})
        with urlopen(req, timeout=15, context=_SSL) as r:
            return len(r.read())
    return os.path.getsize(target)


# ---- manifest parsing ------------------------------------------------------
def attr(line, key):
    # Key is preceded by ':' (first attr after the tag) or ',' (subsequent) — never
    # an alnum or '-', so the lookbehind also stops BANDWIDTH matching inside
    # AVERAGE-BANDWIDTH.
    m = re.search(r'(?<![\w-])' + re.escape(key) + r'=("([^"]*)"|[^,]*)', line)
    if not m:
        return ""
    return m.group(2) if m.group(2) is not None else m.group(1)


def parse_master(text):
    """Return (variants, audio_uri). Each variant: dict(res,bw,avg,codecs,uri)."""
    lines = text.splitlines()
    variants, audio_uri = [], None
    for i, l in enumerate(lines):
        s = l.strip()
        if s.startswith("#EXT-X-MEDIA") and "TYPE=AUDIO" in s:
            audio_uri = attr(s, "URI") or audio_uri
        if s.startswith("#EXT-X-STREAM-INF"):
            bw = attr(s, "BANDWIDTH")
            avg = attr(s, "AVERAGE-BANDWIDTH")
            uri = lines[i + 1].strip() if i + 1 < len(lines) else ""
            variants.append({
                "resolution": attr(s, "RESOLUTION") or "?",
                "bw": int(bw) if bw.isdigit() else 0,
                "avg": int(avg) if avg.isdigit() else 0,
                "codecs": attr(s, "CODECS"),
                "uri": uri,
            })
    return variants, audio_uri


def measure(base, playlist_uri, n=MEASURE_SEGMENTS):
    """Measured (peak_bps, avg_bps) over the last n media segments of a variant."""
    text = load_text(resolve(base, playlist_uri))
    pbase = resolve(base, playlist_uri)
    durs, byts, dur, brange = [], [], None, None
    for l in text.splitlines():
        s = l.strip()
        if s.startswith("#EXTINF:"):
            dur = float(s.split(":", 1)[1].split(",", 1)[0])
        elif s.startswith("#EXT-X-BYTERANGE:"):
            brange = int(s.split(":", 1)[1].split("@", 1)[0])
        elif s and not s.startswith("#"):
            if dur is not None:
                if brange is not None:          # byterange segment — size is in the playlist
                    byts.append(brange)
                else:                            # whole-file segment — stat / fetch its size
                    try:
                        byts.append(seg_bytes(pbase, s))
                    except Exception:
                        byts.append(0)
                durs.append(dur)
            dur, brange = None, None
    pairs = [(d, b) for d, b in zip(durs, byts) if d > 0 and b > 0][-n:]
    if not pairs:
        return 0.0, 0.0
    peak = max(b * 8 / d for d, b in pairs)
    avg = sum(b * 8 for _, b in pairs) / sum(d for d, _ in pairs)
    return peak, avg


# ---- VMAF (ffmpeg libvmaf) -------------------------------------------------
def measure_vmaf(base, variant_uri, ref, ref_w, ref_h, subsample, full):
    """Pooled VMAF (mean / harmonic_mean / min) of a variant vs the mezzanine."""
    ffmpeg = shutil.which("ffmpeg")
    if not ffmpeg:
        return {"error": "ffmpeg not on PATH"}
    src = resolve(base, variant_uri)
    sub = "" if full else f"n_subsample={subsample}:"
    with tempfile.NamedTemporaryFile(suffix=".json", delete=False) as tf:
        log = tf.name
    try:
        # input 0 = distorted variant, input 1 = reference mezzanine. Normalise
        # BOTH to the ref resolution + yuv420p + SAR 1 + reset PTS so libvmaf
        # never hits a dimension/format/SAR/timebase mismatch (the upscaled rungs
        # otherwise died with "-22 Invalid argument / no packets"). libvmaf wants
        # [distorted][reference].
        flt = (f"[0:v]scale={ref_w}:{ref_h}:flags=bicubic,format=yuv420p,setsar=1,setpts=PTS-STARTPTS[dist];"
               f"[1:v]scale={ref_w}:{ref_h}:flags=bicubic,format=yuv420p,setsar=1,setpts=PTS-STARTPTS[ref];"
               f"[dist][ref]libvmaf={sub}log_fmt=json:log_path={log}")
        cmd = [ffmpeg, "-nostdin", "-hide_banner", "-i", src, "-i", ref,
               "-lavfi", flt, "-f", "null", "-"]
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=3600)
        if r.returncode != 0:
            return {"error": (r.stderr or "")[-300:]}
        with open(log) as f:
            data = json.load(f)
        p = data.get("pooled_metrics", {}).get("vmaf", {}) or data.get("VMAF", {})
        return {
            "mean": round(p.get("mean", data.get("vmaf", 0)), 2),
            "harmonic_mean": round(p.get("harmonic_mean", 0), 2),
            "min": round(p.get("min", 0), 2),
        }
    except Exception as e:
        return {"error": str(e)}
    finally:
        try:
            os.unlink(log)
        except OSError:
            pass


# ---- checks ----------------------------------------------------------------
def check(results, kind, severity, detail):
    results.append({"kind": kind, "severity": severity, "detail": detail})


def run_checks(variants, audio):
    """variants: list with advertised+measured fields, sorted ascending by bw."""
    res = []
    ap, aa = audio  # advertised audio peak/avg are folded into each variant already

    # per-variant rules
    for v in variants:
        lbl = v["resolution"]
        if v["avg"] <= 0:
            check(res, "missing_average_bandwidth", "warn", f"{lbl}: no AVERAGE-BANDWIDTH")
        elif v["avg"] >= v["bw"]:
            check(res, "avg_ge_peak", "fail", f"{lbl}: avg {v['avg']} >= peak {v['bw']}")
        if v["avg"] > 0 and v["bw"] / v["avg"] > MAX_PEAK_OVER_AVG:
            check(res, "peak_over_2x_avg", "fail",
                  f"{lbl}: peak/avg {v['bw']/v['avg']:.2f}x > {MAX_PEAK_OVER_AVG}x (Apple)")
        if not re.search(r'avc1|hvc1|hev1|av01', v["codecs"]) or "mp4a" not in v["codecs"]:
            check(res, "codecs_incomplete", "warn",
                  f"{lbl}: CODECS={v['codecs']!r} should list video AND audio (mp4a)")
        if v["resolution"] == "?":
            check(res, "missing_resolution", "warn", f"bw={v['bw']}: no RESOLUTION")
        # accuracy vs measured
        if v["meas_peak"] > 0 and v["bw"] > 0:
            d = v["meas_peak"] / v["bw"] - 1
            if abs(d) > ACCURACY_TOL:
                check(res, "peak_accuracy", "fail",
                      f"{lbl}: measured peak {d*100:+.0f}% vs BANDWIDTH (>10%)")
        if v["meas_avg"] > 0 and v["avg"] > 0:
            d = v["meas_avg"] / v["avg"] - 1
            if abs(d) > ACCURACY_TOL:
                check(res, "avg_accuracy", "fail",
                      f"{lbl}: measured avg {d*100:+.0f}% vs AVERAGE-BANDWIDTH (>10%)")

    # adjacent-rung rules (ascending by peak)
    for i in range(1, len(variants)):
        prev, cur = variants[i - 1], variants[i]
        if cur["bw"] == prev["bw"]:
            check(res, "duplicate_bandwidth", "fail",
                  f"{prev['resolution']} and {cur['resolution']} share BANDWIDTH={cur['bw']}")
            continue
        ratio = cur["bw"] / prev["bw"] if prev["bw"] else 0
        if ratio and ratio < MIN_PEAK_SPACING:
            check(res, "tight_spacing", "fail",
                  f"{prev['resolution']}->{cur['resolution']} peak ratio {ratio:.2f}x < {MIN_PEAK_SPACING}x")
        elif ratio > MAX_PEAK_SPACING:
            check(res, "wide_spacing", "warn",
                  f"{prev['resolution']}->{cur['resolution']} peak ratio {ratio:.2f}x > {MAX_PEAK_SPACING}x")
        if prev["avg"] > 0 and cur["avg"] > 0 and cur["avg"] < prev["avg"]:
            check(res, "inversion", "fail",
                  f"{cur['resolution']} avg {cur['avg']} < {prev['resolution']} avg {prev['avg']} while peak rises")
        # avg/peak overlap — symptom of tight spacing; reported, not a separate fail
        if cur["avg"] > 0 and cur["avg"] <= prev["bw"]:
            check(res, "avg_peak_overlap", "info",
                  f"{cur['resolution']} avg {cur['avg']} <= {prev['resolution']} peak {prev['bw']} "
                  f"(overlap band — avg-keyed players over-select; see tight_spacing)")

    if len(variants) < 4:
        check(res, "few_tiers", "warn", f"only {len(variants)} video tiers")

    # VMAF monotonicity / redundancy / cliffs (ascending by peak)
    vmafs = [(v["resolution"], v.get("vmaf", {}).get("mean")) for v in variants]
    have = [(r, m) for r, m in vmafs if isinstance(m, (int, float))]
    for i in range(1, len(have)):
        (pr, pm), (cr, cm) = have[i - 1], have[i]
        if cm < pm - 0.5:
            check(res, "vmaf_inversion", "fail", f"{cr} VMAF {cm} < {pr} VMAF {pm} (higher bitrate, lower quality)")
        elif abs(cm - pm) < VMAF_REDUNDANT_DELTA:
            check(res, "vmaf_redundant_rung", "warn",
                  f"{pr}->{cr} VMAF {pm}->{cm} (Δ<{VMAF_REDUNDANT_DELTA}) — rung may be redundant")
        elif cm - pm > VMAF_CLIFF_DELTA:
            check(res, "vmaf_cliff", "warn", f"{pr}->{cr} VMAF jumps {pm}->{cm} (Δ>{VMAF_CLIFF_DELTA}) — quality cliff")
    return res


# ---- report rendering ------------------------------------------------------
def render_markdown(audit):
    L = []
    L.append("## Ladder audit\n")
    L.append(f"- source: `{audit['source']}` ({audit['mode']})")
    L.append(f"- audited: {audit['audited_at']}")
    s = audit["summary"]
    L.append(f"- result: **{s['fails']} fail / {s['warns']} warn / {s['infos']} info** "
             f"across {s['variants']} tiers\n")
    L.append("| res | adv peak | meas peak Δ% | adv avg | meas avg Δ% | peak/prev | peak/avg | VMAF |")
    L.append("|---|---|---|---|---|---|---|---|")
    prev = None
    for v in audit["variants"]:
        dpk = f"{(v['meas_peak']/v['bw']-1)*100:+.0f}%" if v["meas_peak"] and v["bw"] else "-"
        dav = f"{(v['meas_avg']/v['avg']-1)*100:+.0f}%" if v["meas_avg"] and v["avg"] else "-"
        ratio = f"{v['bw']/prev['bw']:.2f}x" if prev and prev["bw"] else "-"
        pa = f"{v['bw']/v['avg']:.2f}x" if v["avg"] else "-"
        vm = v.get("vmaf", {}).get("mean", "-")
        L.append(f"| {v['resolution']} | {v['bw']} | {dpk} | {v['avg']} | {dav} | {ratio} | {pa} | {vm} |")
        prev = v
    L.append("")
    for c in audit["checks"]:
        icon = {"fail": "❌", "warn": "⚠️", "info": "ℹ️"}.get(c["severity"], "•")
        L.append(f"- {icon} **{c['kind']}** — {c['detail']}")
    L.append("")
    return "\n".join(L)


# ---- main ------------------------------------------------------------------
def find_master(d):
    for name in ("master.m3u8", "master_2s.m3u8"):
        p = os.path.join(d, name)
        if os.path.exists(p):
            return p
    raise SystemExit(f"no master.m3u8 / master_2s.m3u8 in {d}")


def main():
    ap = argparse.ArgumentParser(description="Audit an HLS ABR ladder.")
    g = ap.add_mutually_exclusive_group(required=True)
    g.add_argument("--dir", help="encode output dir (reads master + segments off disk)")
    g.add_argument("--url", help="master playlist URL (live/origin)")
    ap.add_argument("--vmaf-ref", help="mezzanine reference video; enables VMAF when set")
    ap.add_argument("--no-vmaf", action="store_true", help="skip VMAF even if --vmaf-ref given")
    ap.add_argument("--vmaf-full", action="store_true", help="VMAF every frame (no subsample)")
    ap.add_argument("--vmaf-subsample", type=int, default=VMAF_SUBSAMPLE)
    ap.add_argument("--segments", type=int, default=MEASURE_SEGMENTS)
    ap.add_argument("--json", action="store_true", help="print ladder_audit.json to stdout")
    ap.add_argument("--out", help="write ladder_audit.json here (default <dir>/ladder_audit.json for --dir)")
    ap.add_argument("--report-md", help="append the markdown section to this file (e.g. ENCODING_REPORT.md)")
    args = ap.parse_args()

    base = find_master(args.dir) if args.dir else args.url
    master = load_text(base)
    variants, audio_uri = parse_master(master)
    if not variants:
        raise SystemExit("no #EXT-X-STREAM-INF variants in master playlist")

    # audio (advertised values already include it; measure it to fold into combined)
    a_peak, a_avg = (measure(base, audio_uri, args.segments) if audio_uri else (0.0, 0.0))

    for v in variants:
        vp, va = measure(base, v["uri"], args.segments)
        v["meas_peak"] = round(vp + a_peak)   # combined video+audio, as advertised
        v["meas_avg"] = round(va + a_avg)

    # VMAF (encode-time only — needs the mezzanine; skip on live audits without a ref)
    do_vmaf = bool(args.vmaf_ref) and not args.no_vmaf
    if do_vmaf:
        for v in variants:
            wh = v["resolution"].split("x") if "x" in v["resolution"] else None
            if not wh:
                continue
            v["vmaf"] = measure_vmaf(base, v["uri"], args.vmaf_ref, wh[0], wh[1],
                                     args.vmaf_subsample, args.vmaf_full)

    variants.sort(key=lambda v: v["bw"])
    checks = run_checks(variants, (a_peak, a_avg))
    summary = {
        "variants": len(variants),
        "fails": sum(c["severity"] == "fail" for c in checks),
        "warns": sum(c["severity"] == "warn" for c in checks),
        "infos": sum(c["severity"] == "info" for c in checks),
    }
    audit = {
        "tool": "ladder_audit.py",
        "audited_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "source": base,
        "mode": "on-disk" if args.dir else "live",
        "vmaf": "subsampled" if (do_vmaf and not args.vmaf_full) else ("full" if do_vmaf else "off"),
        "audio_measured_bps": {"peak": round(a_peak), "avg": round(a_avg)},
        "variants": variants,
        "checks": checks,
        "summary": summary,
    }

    out_path = args.out or (os.path.join(args.dir, "ladder_audit.json") if args.dir else None)
    if out_path:
        with open(out_path, "w") as f:
            json.dump(audit, f, indent=2)
        print(f"wrote {out_path}", file=sys.stderr)
    if args.report_md:
        with open(args.report_md, "a") as f:
            f.write("\n" + render_markdown(audit) + "\n")
        print(f"appended ladder-audit section to {args.report_md}", file=sys.stderr)

    if args.json:
        print(json.dumps(audit, indent=2))
    else:
        print(render_markdown(audit))

    return 1 if summary["fails"] else 0


if __name__ == "__main__":
    raise SystemExit(main())
