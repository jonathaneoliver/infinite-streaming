#!/usr/bin/env python3
import argparse
from collections import deque
import concurrent.futures as cf
from datetime import datetime, timezone
import re
import time
import urllib.parse
import urllib.request

UA = "hls-speed-probe/1.0"


def utc_now_iso():
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def http_get_bytes(url, timeout=20):
    req = urllib.request.Request(
        url,
        headers={
            "User-Agent": UA,
            "Cache-Control": "no-cache",
            "Pragma": "no-cache",
        },
    )
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            data = r.read()
            status = getattr(r, "status", r.getcode())
        dt = time.time() - t0
        print(
            f"{utc_now_iso()} FETCH status={status} dur_ms={dt * 1000:.1f} bytes={len(data)} url={url}",
            flush=True,
        )
        return data, dt
    except Exception as exc:
        dt = time.time() - t0
        print(
            f"{utc_now_iso()} FETCH status=ERR dur_ms={dt * 1000:.1f} bytes=0 url={url} error={exc}",
            flush=True,
        )
        raise


def http_get_text(url, timeout=10):
    data, _ = http_get_bytes(url, timeout=timeout)
    return data.decode("utf-8", errors="replace")


def inherit_parent_query(parent_url, child_url):
    parent = urllib.parse.urlsplit(parent_url)
    child = urllib.parse.urlsplit(child_url)
    if child.query or not parent.query:
        return child_url
    return urllib.parse.urlunsplit(
        (child.scheme, child.netloc, child.path, parent.query, child.fragment)
    )


def pick_best_variant(master_text, base_url):
    lines = [x.strip() for x in master_text.splitlines()]
    best_bw = -1
    best_uri = None
    for i, line in enumerate(lines):
        if line.startswith("#EXT-X-STREAM-INF"):
            m = re.search(r"BANDWIDTH=(\d+)", line)
            bw = int(m.group(1)) if m else 0
            j = i + 1
            while j < len(lines) and (not lines[j] or lines[j].startswith("#")):
                j += 1
            if j < len(lines):
                uri = urllib.parse.urljoin(base_url, lines[j])
                uri = inherit_parent_query(base_url, uri)
                if bw > best_bw:
                    best_bw, best_uri = bw, uri
    return best_uri, best_bw


def parse_media_playlist(text, base_url):
    lines = [x.strip() for x in text.splitlines()]
    segs = []
    target = 6
    endlist = False
    for line in lines:
        if not line:
            continue
        if line.startswith("#EXT-X-TARGETDURATION:"):
            try:
                target = max(1, int(line.split(":", 1)[1]))
            except Exception:
                pass
        elif line.startswith("#EXT-X-ENDLIST"):
            endlist = True
        elif not line.startswith("#"):
            seg_url = urllib.parse.urljoin(base_url, line)
            segs.append(inherit_parent_query(base_url, seg_url))
    return segs, target, endlist


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("url", help="master or media m3u8 URL")
    ap.add_argument("--workers", type=int, default=8)
    ap.add_argument("--seconds", type=int, default=60, help="max run time")
    args = ap.parse_args()

    url = args.url
    text = http_get_text(url)
    bw_advertised = None

    if "#EXT-X-STREAM-INF" in text:
        vurl, bw = pick_best_variant(text, url)
        if not vurl:
            raise SystemExit("Could not pick variant from master playlist")
        print(f"Using top variant: {vurl} (BANDWIDTH={bw/1e6:.3f} Mbps)")
        url = vurl
        bw_advertised = bw

    print("Mode: continuous segment fetch loop (re-fetch allowed)")

    bytes_total = 0
    dl_time_total = 0.0
    fetch_count = 0
    playlist_refresh_count = 0
    t_wall0 = time.time()
    recent_fetches = deque()  # (completion_time, bytes_received)

    current_segments = []
    current_target = 6
    next_playlist_refresh = 0.0
    rr_index = 0

    with cf.ThreadPoolExecutor(max_workers=args.workers) as ex:
        while True:
            if time.time() - t_wall0 > args.seconds:
                break

            now = time.time()
            if now >= next_playlist_refresh or not current_segments:
                media = http_get_text(url)
                segs, target, _ = parse_media_playlist(media, url)
                if segs:
                    current_segments = segs
                    current_target = target
                    rr_index = rr_index % len(current_segments)
                playlist_refresh_count += 1
                next_playlist_refresh = now + max(0.5, current_target / 2)
                print(
                    f"{utc_now_iso()} PLAYLIST refresh={playlist_refresh_count} target_s={current_target} segments={len(current_segments)}",
                    flush=True,
                )

            if not current_segments:
                time.sleep(0.25)
                continue

            batch = []
            for _ in range(max(1, args.workers)):
                seg_url = current_segments[rr_index]
                batch.append(seg_url)
                rr_index = (rr_index + 1) % len(current_segments)

            futures = [ex.submit(http_get_bytes, s, 30) for s in batch]
            for fut in cf.as_completed(futures):
                data, dt = fut.result()
                bytes_total += len(data)
                dl_time_total += max(dt, 1e-6)
                fetch_count += 1
                recent_fetches.append((time.time(), len(data)))

            now = time.time()
            cutoff = now - 1.0
            while recent_fetches and recent_fetches[0][0] < cutoff:
                recent_fetches.popleft()
            rolling_1s_bytes = sum(item[1] for item in recent_fetches)
            rolling_1s_mbps = (rolling_1s_bytes * 8) / 1e6

            wall_elapsed = max(time.time() - t_wall0, 1e-6)
            xfer_mbps = (bytes_total * 8) / dl_time_total / 1e6 if dl_time_total > 0 else 0
            wall_mbps = (bytes_total * 8) / wall_elapsed / 1e6
            print(
                f"fetches={fetch_count:5d} roll1s={rolling_1s_mbps:7.3f} Mbps xfer={xfer_mbps:7.3f} Mbps wall={wall_mbps:7.3f} Mbps",
                flush=True,
            )

    print("\nFinal:")
    if bw_advertised:
        print(f"  Advertised top variant: {bw_advertised/1e6:.3f} Mbps")
    print(f"  Playlist refreshes: {playlist_refresh_count}")
    print(f"  Segment fetches: {fetch_count}")
    print(f"  Transfer Mbps (download time only): {(bytes_total*8/max(dl_time_total,1e-6))/1e6:.3f}")
    print(f"  Wall Mbps (includes waiting for live): {(bytes_total*8/max(time.time()-t_wall0,1e-6))/1e6:.3f}")


if __name__ == "__main__":
    main()
