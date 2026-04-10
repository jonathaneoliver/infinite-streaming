#!/usr/bin/env python3
"""
CRF bandwidth sweep for short content characterization.

Encodes a short clip at multiple resolutions and CRF values, then reports
measured average and peak bandwidth from the generated media segments.
"""

from __future__ import annotations

import argparse
import csv
import json
import math
import shutil
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Dict, Iterable, List, Optional, Set, Tuple


RESOLUTION_PRESETS = {
    "360p": (640, 360),
    "540p": (960, 540),
    "720p": (1280, 720),
    "1080p": (1920, 1080),
    "1440p": (2560, 1440),
    "2160p": (3840, 2160),
}


@dataclass
class SweepRow:
    codec: str
    mode: str
    encoder: str
    resolution: str
    width: int
    height: int
    crf: int
    hw_quality_qv: Optional[int]
    target_avg_mbps: Optional[float]
    avg_mbps: float
    peak_mbps: float
    vmaf_mean: Optional[float]
    duration_s: float
    total_bytes: int
    segment_count: int
    output_mp4: Path


def run(cmd: List[str]) -> subprocess.CompletedProcess:
    return subprocess.run(cmd, check=True, text=True, capture_output=True)


def ffprobe_json(path: Path, entries: str, stream_selector: Optional[str] = None) -> str:
    cmd = [
        "ffprobe",
        "-v",
        "error",
    ]
    if stream_selector:
        cmd.extend(["-select_streams", stream_selector])
    cmd.extend(
        [
            "-show_entries",
            entries,
            "-of",
            "default=noprint_wrappers=1:nokey=1",
            str(path),
        ]
    )
    return run(cmd).stdout.strip()


def parse_fraction(frac: str) -> float:
    if not frac or frac == "0/0":
        return 25.0
    if "/" in frac:
        n, d = frac.split("/", 1)
        n_f = float(n)
        d_f = float(d)
        if d_f == 0:
            return 25.0
        return n_f / d_f
    return float(frac)


def parse_mode_list(raw: str) -> List[str]:
    out: List[str] = []
    for token in [x.strip().lower() for x in raw.split(",") if x.strip()]:
        if token not in {"sw", "hw", "hwmatch"}:
            raise ValueError(f"Unsupported mode token: {token} (use sw,hw,hwmatch)")
        out.append(token)
    if not out:
        raise ValueError("No modes parsed from --modes")
    deduped = []
    seen: Set[str] = set()
    for mode in out:
        if mode in seen:
            continue
        seen.add(mode)
        deduped.append(mode)
    return deduped


def parse_codec_list(raw: str) -> List[str]:
    out: List[str] = []
    for token in [x.strip().lower() for x in raw.split(",") if x.strip()]:
        if token in {"h265", "x265"}:
            token = "hevc"
        if token not in {"h264", "hevc"}:
            raise ValueError(f"Unsupported codec token: {token} (use h264,hevc)")
        out.append(token)
    if not out:
        raise ValueError("No codecs parsed from --codecs")
    deduped: List[str] = []
    seen: Set[str] = set()
    for codec in out:
        if codec in seen:
            continue
        seen.add(codec)
        deduped.append(codec)
    return deduped


def list_encoders() -> Set[str]:
    cp = run(["ffmpeg", "-hide_banner", "-encoders"])
    encoders: Set[str] = set()
    for line in cp.stdout.splitlines():
        parts = line.strip().split()
        if len(parts) >= 2 and parts[0].startswith("V"):
            encoders.add(parts[1])
    return encoders


def select_encoder(codec: str, mode: str, available: Set[str]) -> str:
    preferred: Dict[Tuple[str, str], str] = {
        ("h264", "sw"): "libx264",
        ("hevc", "sw"): "libx265",
        ("h264", "hw"): "h264_videotoolbox",
        ("hevc", "hw"): "hevc_videotoolbox",
        ("h264", "hwmatch"): "h264_videotoolbox",
        ("hevc", "hwmatch"): "hevc_videotoolbox",
    }
    enc = preferred[(codec, mode)]
    if enc not in available:
        raise RuntimeError(
            f"Requested {mode} encoder for codec={codec} is not available: {enc}"
        )
    return enc


def map_crf_to_videotoolbox_qv(crf: int) -> int:
    # Rough mapping only: VideoToolbox does not support true CRF.
    # Keep quality values in a practical range where lower CRF -> higher quality.
    qv = int(round((52 - crf) * 1.5))
    return max(1, min(100, qv))


