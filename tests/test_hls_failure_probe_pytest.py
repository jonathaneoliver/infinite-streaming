import os
import shlex
import subprocess
import sys

import pytest


def build_args():
    args = [sys.executable, os.path.join(os.path.dirname(__file__), "hls_failure_probe.py")]
    url = os.getenv("HLS_FAILURE_PROBE_URL")
    if url:
        args.append(url)
    opts = os.getenv("HLS_FAILURE_PROBE_OPTS", "")
    if opts:
        args.extend(shlex.split(opts))
    return args


@pytest.mark.integration
def test_hls_failure_probe():
    if os.getenv("HLS_FAILURE_PROBE_RUN") != "1":
        pytest.skip("Set HLS_FAILURE_PROBE_RUN=1 to enable the live failure probe")

    args = build_args()
    result = subprocess.run(args, check=False, capture_output=True, text=True)
    if result.returncode != 0:
        print("STDOUT:\n" + result.stdout)
        print("STDERR:\n" + result.stderr)
    assert result.returncode == 0