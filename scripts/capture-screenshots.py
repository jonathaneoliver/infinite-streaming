#!/usr/bin/env python3
"""Capture the dashboard screenshots referenced from README.md and docs/.

Drives a real system Google Chrome via Playwright (headful, with H.264 decoders)
so videos render real frames and charts populate from live session state.

Prereqs:
    pip install -r scripts/requirements.txt
    playwright install chrome

Usage:
    python scripts/capture-screenshots.py [--base-url URL] [--only NAME] [--session-id ID]

By default captures against http://localhost:30000 and writes to repo/screenshots/.
"""

from __future__ import annotations

import argparse
import json
import sys
import time
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Callable, Optional

from playwright.sync_api import Page, TimeoutError as PWTimeout, sync_playwright


REPO_ROOT = Path(__file__).resolve().parent.parent
SCREENSHOT_DIR = REPO_ROOT / "screenshots"
VIEWPORT = {"width": 1600, "height": 1000}
DEVICE_SCALE = 2


@dataclass
class PageSpec:
    name: str
    path_template: str
    ready: Optional[Callable[[Page], None]] = None
    settle_seconds: float = 0.0
    full_page: bool = True


def wait_video_ready(page: Page, timeout_ms: int = 15000) -> None:
    """Wait for at least one <video> in the page to reach readyState >= 2 (HAVE_CURRENT_DATA)."""
    try:
        page.wait_for_function(
            """() => {
                const vids = Array.from(document.querySelectorAll('video'));
                return vids.some(v => v.readyState >= 2);
            }""",
            timeout=timeout_ms,
        )
    except PWTimeout:
        print(f"    [warn] no <video> reached readyState>=2 within {timeout_ms}ms; capturing anyway")


def wait_any_selector(page: Page, selectors: list[str], timeout_ms: int = 10000) -> None:
    """Wait until any one of the given selectors matches at least one element."""
    try:
        page.wait_for_selector(", ".join(selectors), timeout=timeout_ms)
    except PWTimeout:
        print(f"    [warn] none of {selectors} appeared within {timeout_ms}ms; capturing anyway")


def wait_bandwidth_chart_has_data(page: Page, min_points: int = 3, timeout_ms: int = 30000) -> None:
    """Wait for the testing-session bitrate chart to have at least `min_points` data points."""
    try:
        page.wait_for_function(
            f"""() => {{
                const canvases = Array.from(document.querySelectorAll('canvas.bandwidth-chart, .bandwidth-chart canvas'));
                if (canvases.length === 0) return false;
                // Chart.js attaches instances; look for a dataset with data.length >= N
                for (const c of canvases) {{
                    const chart = window.Chart?.getChart?.(c);
                    if (!chart) continue;
                    const datasets = chart.data?.datasets || [];
                    if (datasets.some(d => (d.data?.length || 0) >= {min_points})) return true;
                }}
                return false;
            }}""",
            timeout=timeout_ms,
        )
    except PWTimeout:
        print(f"    [warn] bitrate chart never got >= {min_points} points; capturing anyway")


PAGES: list[PageSpec] = [
    PageSpec("dashboard",      "/dashboard/dashboard.html"),
    PageSpec("upload-content", "/dashboard/upload.html"),
    PageSpec(
        "source-library", "/dashboard/sources.html",
        ready=lambda p: wait_any_selector(p, [".source-card", ".source-row", ".sources-list", "table"], 8000),
    ),
    PageSpec("encoding-jobs",  "/dashboard/jobs.html"),
    PageSpec(
        "playback", "/dashboard/playback.html",
        ready=lambda p: wait_video_ready(p, 15000),
        settle_seconds=2.0,
    ),
    PageSpec(
        "mosaic", "/dashboard/grid.html",
        ready=lambda p: wait_video_ready(p, 20000),
        settle_seconds=3.0,
    ),
    PageSpec(
        "live-offset", "/dashboard/segment-duration-comparison.html",
        ready=lambda p: wait_video_ready(p, 20000),
        settle_seconds=3.0,
    ),
    # testing-session.html is appended dynamically once we resolve a live session.
]


def pick_active_session(base_url: str, session_id: Optional[str]) -> Optional[dict]:
    """Return a session dict suitable for building testing-session.html URL, or None."""
    try:
        with urllib.request.urlopen(f"{base_url}/api/sessions", timeout=5) as resp:
            sessions = json.loads(resp.read())
    except Exception as e:
        print(f"    [warn] could not fetch /api/sessions: {e}")
        return None
    if not sessions:
        return None
    if session_id:
        for s in sessions:
            if s.get("player_id") == session_id or str(s.get("session_id")) == str(session_id):
                return s
        print(f"    [warn] requested session {session_id!r} not found; falling back to first active")
    return sessions[0]