def detect_source_dims_and_fps(input_path: Path) -> Tuple[int, int, float]:
    dims = ffprobe_json(input_path, "stream=width,height,avg_frame_rate", "v:0").splitlines()
    if len(dims) < 3:
        raise RuntimeError(f"Could not read source video stream metadata from {input_path}")
    width = int(float(dims[0]))
    height = int(float(dims[1]))
    fps = parse_fraction(dims[2])
    if fps <= 0:
        fps = 25.0
    return width, height, fps


def parse_resolution_list(raw: str) -> List[Tuple[str, int, int]]:
    out: List[Tuple[str, int, int]] = []
    for token in [x.strip() for x in raw.split(",") if x.strip()]:
        if token in RESOLUTION_PRESETS:
            w, h = RESOLUTION_PRESETS[token]
            out.append((token, w, h))
            continue
        if "x" in token.lower():
            w_s, h_s = token.lower().split("x", 1)
            w = int(w_s)
            h = int(h_s)
            out.append((f"{h}p", w, h))
            continue
        raise ValueError(f"Unsupported resolution token: {token}")
    if not out:
        raise ValueError("No resolutions parsed from --resolutions")
    return out


def parse_crf_list(raw: str) -> List[int]:
    vals = [int(x.strip()) for x in raw.split(",") if x.strip()]
    if not vals:
        raise ValueError("No CRF values parsed from --crf-list")
    return vals


def maybe_filter_resolutions(
    resolutions: List[Tuple[str, int, int]],
    src_w: int,
    src_h: int,
    allow_upscale: bool,
) -> List[Tuple[str, int, int]]:
    if allow_upscale:
        return resolutions
    return [r for r in resolutions if r[1] <= src_w and r[2] <= src_h]


def build_scale_pad_filter(width: int, height: int) -> str:
    # Keep aspect ratio and pad to exact output size.
    return (
        f"scale=w={width}:h={height}:force_original_aspect_ratio=decrease,"
        f"pad={width}:{height}:(ow-iw)/2:(oh-ih)/2"
    )


def encode_variant(
    input_path: Path,
    out_mp4: Path,
    codec: str,
    mode: str,
    encoder: str,
    crf: int,
    width: int,
    height: int,
    fps: float,
    start_s: float,
    duration_s: float,
    seg_duration_s: float,
    preset: str,
    hw_match_target_mbps: Optional[float] = None,
    hw_match_maxrate_mult: float = 1.35,
    hw_match_bufsize_mult: float = 2.0,
) -> Optional[int]:
    out_mp4.parent.mkdir(parents=True, exist_ok=True)
    keyint = max(1, int(round(fps * seg_duration_s)))
    filter_expr = build_scale_pad_filter(width, height)

    cmd = [
        "ffmpeg",
        "-y",
        "-ss",
        f"{start_s:.3f}",
        "-t",
        f"{duration_s:.3f}",
        "-i",
        str(input_path),
        "-an",
        "-vf",
        filter_expr,
    ]

    hw_quality_qv: Optional[int] = None
    if mode == "sw" and codec == "h264":
        cmd.extend(
            [
                "-c:v",
                encoder,
                "-preset",
                preset,
                "-crf",
                str(crf),
                "-x264-params",
                f"keyint={keyint}:min-keyint={keyint}:scenecut=0",
                "-g",
                str(keyint),
            ]
        )
    elif mode == "sw" and codec == "hevc":
        cmd.extend(
            [
                "-c:v",
                encoder,
                "-preset",
                preset,
                "-crf",
                str(crf),
                "-x265-params",
                f"keyint={keyint}:min-keyint={keyint}:scenecut=0",
                "-g",
                str(keyint),
            ]
        )
    elif mode == "hw" and codec in {"h264", "hevc"}:
        hw_quality_qv = map_crf_to_videotoolbox_qv(crf)
        cmd.extend(
            [
                "-c:v",
                encoder,
                "-q:v",
                str(hw_quality_qv),
                "-g",
                str(keyint),
            ]
        )
    elif mode == "hwmatch" and codec in {"h264", "hevc"}:
        if hw_match_target_mbps is None or hw_match_target_mbps <= 0:
            raise ValueError(
                f"hwmatch requires a positive target bitrate (got {hw_match_target_mbps})"
            )
        target_bps = int(round(hw_match_target_mbps * 1_000_000.0))
        maxrate_bps = int(round(target_bps * hw_match_maxrate_mult))
        bufsize_bps = int(round(target_bps * hw_match_bufsize_mult))
        cmd.extend(
            [
                "-c:v",
                encoder,
                "-b:v",
                str(target_bps),
                "-maxrate",
                str(maxrate_bps),
                "-bufsize",
                str(bufsize_bps),
                "-g",
                str(keyint),
            ]
        )
    else:
        raise ValueError(f"Unsupported encode combination: codec={codec}, mode={mode}")

    cmd.extend(["-movflags", "+faststart", str(out_mp4)])
    subprocess.run(cmd, check=True)
    return hw_quality_qv


