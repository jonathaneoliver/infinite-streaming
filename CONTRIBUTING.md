# Contributing

Thanks for your interest in contributing to **InfiniteStream**.

## License and attribution

By contributing, you agree that your contributions are licensed under the terms of the `LICENSE` file in this repository. Attribution to **Jonathan Oliver** must be preserved in any redistribution.

## How to contribute

1. Fork the repo (public forks are allowed with attribution).
2. Create a feature branch (`feature/<short-description>` or `fix/<short-description>`).
3. Keep changes focused and well-described — one concern per PR.
4. Open a pull request with a concise summary and testing notes.

## Getting oriented

Before starting:

- Read [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the service topology.
- Read [`PRD.md`](PRD.md) — product behavior source of truth. If you're changing user-facing behavior, align with PRD or update it as part of the PR.
- Skim [`CLAUDE.md`](CLAUDE.md) for repo-specific conventions (also used by AI coding assistants).

## Development loop

### Build and run

Everything runs inside one Docker container:

```bash
make build                # build the image (no-cache)
make run                  # docker compose up -d
make shell                # shell into the running container
docker compose logs -f    # follow logs
make stop                 # tear down
```

UI: `http://localhost:30000/`.

### Iterating on a single Go service

Each Go service lives under its own directory (`go-live/`, `go-upload/`, `go-proxy/`). For tight iteration:

```bash
# build and test one service locally
cd go-live
go build ./...
go test ./...
```

Running a service outside the container is possible but needs careful env setup — the media volume layout and nginx upstreams have to be reachable. For most work, rebuilding the full container (`make build && make run`) is faster to reason about.

### Iterating on the dashboard

UI is static files under `content/dashboard/`. Edits take effect on page reload — no rebuild needed if you've mounted `content/` as a bind mount, or after a `docker compose restart` otherwise.

### Iterating on nginx config

`nginx-content.conf.template` is rendered at container start by `launch.sh` via `envsubst`. Edit and rebuild.

### Iterating on the encoding pipeline

Bash + Python under `generate_abr/`. You can run `create_abr_ladder.sh` directly on the host if ffmpeg + shaka-packager are installed, or run it inside the container with `make shell`.

See [`docs/CLOUD_ENCODING.md`](docs/CLOUD_ENCODING.md) for offloading encodes to AWS.

## Testing

Integration tests live in [`tests/integration/`](tests/integration/) and require a running server.

```bash
cd tests/integration
pip install -r requirements.txt

# fastest: smoke only
pytest -m smoke

# all markers
pytest

# by category
pytest -m http
pytest -m "not slow"
pytest -k test_name
```

Key markers: `smoke`, `http`, `segment`, `manifest`, `transport`, `abrchar`.

Default target is `$K3S_HOST:30000/30081`. Override with `--host`, `--api-port`, `--hls-port`, or `--api-base`.

Player characterization (ABR ramp sweeps):

```bash
pytest test_player_characterization_pytest.py -m abrchar -v \
  --host localhost --scheme http --api-port 30000 --hls-port 30081
```

See [`tests/integration/README.md`](tests/integration/README.md) and [`tests/integration/PLAYER_CHARACTERIZATION_PYTEST.md`](tests/integration/PLAYER_CHARACTERIZATION_PYTEST.md) for the full guide.

## Code style

- **Go**: standard `gofmt`; no extra tooling required. Run `go vet ./...` before submitting.
- **Shell**: POSIX-friendly where practical. Use `set -euo pipefail` at the top of new scripts.
- **JavaScript / CSS / HTML**: keep changes explicit and readable. No build step — scripts are loaded directly, so stick to ES2020+ and browser-native features.
- **Python** (encoding helpers only): format with `ruff` if available, otherwise leave existing style alone.

## PR conventions

- Short, descriptive title (`feat:`, `fix:`, `docs:`, `refactor:`, `chore:`).
- Describe the **why** in the body, not just the what. The diff shows the what.
- Include test notes: what you ran, what you verified, any manual steps.
- Screenshots for UI-visible changes.
- One concern per PR — split large refactors from behavior changes.

When creating issues/PRs/comments with `gh`, pass the body via heredoc or `--body-file`. Don't embed `\n` in quoted strings — `gh` won't unescape them.

## Releases

- Tag releases as `vX.Y.Z` (immutable) in addition to `:latest`.
- Build and push images under that tag: `make build-push-k3s K3S_SERVER_IMAGE=$K3S_REGISTRY/infinite-streaming:vX.Y.Z`.
- Update the `VERSION` file.

## Reporting issues

Include the diagnostic info listed at the bottom of [`docs/TROUBLESHOOTING.md`](docs/TROUBLESHOOTING.md):

- `docker compose logs infinite-streaming` (or `kubectl logs`)
- `GET /api/version`
- For playback: `/api/session/{id}/network`
- For encoding: `/api/jobs/{job_id}` and a tail of the encoder log stream