def build_testing_session_path(session: dict) -> str:
    """Construct /dashboard/testing-session.html?... from a session dict."""
    player_id = session.get("player_id") or ""
    manifest_url = session.get("manifest_url") or ""

    # manifest_url is like "go-live/<content>/playlist_6s_1080p.m3u8" (variant).
    # We want the master: "go-live/<content>/master_6s.m3u8" or master.m3u8 (LL).
    parts = manifest_url.split("/")
    # Detect variant segment-duration suffix, if any, to match the master variant.
    master = "master.m3u8"
    for p in parts:
        if "_2s_" in p or p.endswith("_2s.m3u8"):
            master = "master_2s.m3u8"; break
        if "_6s_" in p or p.endswith("_6s.m3u8"):
            master = "master_6s.m3u8"; break
    content = parts[1] if len(parts) >= 2 and parts[0] == "go-live" else ""
    stream_url = f"go-live/{content}/{master}" if content else manifest_url or ""

    # testing-session.html expects query params url= (absolute or path) and player_id=
    from urllib.parse import urlencode
    qs = urlencode({"url": stream_url, "player_id": player_id, "nav": "1"})
    return f"/dashboard/testing-session.html?{qs}"


def capture(page: Page, spec: PageSpec, base_url: str, out_dir: Path) -> Path:
    url = base_url.rstrip("/") + spec.path_template
    print(f"  → {spec.name:<18} {url}")
    # Use 'load' not 'networkidle' — the dashboard opens SSE streams that never idle.
    page.goto(url, wait_until="load", timeout=30000)
    # Give XHRs a brief moment to settle, but don't block forever if they don't.
    try:
        page.wait_for_load_state("networkidle", timeout=3000)
    except PWTimeout:
        pass
    if spec.ready:
        spec.ready(page)
    if spec.settle_seconds:
        page.wait_for_timeout(int(spec.settle_seconds * 1000))
    out_path = out_dir / f"{spec.name}.png"
    page.screenshot(path=str(out_path), full_page=spec.full_page)
    size_kb = out_path.stat().st_size // 1024
    print(f"     wrote {out_path.name} ({size_kb} KB)")
    return out_path


def main() -> int:
    ap = argparse.ArgumentParser(description="Capture InfiniteStream dashboard screenshots.")
    ap.add_argument("--base-url", default="http://localhost:30000",
                    help="Base URL of the running server (default: http://localhost:30000)")
    ap.add_argument("--only", help="Capture only the named page (e.g. 'mosaic')")
    ap.add_argument("--session-id", help="Use this player_id or session_id for testing-playback capture")
    ap.add_argument("--skip-testing-playback", action="store_true",
                    help="Skip testing-playback.png even if an active session is found")
    ap.add_argument("--output-dir", default=str(SCREENSHOT_DIR),
                    help=f"Where to write .png files (default: {SCREENSHOT_DIR})")
    args = ap.parse_args()

    base_url = args.base_url.rstrip("/")
    out_dir = Path(args.output_dir).resolve()
    out_dir.mkdir(parents=True, exist_ok=True)
    print(f"base_url: {base_url}")
    print(f"output:   {out_dir}\n")

    # Build page list, optionally appending the testing-playback spec.
    specs = list(PAGES)
    if not args.skip_testing_player:
        session = pick_active_session(base_url, args.session_id)
        if session:
            path = build_testing_session_path(session)
            print(f"[testing-playback] using live session player_id={session.get('player_id')}")
            specs.append(PageSpec(
                "testing-playback", path,
                ready=lambda p: wait_bandwidth_chart_has_data(p, min_points=3, timeout_ms=30000),
                settle_seconds=5.0,
            ))
        else:
            print("[testing-playback] no active session found — skipping. "
                  "Start a testing session and re-run to capture this one.")

    if args.only:
        specs = [s for s in specs if s.name == args.only]
        if not specs:
            print(f"error: no page named {args.only!r}")
            return 2

    with sync_playwright() as pw:
        browser = pw.chromium.launch(channel="chrome", headless=False,
                                     args=["--autoplay-policy=no-user-gesture-required"])
        try:
            context = browser.new_context(
                viewport=VIEWPORT,
                device_scale_factor=DEVICE_SCALE,
                ignore_https_errors=True,
            )
            page = context.new_page()
            failed: list[str] = []
            for spec in specs:
                try:
                    capture(page, spec, base_url, out_dir)
                except Exception as e:
                    print(f"     [ERROR] {spec.name}: {e}")
                    failed.append(spec.name)
            context.close()
            if failed:
                print(f"\n{len(failed)} failed: {', '.join(failed)}")
                return 1
            print(f"\n{len(specs)} screenshot(s) captured in {out_dir}")
        finally:
            browser.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