def compute_vmaf(
    source_path: Path,
    encoded_mp4: Path,
    width: int,
    height: int,
    fps: float,
    start_s: float,
    duration_s: float,
    log_path: Path,
) -> float:
    log_path.parent.mkdir(parents=True, exist_ok=True)
    filter_expr = (
        f"[0:v]fps={fps:.6f},setpts=PTS-STARTPTS,"
        f"{build_scale_pad_filter(width, height)}[ref];"
        f"[1:v]fps={fps:.6f},setpts=PTS-STARTPTS,"
        f"{build_scale_pad_filter(width, height)}[dist];"
        f"[dist][ref]libvmaf=log_fmt=json:log_path={log_path}"
    )
    subprocess.run(
        [
            "ffmpeg",
            "-v",
            "error",
            "-ss",
            f"{start_s:.3f}",
            "-t",
            f"{duration_s:.3f}",
            "-i",
            str(source_path),
            "-i",
            str(encoded_mp4),
            "-filter_complex",
            filter_expr,
            "-f",
            "null",
            "-",
        ],
        check=True,
    )
    with log_path.open("r", encoding="utf-8") as f:
        payload = json.load(f)
    pooled = payload.get("pooled_metrics", {})
    vmaf = pooled.get("vmaf", {})
    mean = vmaf.get("mean")
    if mean is None:
        raise RuntimeError(f"Could not parse VMAF mean from {log_path}")
    return float(mean)


def segment_fmp4(encoded_mp4: Path, seg_dir: Path, seg_duration_s: float) -> None:
    seg_dir.mkdir(parents=True, exist_ok=True)
    init_path = seg_dir / "init.mp4"
    seg_tpl = seg_dir / "segment_%05d.m4s"

    subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-i",
            str(encoded_mp4),
            "-c",
            "copy",
            "-t",
            "0",
            "-movflags",
            "frag_keyframe+empty_moov+default_base_moof+separate_moof",
            str(init_path),
        ],
        check=True,
    )

    subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-i",
            str(encoded_mp4),
            "-c",
            "copy",
            "-map",
            "0:v:0",
            "-f",
            "segment",
            "-segment_format",
            "mp4",
            "-segment_time",
            f"{seg_duration_s:.3f}",
            "-segment_format_options",
            "movflags=frag_keyframe+empty_moov+default_base_moof+separate_moof",
            "-reset_timestamps",
            "1",
            str(seg_tpl),
        ],
        check=True,
    )


def file_size(path: Path) -> int:
    return path.stat().st_size


def measure_segments(seg_dir: Path, include_init: bool = True) -> Tuple[float, float, float, int, int]:
    init_path = seg_dir / "init.mp4"
    seg_files = sorted(seg_dir.glob("segment_*.m4s"))
    if not seg_files:
        raise RuntimeError(f"No segments found in {seg_dir}")

    total_bytes = 0
    if include_init and init_path.exists():
        total_bytes += file_size(init_path)

    total_duration = 0.0
    peak_bps = 0.0

    for seg in seg_files:
        seg_bytes = file_size(seg)
        seg_dur_txt = ffprobe_json(seg, "format=duration")
        seg_dur = float(seg_dur_txt) if seg_dur_txt and seg_dur_txt != "N/A" else 0.0
        if seg_dur <= 0:
            continue
        total_bytes += seg_bytes
        total_duration += seg_dur
        seg_bps = (seg_bytes * 8.0) / seg_dur
        if seg_bps > peak_bps:
            peak_bps = seg_bps

    if total_duration <= 0:
        raise RuntimeError(f"Could not measure segment durations in {seg_dir}")
    avg_bps = (total_bytes * 8.0) / total_duration
    return avg_bps, peak_bps, total_duration, total_bytes, len(seg_files)


