# Deployment

For the standard development / local-use path, Docker Compose (covered in the [README Quick Start](../README.md#quick-start)) is all you need. This page covers the less common paths: running in k3s, side-by-side release and dev stacks, pinning release tags, and publishing images from your own fork.

## k3s: release and dev side by side

Two manifests, two NodePort ranges, two image tags — so you can run a stable release stack and an active dev stack on the same cluster without conflicts.

| Stack | UI | Session ports | Image tag | Manifest | `make` target |
|---|---|---|---|---|---|
| Release | `$K3S_HOST:30000` | 30081 (base) + 30181–30881 | `:latest` or pinned | `k8s-infinite-streaming.yaml` | `make deploy-release` |
| Dev | `$K3S_HOST:40000` | 40081 (base) + 40181–40881 | `:dev` | `k8s-infinite-streaming-dev.yaml` | `make deploy` |

Configure `.env` with cluster details before deploying:

```
K3S_HOST=<cluster-host>
K3S_SSH_HOST=user@<cluster-host>
K3S_REGISTRY=<registry-host:port>      # if using a local registry
K3S_KUBECONFIG=<path on K3S_SSH_HOST>
```

Deploy:

```bash
# dev stack
make deploy

# release stack (uses K3S_SERVER_IMAGE / K3S_PROXY_IMAGE)
make deploy-release
```

go-proxy maps external NodePorts to internal container ports using `EXTERNAL_PORT_BASE`, `INTERNAL_PORT_BASE`, and `PORT_RANGE_COUNT` env vars in the manifest — that's how release and dev coexist.

## Release tagging

Use an immutable tag (for example `v1.2.3` or a commit SHA) for releases. Keep `:latest` as a moving pointer.

Suggested flow:

1. Tag the commit and push:
   ```bash
   git tag v1.2.3 && git push origin v1.2.3
   ```
2. Build and push images with that tag:
   ```bash
   make build-push-k3s K3S_SERVER_IMAGE=$K3S_REGISTRY/infinite-streaming:v1.2.3
   ```
3. Deploy the release pinned to that tag:
   ```bash
   make deploy-release K3S_SERVER_IMAGE=$K3S_REGISTRY/infinite-streaming:v1.2.3
   ```

## GHCR publishing (for forks)

A GitHub Actions workflow builds and publishes to GHCR on pushes to `main`. Tags produced:

- `ghcr.io/<owner>/infinite-streaming:latest`
- `ghcr.io/<owner>/infinite-streaming:main`
- `ghcr.io/<owner>/infinite-streaming:sha-<short>`

To enable in your fork:

1. Set the default branch to `main`.
2. **Settings → Actions → General** → allow GitHub Actions to write packages.

Consumption (pulling pre-built images) is documented in the [README](../README.md#advanced-deployment).

## Cloud encoding

For offloading ABR ladder encoding to AWS EC2 spot instances, see [`CLOUD_ENCODING.md`](CLOUD_ENCODING.md).

## Troubleshooting

Deployment-specific issues (image pull failures, port conflicts, NodePort mapping mismatches) are covered in [`TROUBLESHOOTING.md`](TROUBLESHOOTING.md#k3s-deployment).
