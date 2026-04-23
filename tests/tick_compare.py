#!/usr/bin/env python3
"""
Compare tick generation timings between ll-live (Python) and go-live.

Usage:
  python3 tools/tick_compare.py [seconds]
"""

import re
import subprocess
import sys
import time


GO_RE = re.compile(r"\[GO-LIVE\] Tick generation: ([0-9.]+)s avg_5m=([0-9.]+)s")
PY_RE = re.compile(r"\[LL-LIVE\] Tick generation: ([0-9.]+)s avg_5m=([0-9.]+)s")


def tail_logs(service, seconds):
    cmd = ["docker", "compose", "logs", "--since", f"{seconds}s", service]
    proc = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    return proc.stdout.splitlines()


def parse(lines, regex):
    samples = []
    for line in lines:
        match = regex.search(line)
        if match:
            samples.append((float(match.group(1)), float(match.group(2))))
    return samples


def summarize(label, samples):
    if not samples:
        return f"{label}: no samples"
    durations = [s[0] for s in samples]
    avg = sum(durations) / len(durations)
    return f"{label}: samples={len(samples)} avg_tick={avg:.4f}s last_avg5m={samples[-1][1]:.4f}s"


def main():
    seconds = 60
    if len(sys.argv) > 1:
        seconds = int(sys.argv[1])

    time.sleep(1)
    go_lines = tail_logs("go-live", seconds)
    py_lines = tail_logs("ism1", seconds)

    go_samples = parse(go_lines, GO_RE)
    py_samples = parse(py_lines, PY_RE)

    print(f"Window: {seconds}s")
    print(summarize("GO-LIVE", go_samples))
    print(summarize("LL-LIVE", py_samples))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
