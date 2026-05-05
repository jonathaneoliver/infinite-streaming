# analytics

Sidecar analytics tier for InfiniteStream. Subscribes to the SSE session
stream emitted by `go-proxy`, writes session-state snapshots into
ClickHouse, and exposes them to Grafana (ad-hoc dashboards) and to
`testing.html` (historical replay mode).

The streaming app does not store anything itself — if `forwarder` is
down the live UI keeps working, archival just pauses until it restarts.

## Components

- `go-forwarder/` — standalone Go binary. Subscribes to
  `/api/sessions/stream`, dedupes by snapshot fingerprint, batches
  inserts into ClickHouse. Also serves the read-only HTTP API at
  `:8080` that nginx proxies as `/analytics/api/*`. All ClickHouse
  queries use parameterized `{name:Type}` placeholders — no string
  interpolation of user input.
- `clickhouse/init.d/01-schema.sql` — bootstrap schema. ClickHouse
  applies it on first start. One wide `session_snapshots` table with
  hot fields as typed columns and a `session_json` column for the long
  tail. 30-day TTL.
- `grafana/provisioning/` — datasource + starter dashboard, picked up
  automatically by Grafana.

## Local (Docker Compose)

`make run` brings up the analytics services alongside the main stack:

| Service     | Port  | Notes                                    |
|-------------|-------|------------------------------------------|
| ClickHouse  | 30123 | HTTP interface (native 9000 not exposed) |
| Grafana     | 30300 | Anonymous editor; pre-provisioned        |

Then:

- Grafana dashboard: <http://localhost:30300/d/is-session-analytics>
- Replay a session: <http://localhost:30000/testing.html?replay=1&session=SESSION_ID>

## k3d

`make deploy` and `make deploy-release` apply `k8s-analytics.yaml`
into the matching k3d cluster automatically — one analytics tier per
cluster (each cluster has its own ClickHouse PVC, forwarder, and
Grafana). The forwarder's SSE source is the same in-cluster URL in
both clusters (`http://infinite-streaming:30081/api/sessions/stream`)
because each cluster has exactly one `infinite-streaming` Service at
NodePort 30081.

## Replay mode

`testing.html?replay=1&session=<id>[&from=<rfc3339>][&to=<rfc3339>]`
fetches snapshots from `/analytics/api/snapshots`, then re-feeds them
through the same `applySessionsList` renderer the live stream uses.
`Date.now` is briefly patched per snapshot so chart point timestamps
line up with recorded `ts`.

## Re-provisioning the dashboard

Edits to `grafana/provisioning/dashboards/*.json` are picked up
automatically by the running Grafana container (every 30s). However,
adding/removing files in this directory after `docker compose up`
sometimes leaves the bind mount stale. If a new dashboard or datasource
isn't showing up:

```sh
docker compose up -d --force-recreate grafana
```

## Schema notes

- Hot columns (used by charts and dashboards) are typed: `buffer_depth_s`,
  `network_bitrate_mbps`, `dropped_frames`, `player_state`, etc.
- Everything else lives in `session_json`. Promote a column when a
  query starts using it frequently.
- `ORDER BY (session_id, ts)` makes per-session replay queries cheap.
- `TTL toDateTime(ts) + INTERVAL 30 DAY` enforces retention; tune via
  `ALTER TABLE ... MODIFY TTL`.

## Securing a WAN-exposed deployment

Default docker-compose binds ClickHouse to `127.0.0.1` (host-only) and
keeps Grafana off the host network entirely — Grafana is reachable only
via nginx at `/grafana/`. The dashboard, `/analytics/api/`, and
`/grafana/` routes can be gated with HTTP Basic auth so only known
clients can read or write through nginx. Player apps (Apple, Roku,
AndroidTV) keep working without credentials because their endpoints
(`/api/sessions/stream`, `/api/version`, segment fetches under
`/go-live/`) stay public.

To enable:

```sh
# 1. Generate an htpasswd file (bcrypt; pick your own user/pass)
docker run --rm httpd:alpine htpasswd -nbB myuser mypassword > ./htpasswd

# 2. Mount it into the go-server container and tell launch.sh where it is
#    (docker-compose.override.yml or similar):
services:
  go-server:
    environment:
      - INFINITE_STREAM_AUTH_HTPASSWD=/etc/nginx/auth.htpasswd
    volumes:
      - ./htpasswd:/etc/nginx/auth.htpasswd:ro
```

When `INFINITE_STREAM_AUTH_HTPASSWD` is unset (the default), auth is
disabled — same behaviour as before. With it set, the dashboard pages,
`/analytics/api/*`, and `/grafana/*` all return 401 without credentials.

### k3d deployment (lenovo)

Both clusters use the same Deployment name (`infinite-streaming`); pick the right kubeconfig per cluster.

```sh
# Pick a cluster (dev or release)
export KUBECONFIG=~/.config/k3d/smashing-release-kubeconfig.yaml
# (or smashing-dev-kubeconfig.yaml for the dev cluster)

# Generate htpasswd
docker run --rm httpd:alpine htpasswd -nbB myuser 'mypass' > /tmp/htpasswd

# Create a Secret holding the file (per-cluster)
kubectl create secret generic infinite-streaming-auth \
  --from-file=htpasswd=/tmp/htpasswd

# Patch the deployment (same name in both clusters)
kubectl patch deployment infinite-streaming --patch '
spec:
  template:
    spec:
      containers:
      - name: go-server
        env:
        - name: INFINITE_STREAM_AUTH_HTPASSWD
          value: /etc/secret/htpasswd
        volumeMounts:
        - name: auth-secret
          mountPath: /etc/secret
          readOnly: true
      volumes:
      - name: auth-secret
        secret:
          secretName: infinite-streaming-auth
'

# Verify
kubectl rollout status deployment/infinite-streaming
curl -s -o /dev/null -w '%{http_code}\n' http://lenovo.local:30000/dashboard/dashboard.html
# → 401
curl -s -o /dev/null -w '%{http_code}\n' -u myuser:mypass http://lenovo.local:30000/dashboard/dashboard.html
# → 200
```

Rotate the password by re-creating the secret and restarting the rollout (in the same cluster context):

```sh
docker run --rm httpd:alpine htpasswd -nbB myuser 'newpass' > /tmp/htpasswd
kubectl create secret generic infinite-streaming-auth --from-file=htpasswd=/tmp/htpasswd \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl rollout restart deployment/infinite-streaming
```

Disable auth without removing the secret by setting the env var to an
empty string:

```sh
kubectl set env deployment/infinite-streaming INFINITE_STREAM_AUTH_HTPASSWD=
```
