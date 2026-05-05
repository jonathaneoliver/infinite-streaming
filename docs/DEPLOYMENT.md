# Deployment

For the standard development / local-use path, Docker Compose (covered in the [README Quick Start](../README.md#quick-start)) is all you need. This page covers the less common paths: running in two side-by-side **k3d** clusters (release + dev), pinning immutable release tags, and publishing images from your own fork.

## k3d: release and dev as separate clusters

Two independent k3d clusters on the same host — separate kubeconfigs, separate Service namespaces, separate ClickHouse PVCs, separate Grafana state. Stack identity is the cluster context, not a name suffix; one stray `kubectl apply` can't hit the wrong stack.

| Stack | UI | Session ports | API | Image tag | Cluster | `make` target |
|---|---|---|---|---|---|---|
| Release | `$K3S_HOST:30000` | 30081 + 30181–30881 | `:6544` | `:latest` or pinned | `release` | `make deploy-release` |
| Dev | `$K3S_HOST:40000` | 40081 + 40181–40881 | `:6543` | `:dev` | `dev` | `make deploy` |

Both clusters share the host's local Docker registry (`$K3S_REGISTRY`, HTTP-only — wired via `--registry-config`), and mount the host's `$K3S_MEDIA_DIR` and `$K3S_CERTS_DIR` paths via `--volume`. Per-cluster kubeconfigs are written to `~/.config/k3d/smashing-{dev,release}-kubeconfig.yaml` on the remote host.

### Configure `.env`

```
K3S_HOST=<cluster-host>                # bare hostname, used in announce URLs
K3S_SSH_HOST=user@<cluster-host>       # SSH target for kubectl
K3S_REGISTRY=<registry-host:port>      # local Docker registry, HTTP
K3S_MEDIA_DIR=<host path mounted as /media in pods>
K3S_CERTS_DIR=<host path with localhost.pem/-key.pem>
```

### Bootstrap the clusters (once)

```bash
make k3d-bootstrap
```

This installs k3d into `~/.local/bin` on `$K3S_SSH_HOST` (no sudo), writes `~/.config/k3d/smashing-registries.yaml` so both clusters can pull from the local HTTP registry, then creates the two clusters with the right host-port mappings, volume mounts, and per-cluster kubeconfigs. Idempotent — re-running is a no-op once both clusters exist.

### Deploy

```bash
# dev cluster: builds + pushes :dev, applies main app + analytics tier
make deploy

# release cluster: builds + pushes the release tag, applies main app + analytics tier
make deploy-release
```

Each `make deploy[-release]` invocation also builds and pushes the analytics forwarder image, applies `k8s-analytics.yaml` (ClickHouse + forwarder + Grafana with inlined schema and Grafana provisioning), and waits for rollout. The single `k8s-infinite-streaming.yaml.tmpl` template renders identically into both clusters; per-stack identity comes from envsubst placeholders (`SERVER_ID`, `ANNOUNCE_URL`, `EXTERNAL_PORT_BASE`) bound by the `make` target.

### Tear down a single cluster

```bash
make teardown-dev       # k3d cluster delete dev
make teardown-release   # k3d cluster delete release
```

Wipes the cluster entirely (analytics + main app + PVC + node containers) so a subsequent `make deploy[-release]` starts clean. The other cluster is untouched.

## Release tagging

Use an immutable tag (for example `v1.2.3` or a commit SHA) for releases. Keep `:latest` as a moving pointer.

Suggested flow:

1. Tag the commit and push:
   ```bash
   git tag v1.2.3 && git push origin v1.2.3
   ```
2. The `docker-publish` GitHub Actions workflow publishes to GHCR on tag pushes:
   - `ghcr.io/<owner>/infinite-streaming:v1.2.3`
   - `ghcr.io/<owner>/infinite-streaming-forwarder:v1.2.3`
3. Deploy the release cluster pinned to that tag:
   ```bash
   make deploy-release K3S_SERVER_IMAGE=$K3S_REGISTRY/infinite-streaming:v1.2.3
   ```

`:latest` continues to point at the most recent `main` build; `:v1.2.3` is immutable.

## GHCR publishing (for forks)

The `docker-publish` GitHub Actions workflow builds and publishes both `infinite-streaming` (main image) and `infinite-streaming-forwarder` (analytics SSE→ClickHouse forwarder) to GHCR on every push to `main` and on tag pushes. Tags produced per image:

- `ghcr.io/<owner>/infinite-streaming:latest` (and `…-forwarder:latest`) — main branch HEAD
- `ghcr.io/<owner>/infinite-streaming:main`, `:sha-<short>` — branch + sha tags
- `ghcr.io/<owner>/infinite-streaming:v1.2.3`, `:v1.2`, `:v1` — semver tags from `v*` git tags

To enable in your fork:

1. Set the default branch to `main`.
2. **Settings → Actions → General** → allow GitHub Actions to write packages.

Optional Docker Hub mirror: set `vars.DOCKERHUB_NAMESPACE` (e.g. `myuser`) plus `secrets.DOCKERHUB_USERNAME` and `secrets.DOCKERHUB_TOKEN` and the workflow also pushes to Docker Hub. When the namespace is unset (the default for new forks) the Docker Hub steps are skipped entirely.

Consumption (pulling pre-built images) is documented in the [README](../README.md#pre-built-images-from-ghcr-no-source-checkout).

## Cloud encoding

For offloading ABR ladder encoding to AWS EC2 spot instances, see [`CLOUD_ENCODING.md`](CLOUD_ENCODING.md).

## Troubleshooting

Deployment-specific issues (image pull failures, port conflicts, NodePort mapping mismatches) are covered in [`TROUBLESHOOTING.md`](TROUBLESHOOTING.md#k3d-deployment).
