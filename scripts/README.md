# scripts/

Utility scripts for the repo. Currently: screenshot capture for the README and `docs/`.

## capture-screenshots.py

Drives a real system **Google Chrome** via Playwright (headful, H.264 decoders) and captures the dashboard pages referenced from the README. Writes to `../screenshots/*.png`, overwriting existing files.

### One-time setup

```bash
python -m venv /tmp/ism-shot-venv
/tmp/ism-shot-venv/bin/pip install -r requirements.txt
/tmp/ism-shot-venv/bin/playwright install chrome
```

(You can reuse an existing venv — Playwright just needs `playwright>=1.40` installed and the Chrome channel provisioned.)

### Run

The script uses your local Google Chrome to capture *remote* screenshots — you don't need to run it on the server host.

```bash
# against the default localhost:30000
/tmp/ism-shot-venv/bin/python capture-screenshots.py

# against a specific dev server
/tmp/ism-shot-venv/bin/python capture-screenshots.py \
  --base-url=http://jonathanoliver-ubuntu.local:22000

# just one page (for iteration)
/tmp/ism-shot-venv/bin/python capture-screenshots.py --only mosaic
```

Or via the top-level Makefile:

```bash
make screenshots SCREENSHOT_HOST=http://jonathanoliver-ubuntu.local:22000
```

### What it captures

- `dashboard`, `upload-content`, `source-library`, `encoding-jobs` — static pages
- `playback`, `mosaic`, `live-offset` — pages with active video; the script waits for at least one `<video>` to reach `readyState>=2` before capturing so frames are rendered
- `testing-session` — requires an **active testing session**. The script calls `/api/sessions`, picks the first active session, builds the testing-session URL from its `player_id` + manifest URL, waits for the Bitrate chart to accumulate ≥3 data points, then captures. If no session is active, this one is skipped with a warning.

### Prerequisites for meaningful captures

- Server running and reachable from the Mac.
- At least one content item encoded (otherwise Mosaic / Playback will be empty).
- At least one testing session active with some fault or shaping applied (otherwise `testing-session.png` is skipped).

### Regenerating just one page

Pass `--only <name>` to capture a single page without redoing the rest:

```bash
/tmp/ism-shot-venv/bin/python capture-screenshots.py --only testing-session \
  --base-url=http://jonathanoliver-ubuntu.local:22000
```

The valid names are: `dashboard`, `upload-content`, `source-library`, `encoding-jobs`, `playback`, `mosaic`, `live-offset`, `testing-session`.