def row_key(row: SweepRow) -> Tuple[str, str, str, int]:
    return (row.codec, row.mode, row.resolution, row.crf)


def parse_opt_float(txt: Optional[str]) -> Optional[float]:
    if txt is None:
        return None
    s = txt.strip()
    if not s:
        return None
    return float(s)


def parse_opt_int(txt: Optional[str]) -> Optional[int]:
    if txt is None:
        return None
    s = txt.strip()
    if not s:
        return None
    return int(s)


def load_rows_from_csv(path: Path) -> List[SweepRow]:
    rows: List[SweepRow] = []
    with path.open("r", newline="", encoding="utf-8") as f:
        reader = csv.DictReader(f)
        for rec in reader:
            rows.append(
                SweepRow(
                    codec=rec.get("codec", "").strip(),
                    mode=rec.get("mode", "").strip(),
                    encoder=rec.get("encoder", "").strip(),
                    resolution=rec.get("resolution", "").strip(),
                    width=int(float(rec.get("width", "0") or 0)),
                    height=int(float(rec.get("height", "0") or 0)),
                    crf=int(float(rec.get("crf", "0") or 0)),
                    hw_quality_qv=parse_opt_int(rec.get("hw_quality_qv")),
                    target_avg_mbps=parse_opt_float(rec.get("target_avg_mbps")),
                    avg_mbps=float(rec.get("avg_bandwidth_mbps", "0") or 0),
                    peak_mbps=float(rec.get("peak_bandwidth_mbps", "0") or 0),
                    vmaf_mean=parse_opt_float(rec.get("vmaf_mean")),
                    duration_s=float(rec.get("measured_duration_s", "0") or 0),
                    total_bytes=int(float(rec.get("total_bytes", "0") or 0)),
                    segment_count=int(float(rec.get("segment_count", "0") or 0)),
                    output_mp4=Path(rec.get("output_mp4", "").strip() or "."),
                )
            )
    return rows


def build_hwmatch_target_map(
    csv_path: Path, source_mode: str
) -> Dict[Tuple[str, str, int], float]:
    source_rows = load_rows_from_csv(csv_path)
    out: Dict[Tuple[str, str, int], float] = {}
    for r in source_rows:
        if r.mode != source_mode:
            continue
        if r.avg_mbps <= 0:
            continue
        out[(r.codec, r.resolution, r.crf)] = r.avg_mbps
    return out


def merge_rows(existing: List[SweepRow], new_rows: List[SweepRow]) -> List[SweepRow]:
    merged: Dict[Tuple[str, str, str, int], SweepRow] = {}
    for row in existing:
        merged[row_key(row)] = row
    for row in new_rows:
        merged[row_key(row)] = row
    rows = list(merged.values())
    rows.sort(key=lambda r: (r.codec, r.mode, r.height, r.crf))
    return rows


def write_csv(rows: Iterable[SweepRow], out_csv: Path) -> None:
    out_csv.parent.mkdir(parents=True, exist_ok=True)
    with out_csv.open("w", newline="", encoding="utf-8") as f:
        w = csv.writer(f)
        w.writerow(
            [
                "resolution",
                "width",
                "height",
                "codec",
                "mode",
                "encoder",
                "crf",
                "hw_quality_qv",
                "target_avg_mbps",
                "avg_bandwidth_mbps",
                "peak_bandwidth_mbps",
                "vmaf_mean",
            ]
        )
        for r in rows:
            w.writerow(
                [
                    r.resolution,
                    r.width,
                    r.height,
                    r.codec,
                    r.mode,
                    r.encoder,
                    r.crf,
                    "" if r.hw_quality_qv is None else r.hw_quality_qv,
                    "" if r.target_avg_mbps is None else f"{r.target_avg_mbps:.3f}",
                    f"{r.avg_mbps:.3f}",
                    f"{r.peak_mbps:.3f}",
                    "" if r.vmaf_mean is None else f"{r.vmaf_mean:.3f}",
                ]
            )


