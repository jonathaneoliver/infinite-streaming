# Troubleshooting

Common issues and fixes, grouped by where they surface.

## Startup

### "Ports 30000/30081 already in use"

Another service or a previous container is holding the ports.

```bash
docker ps                              # find any running infinite-streaming container
docker stop infinite-streaming         # stop it
lsof -i :30000                         # or find what else is holding the port
```

If a different app needs port 30000, override with `INFINITE_STREAM_LISTEN_PORT` in `.env` and update any bookmarks.

### Container exits immediately or loops on restart

Check logs:

```bash
docker compose logs -f infinite-streaming
# or
docker logs infinite-streaming
```

Most startup failures are:
- **Missing or unreadable `CONTENT_DIR`** — the path in `.env` must exist and be readable by the container's user.
- **Cert generation failed** — the container tries to auto-generate self-signed certs in `$CONTENT_DIR/certs/`. If the directory isn't writable, startup fails. Either make it writable or drop your own `localhost.pem` and `localhost-key.pem` into `certs/` before starting.
- **nftables/tc capability errors** — on non-Linux hosts (macOS Docker Desktop), the proxy still runs but shaping is disabled. The capabilities endpoint (`/api/nftables/capabilities`) will report `available: false` and the UI disables the shaping controls. This is expected, not a bug.

### `.env` isn't being picked up

`docker compose` reads `.env` from the directory you run it in. Run `docker compose` from the repo root, not from a subdirectory. For Docker-run invocations, pass `--env-file .env` explicitly.

## Playback

### "No content in the Source Library"

The dashboard lists content by scanning `/media/dynamic_content`. If you copied files into `$CONTENT_DIR/originals` but nothing appears:

- The Source Library shows *originals*, not encoded output. Make sure you're on the right tab.
- Hit **Refresh** or reload the page — the scan is lazy.
- Run `POST /api/sources/scan-originals` (or click the Rescan button in the UI) to force re-indexing.
- Permissions: the container user must be able to read the files. If you copied as root with restrictive modes, `chmod -R a+rX $CONTENT_DIR/originals`.

### Manifest returns 404 or empty

- Check `GET /go-live/api/status` — if no worker is listed for your content, the first manifest request should spawn one. If it doesn't, look in the container logs for errors from `go-live`.
- Verify `/media/dynamic_content/{content}/manifest.json` exists and is readable. That file is the source of truth for go-live; if it's missing, the encoder never finished or the content name doesn't match the directory name exactly.

### Segments load but manifests don't update / player stalls after ~1 minute

The per-content worker likely shut down on idle timeout. Refresh the manifest and the worker respawns. If this happens under constant playback, bump the idle timeout config (see `go-live/internal/manager`).

### Self-signed cert warning in the browser

Expected. Accept the warning, or drop your own trusted cert into `$CONTENT_DIR/certs/` as `localhost.pem` + `localhost-key.pem` before first start.

## Testing session / fault injection

### "Session doesn't appear after I open the testing URL"

The URL must include `player_id=<uuid>`:

```
http://localhost:30000/dashboard/testing-session.html?player_id=<uuid>&url=<encoded-stream-url>
```

Without `player_id`, go-proxy can't allocate a session port.

### Faults don't seem to apply

- Open the **Network Log** section on the session card — every proxied request is recorded there with fault metadata. If faulted rows aren't showing up, your player isn't routing through the session proxy port.
- Check the stream URL the player loaded (dev tools → network). It should be on the session port (e.g. `:30281`), not the shared `:30081`. The proxy *redirects* initial requests, but if the player cached an earlier URL it may bypass the redirect — click **Restart Playback**.
- Verify `failure_type` is not `none` and the units/frequency fields make sense for the mode.

### Transport faults (DROP/REJECT) don't work

These require Linux with nftables. On macOS Docker Desktop the proxy container doesn't have the required kernel modules. The capabilities endpoint reports this and the UI greys out the controls.

### Rate shaping has no visible effect

Same requirement — Linux + `tc`. Check `GET /api/nftables/capabilities`.

## k3d deployment

### Pods crash-loop on first deploy

Usually image pull failures:

- If using the local registry (`K3S_REGISTRY`), confirm `~/.config/k3d/smashing-registries.yaml` was written by `make k3d-bootstrap` and contains the registry as a mirror with `insecure_skip_verify: true`. The two clusters created via `make k3d-bootstrap` already pass this file via `--registry-config`; only relevant if you re-create a cluster manually.
- If using GHCR, the image must be public or the cluster must have an image pull secret. Pull manually from a worker to confirm: `kubectl --kubeconfig ~/.config/k3d/smashing-dev-kubeconfig.yaml run pull-test --rm -it --image=ghcr.io/jonathaneoliver/infinite-streaming:latest -- echo ok`.

### Dev and release fight over ports

They can't — they're separate k3d clusters with disjoint host port mappings (release: 30000-30881; dev: 40000-40881). If you see conflicts on the host:
- `k3d cluster list` should show both `dev` and `release` running.
- `docker ps --filter name=k3d-` should show four containers: `k3d-dev-server-0`, `k3d-dev-serverlb`, `k3d-release-server-0`, `k3d-release-serverlb`.
- Anything else listening on those host ports (e.g. an old k3s install) needs to be stopped first.

### Session ports work inside the cluster but not from the browser

The cluster's internal NodePort is stable (`30181-30881` in both clusters). k3d's `--port HOST:NODE@loadbalancer` flag handles the host→cluster remap (e.g. host `:40181` → cluster `:30181` for the dev cluster). If a session port works via `kubectl exec` curl but fails from the browser, the host-side mapping is missing — re-run `make k3d-bootstrap` (idempotent) or check `docker port k3d-{dev,release}-serverlb`.

`go-proxy` separately advertises the *external* host port to clients via `EXTERNAL_PORT_BASE` (set per stack in the Makefile — `40081` for dev, `30081` for release). If the per-session URLs the browser receives have the wrong port, this env var is mismatched between the manifest and the cluster's host-port mapping.

## Encoding

### Encoding job stuck in "queued" forever

The job runner is a single-slot queue. Check `GET /api/jobs` — if another job is `running`, this one waits. If nothing is running and yours still won't start, look at the go-upload logs for the scheduler loop.

### "find: … xargs: No such file or directory" errors in encoding

Fixed in the same PR as cloud encoding. Pull `main` or cherry-pick the `create_abr_ladder.sh` guard for empty `find` output.

### Cloud encoding failures

See [`docs/CLOUD_ENCODING.md`](CLOUD_ENCODING.md#troubleshooting) — common issues (bad PAT, missing subnet egress, spot capacity) are covered there.

## Client apps (iOS / Roku / Android)

### Apps can't reach the server

- Mobile devices must be on the same network segment as the host, and the firewall must allow the ports.
- The apps reach the server by hostname or IP — if you run Docker Compose on `localhost`, that's only reachable from the host itself. Use the host's LAN IP.
- Self-signed certs: the apps need the cert trusted (or configured to accept untrusted in dev). See each client's README.

## Filing an issue

If something isn't covered here, include:

- `docker compose logs infinite-streaming` (or `kubectl logs --kubeconfig ~/.config/k3d/smashing-{dev,release}-kubeconfig.yaml deploy/infinite-streaming` for k3d)
- `GET /api/version` output
- For playback issues: `GET /api/session/{id}/network` for the affected session, and the browser's dev-tools network tab
- For encoding issues: `GET /api/jobs/{job_id}` and the last ~100 lines from the encoder log stream (`/api/jobs/{job_id}/stream`)