def print_table(rows: List[SweepRow]) -> None:
    if not rows:
        print("No rows generated.")
        return
    print(
        f"{'RES':<8} {'CODEC':<5} {'MODE':<7} {'ENCODER':<18} {'CRF':>4} {'HW_Q':>5} {'TGT':>7} "
        f"{'AVG_Mbps':>10} {'PEAK_Mbps':>10} {'VMAF':>7}"
    )
    print(
        f"{'-'*8} {'-'*5} {'-'*7} {'-'*18} {'-'*4} {'-'*5} {'-'*7} "
        f"{'-'*10} {'-'*10} {'-'*7}"
    )
    for r in rows:
        hw_q = "-" if r.hw_quality_qv is None else str(r.hw_quality_qv)
        tgt = "-" if r.target_avg_mbps is None else f"{r.target_avg_mbps:.2f}"
        vmaf = "-" if r.vmaf_mean is None else f"{r.vmaf_mean:.2f}"
        print(
            f"{r.resolution:<8} {r.codec:<5} {r.mode:<7} {r.encoder:<18} {r.crf:>4d} {hw_q:>5} {tgt:>7} "
            f"{r.avg_mbps:>10.3f} {r.peak_mbps:>10.3f} {vmaf:>7}"
        )


def ensure_tool(name: str) -> None:
    if shutil.which(name) is None:
        raise RuntimeError(f"Required tool not found in PATH: {name}")


def main() -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Encode first N seconds across resolutions/CRFs and report measured "
            "average + peak bandwidth from segments."
        )
    )
    parser.add_argument(
        "--input",
        required=True,
        type=Path,
        help="Input source file (e.g., original MKV)",
    )
    parser.add_argument(
        "--duration",
        type=float,
        default=18.0,
        help="Clip duration in seconds (default: 18)",
    )
    parser.add_argument(
        "--start",
        type=float,
        default=0.0,
        help="Clip start offset in seconds (default: 0)",
    )
    parser.add_argument(
        "--codec",
        choices=["h264", "hevc"],
        default=None,
        help=argparse.SUPPRESS,
    )
    parser.add_argument(
        "--codecs",
        default="h264,hevc",
        help="Comma list of codecs: h264,hevc (default: h264,hevc)",
    )
    parser.add_argument(
        "--modes",
        default="sw,hw",
        help="Comma list of encode modes: sw,hw,hwmatch (default: sw,hw)",
    )
    parser.add_argument(
        "--resolutions",
        default="360p,540p,720p,1080p,1440p,2160p",
        help="Comma list (e.g. 720p,1080p,2160p or 1920x1080,3840x2160)",
    )
    parser.add_argument(
        "--crf-list",
        default="18,20,22,24,26",
        help="Comma list of CRF values (default: 18,20,22,24,26)",
    )
    parser.add_argument(
        "--lowres-extra-crf-list",
        default="",
        help=(
            "Optional extra CRFs only for low resolutions "
            "(e.g. 28,30). Applied when height <= --lowres-max-height."
        ),
    )
    parser.add_argument(
        "--lowres-max-height",
        type=int,
        default=540,
        help="Max height for low-res extra CRFs (default: 540)",
    )
    parser.add_argument(
        "--segment-duration",
        type=float,
        default=2.0,
        help="Segment duration for measurement in seconds (default: 2.0)",
    )
    parser.add_argument(
        "--preset",
        default="medium",
        help="Encoder preset (default: medium)",
    )
    parser.add_argument(
        "--output-dir",
        type=Path,
        default=Path("generate_abr/output/crf_sweep"),
        help="Output directory for artifacts and CSV",
    )
    parser.add_argument(
        "--allow-upscale",
        action="store_true",
        help="Allow testing resolutions above source size",
    )
    parser.add_argument(
        "--exclude-init",
        action="store_true",
        help="Exclude init.mp4 bytes from average bitrate computation",
    )
    parser.add_argument(
        "--vmaf",
        action="store_true",
        help="Compute VMAF for each encode (slower)",
    )
    parser.add_argument(
        "--hw-match-sw-csv",
        type=Path,
        default=None,
        help=(
            "For mode=hwmatch, CSV that provides source target avg bitrate rows "
            "(typically software rows from a previous run)"
        ),
    )
    parser.add_argument(
        "--hw-match-source-mode",
        default="sw",
        help="Source mode to read from --hw-match-sw-csv (default: sw)",
    )
    parser.add_argument(
        "--hw-match-maxrate-mult",
        type=float,
        default=1.35,
        help="For hwmatch: maxrate multiplier against target avg bitrate (default: 1.35)",
    )
    parser.add_argument(
        "--hw-match-bufsize-mult",
        type=float,
        default=2.0,
        help="For hwmatch: bufsize multiplier against target avg bitrate (default: 2.0)",
    )
    parser.add_argument(
        "--skip-existing-csv",
        type=Path,
        default=None,
        help=(
            "Skip rows already present in this CSV "
            "(key: codec+mode+resolution+crf)"
        ),
    )
    parser.add_argument(
        "--merge-with-csv",
        type=Path,
        default=None,
        help="Merge newly generated rows with this CSV",
    )
    parser.add_argument(
        "--merged-output-csv",
        type=Path,
        default=None,
        help=(
            "Merged output CSV path. Default: overwrite --merge-with-csv if set, "
            "otherwise <output-dir>/crf_bandwidth_sweep_merged.csv"
        ),
    )
    args = parser.parse_args()

    ensure_tool("ffmpeg")
    ensure_tool("ffprobe")

    input_path = args.input.expanduser().resolve()
    if not input_path.exists():
        raise SystemExit(f"Input not found: {input_path}")

    src_w, src_h, src_fps = detect_source_dims_and_fps(input_path)
    print(f"Source: {input_path}")
    print(f"Source characteristics: {src_w}x{src_h} @ {src_fps:.3f} fps")

    modes = parse_mode_list(args.modes)
    codecs = [args.codec] if args.codec else parse_codec_list(args.codecs)
    available_encoders = list_encoders()
    encoder_by_codec_mode: Dict[Tuple[str, str], str] = {}
    for codec in codecs:
        for mode in modes:
            encoder_by_codec_mode[(codec, mode)] = select_encoder(codec, mode, available_encoders)

    print("Selected encoders:")
    for codec in codecs:
        print(
            f"  {codec}: "
            + ", ".join(
                f"{mode}={encoder_by_codec_mode[(codec, mode)]}" for mode in modes
            )
        )

    resolutions = parse_resolution_list(args.resolutions)
    resolutions = maybe_filter_resolutions(
        resolutions, src_w, src_h, allow_upscale=args.allow_upscale
    )
    if not resolutions:
        raise SystemExit("No usable resolutions after filtering.")
    crf_values = parse_crf_list(args.crf_list)
    lowres_extra_crf_values = (
        parse_crf_list(args.lowres_extra_crf_list)
        if args.lowres_extra_crf_list.strip()
        else []
    )
    if lowres_extra_crf_values:
        print(
            "Low-res extra CRFs enabled: "
            f"{lowres_extra_crf_values} for height <= {args.lowres_max_height}"
        )

    out_root = args.output_dir.expanduser().resolve()
    out_root.mkdir(parents=True, exist_ok=True)

    rows: List[SweepRow] = []
    skip_keys: Set[Tuple[str, str, str, int]] = set()
    if args.skip_existing_csv is not None:
        skip_csv_path = args.skip_existing_csv.expanduser().resolve()
        if skip_csv_path.exists():
            for old_row in load_rows_from_csv(skip_csv_path):
                skip_keys.add(row_key(old_row))
            print(f"Skip-existing loaded {len(skip_keys)} keys from {skip_csv_path}")
        else:
            print(f"Skip-existing CSV not found, ignored: {skip_csv_path}")

    hwmatch_targets: Dict[Tuple[str, str, int], float] = {}
    if "hwmatch" in modes:
        if args.hw_match_sw_csv is None:
            raise SystemExit(
                "mode=hwmatch requires --hw-match-sw-csv pointing to a prior sweep CSV"
            )
        hw_csv_path = args.hw_match_sw_csv.expanduser().resolve()
        if not hw_csv_path.exists():
            raise SystemExit(f"--hw-match-sw-csv not found: {hw_csv_path}")
        hwmatch_targets = build_hwmatch_target_map(
            hw_csv_path, source_mode=args.hw_match_source_mode
        )
        print(
            f"HW match loaded {len(hwmatch_targets)} targets from {hw_csv_path} "
            f"(source mode={args.hw_match_source_mode})"
        )

    for codec in codecs:
        for mode in modes:
            enc = encoder_by_codec_mode[(codec, mode)]
            for label, width, height in resolutions:
                active_crf_values = list(crf_values)
                if lowres_extra_crf_values and height <= args.lowres_max_height:
                    for extra_crf in lowres_extra_crf_values:
                        if extra_crf not in active_crf_values:
                            active_crf_values.append(extra_crf)
                for crf in active_crf_values:
                    tag = f"{label}_crf{crf}_{codec}_{mode}"
                    case_dir = out_root / tag
                    mp4_path = case_dir / "encoded.mp4"
                    seg_dir = case_dir / "segments"
                    pending_key = (codec, mode, label, crf)
                    if pending_key in skip_keys:
                        print(
                            f"[skip] already present in skip CSV: "
                            f"mode={mode} codec={codec} res={label} crf={crf}"
                        )
                        continue

                    target_avg_mbps: Optional[float] = None
                    if mode == "hwmatch":
                        target_avg_mbps = hwmatch_targets.get((codec, label, crf))
                        if target_avg_mbps is None:
                            print(
                                f"[skip] no hwmatch target found in source CSV: "
                                f"mode={mode} codec={codec} res={label} crf={crf}"
                            )
                            continue

                    print(f"\n[encode] mode={mode} encoder={enc} {label} CRF={crf} codec={codec}")
                    try:
                        hw_quality_qv = encode_variant(
                            input_path=input_path,
                            out_mp4=mp4_path,
                            codec=codec,
                            mode=mode,
                            encoder=enc,
                            crf=crf,
                            width=width,
                            height=height,
                            fps=src_fps,
                            start_s=args.start,
                            duration_s=args.duration,
                            seg_duration_s=args.segment_duration,
                            preset=args.preset,
                            hw_match_target_mbps=target_avg_mbps,
                            hw_match_maxrate_mult=args.hw_match_maxrate_mult,
                            hw_match_bufsize_mult=args.hw_match_bufsize_mult,
                        )
                        segment_fmp4(mp4_path, seg_dir, args.segment_duration)
                        avg_bps, peak_bps, dur_s, total_bytes, seg_count = measure_segments(
                            seg_dir, include_init=not args.exclude_init
                        )

                        vmaf_mean: Optional[float] = None
                        if args.vmaf:
                            vmaf_log = case_dir / "vmaf.json"
                            print(f"[vmaf] mode={mode} codec={codec} {label} CRF={crf}")
                            vmaf_mean = compute_vmaf(
                                source_path=input_path,
                                encoded_mp4=mp4_path,
                                width=width,
                                height=height,
                                fps=src_fps,
                                start_s=args.start,
                                duration_s=args.duration,
                                log_path=vmaf_log,
                            )

                        rows.append(
                            SweepRow(
                                codec=codec,
                                mode=mode,
                                encoder=enc,
                                resolution=label,
                                width=width,
                                height=height,
                                crf=crf,
                                hw_quality_qv=hw_quality_qv,
                                target_avg_mbps=target_avg_mbps,
                                avg_mbps=avg_bps / 1_000_000.0,
                                peak_mbps=peak_bps / 1_000_000.0,
                                vmaf_mean=vmaf_mean,
                                duration_s=dur_s,
                                total_bytes=total_bytes,
                                segment_count=seg_count,
                                output_mp4=mp4_path,
                            )
                        )
                    except subprocess.CalledProcessError as exc:
                        print(
                            f"[skip] encode failed: mode={mode} codec={codec} "
                            f"res={label} crf={crf} exit={exc.returncode}"
                        )
                        continue

    rows.sort(key=lambda r: (r.codec, r.mode, r.height, r.crf))
    print("\n=== Sweep Results ===")
    print_table(rows)

    csv_path = out_root / "crf_bandwidth_sweep.csv"
    write_csv(rows, csv_path)
    print(f"\nCSV written: {csv_path}")

    if args.merge_with_csv is not None:
        merge_src = args.merge_with_csv.expanduser().resolve()
        if not merge_src.exists():
            raise SystemExit(f"--merge-with-csv not found: {merge_src}")
        existing = load_rows_from_csv(merge_src)
        merged_rows = merge_rows(existing, rows)
        merge_out = (
            args.merged_output_csv.expanduser().resolve()
            if args.merged_output_csv is not None
            else merge_src
        )
        write_csv(merged_rows, merge_out)
        print(
            f"Merged CSV written: {merge_out} "
            f"(existing={len(existing)} new={len(rows)} merged={len(merged_rows)})"
        )
    return 0


if __name__ == "__main__":
    sys.exit(main())
