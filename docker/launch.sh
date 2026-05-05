#!/bin/sh
#
# launch.sh
#
# Nginx listens on INFINITE_STREAM_LISTEN_PORT (or legacy vars), default 30000.


export INFINITE_STREAM_LISTEN_PORT="${INFINITE_STREAM_LISTEN_PORT:-${INFINITE_LISTEN_PORT:-${ISM_LISTEN_PORT:-30000}}}"

# Generate nginx config from template with environment variable substitution
export INFINITE_STREAM_OUTPUT_DIR="${INFINITE_STREAM_OUTPUT_DIR:-${INFINITE_OUTPUT_DIR:-/media/dynamic_content}}"

# Pre-create /media subdirectories the services log into. In docker-compose
# the `init-permissions` sidecar handles this; in k3s there's no
# equivalent, so a fresh PVC starts empty and nginx + the tee pipelines
# below blow up on missing /media/logs. Same pattern as #343 / #372 which
# pre-created SQLite parent dirs in the go services. Idempotent: `-p` is a
# no-op when the directory already exists.
mkdir -p /media/logs /media/data "$INFINITE_STREAM_OUTPUT_DIR"

# Auto-generate self-signed TLS certs if missing
certdir="/media/certs"
if [ -f "$certdir/localhost.pem" ] && [ -f "$certdir/localhost-key.pem" ]; then
  echo "Using existing TLS certificates from $certdir"
else
  mkdir -p "$certdir"
  openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
    -keyout "$certdir/localhost-key.pem" \
    -out "$certdir/localhost.pem" \
    -subj "/CN=localhost" 2>/dev/null
  echo "Auto-generated self-signed TLS certificates in $certdir"
fi
# Ensure nginx can find the certs
mkdir -p /etc/nginx/certs
ln -sf "$certdir/localhost.pem" /etc/nginx/certs/localhost.pem
ln -sf "$certdir/localhost-key.pem" /etc/nginx/certs/localhost-key.pem
export INFINITE_STREAM_PROXY_HOST="${INFINITE_STREAM_PROXY_HOST:-127.0.0.1}"

# Analytics sidecar (forwarder + Grafana) is OPT-IN. Compose sets
# INFINITE_STREAM_ENABLE_ANALYTICS=1 so /analytics/api/* and /grafana/*
# route to the sibling services. k3s deployments leave it off until
# k8s-analytics.yaml is also applied — the main image must boot
# regardless (per CLAUDE.md: "the live streaming path is independent
# of the sidecar"). When off, the analytics location blocks aren't
# rendered, and /analytics/api/* + /grafana/* simply 404.
export INFINITE_STREAM_ENABLE_ANALYTICS="${INFINITE_STREAM_ENABLE_ANALYTICS:-0}"
export INFINITE_STREAM_FORWARDER_HOST="${INFINITE_STREAM_FORWARDER_HOST:-forwarder}"
export INFINITE_STREAM_GRAFANA_HOST="${INFINITE_STREAM_GRAFANA_HOST:-grafana}"

# DNS resolver for runtime upstream resolution in the analytics
# fragment. nginx's `resolver` directive doesn't read /etc/resolv.conf
# automatically — extract the first nameserver from the OS resolver
# config so this works in both docker-compose (Docker embedded DNS at
# 127.0.0.11) and k3s (CoreDNS at the cluster's nameserver IP).
INFINITE_STREAM_DNS_RESOLVER="$(awk '/^nameserver/ { print $2; exit }' /etc/resolv.conf 2>/dev/null)"
export INFINITE_STREAM_DNS_RESOLVER="${INFINITE_STREAM_DNS_RESOLVER:-127.0.0.11}"

# Opt-in basic-auth on dashboard pages, /analytics/api/, and /grafana/.
# When INFINITE_STREAM_AUTH_HTPASSWD points at a readable htpasswd file,
# nginx requires Basic auth on those routes. HLS playback URLs and the
# live /api/* endpoints used by player apps are unaffected so unattended
# Apple/Roku/AndroidTV clients keep working without credentials.
# When unset (default), auth is disabled — same behaviour as before.
if [ -n "$INFINITE_STREAM_AUTH_HTPASSWD" ] && [ -r "$INFINITE_STREAM_AUTH_HTPASSWD" ]; then
  cp "$INFINITE_STREAM_AUTH_HTPASSWD" /etc/nginx/htpasswd
  # 0644 — nginx workers may run as a non-root user and need read.
  # The hashed passwords inside are bcrypt/sha-crypt anyway; leaking
  # this file is a brute-force opportunity, not a credential dump.
  chmod 0644 /etc/nginx/htpasswd
  export INFINITE_STREAM_AUTH_DIRECTIVES="auth_basic \"InfiniteStream\"; auth_basic_user_file /etc/nginx/htpasswd;"
  echo "Basic auth ENABLED for dashboard / analytics / grafana routes."
else
  export INFINITE_STREAM_AUTH_DIRECTIVES="auth_basic off;"
  echo "Basic auth disabled (set INFINITE_STREAM_AUTH_HTPASSWD to a htpasswd file path to enable)."
fi
envsubst '${INFINITE_STREAM_OUTPUT_DIR} ${INFINITE_STREAM_PROXY_HOST} ${INFINITE_STREAM_LISTEN_PORT} ${INFINITE_STREAM_AUTH_DIRECTIVES}' < /etc/nginx/http.d/nginx-content.conf.template > /etc/nginx/http.d/nginx-content.conf

# Analytics fragment: emitted only when the sidecar is enabled.
# /etc/nginx/snippets/ is glob-included from inside the main config's
# server block, so a missing file is a clean no-op (locations 404).
mkdir -p /etc/nginx/snippets
rm -f /etc/nginx/snippets/analytics-locations.conf
if [ "$INFINITE_STREAM_ENABLE_ANALYTICS" = "1" ]; then
  envsubst '${INFINITE_STREAM_DNS_RESOLVER} ${INFINITE_STREAM_FORWARDER_HOST} ${INFINITE_STREAM_GRAFANA_HOST} ${INFINITE_STREAM_AUTH_DIRECTIVES}' \
    < /etc/nginx/snippets/nginx-analytics.conf.template \
    > /etc/nginx/snippets/analytics-locations.conf
  echo "Analytics ENABLED — /analytics/api/* → $INFINITE_STREAM_FORWARDER_HOST:8080, /grafana/* → $INFINITE_STREAM_GRAFANA_HOST:3000 (resolver=$INFINITE_STREAM_DNS_RESOLVER)."
else
  echo "Analytics disabled (set INFINITE_STREAM_ENABLE_ANALYTICS=1 to wire /analytics/api/* and /grafana/* to a forwarder + Grafana sidecar)."
fi

# Mirror service stdout/stderr to /media/logs/ so logs are inspectable from
# the host (tail -f, rsync, attach to bug bundle) without losing the docker
# log stream. See #377.
#
# /media/logs is pre-created by the `init-permissions` compose service
# (which all three services consuming /media bind-mounts depend on) so
# this script doesn't have to mkdir or chmod across unrelated services.
#
# Each backend's combined output is piped through `tee -a` to /media/logs/<svc>.log
# AND to the container's stdout, so `docker logs` continues to interleave all
# three services normally. The pipeline runs in a subshell backgrounded with `&`
# so the parent shell proceeds to exec nginx as PID 1.
( echo "Go mode." ) && \
( /usr/local/bin/go-upload 2>&1 | tee -a /media/logs/go-upload.log & ) && \
( /usr/local/bin/go-live   2>&1 | tee -a /media/logs/go-live.log   & ) && \
( /usr/local/bin/go-proxy  2>&1 | tee -a /media/logs/go-proxy.log  & ) && \
( echo "Go upload service handles /api/*;" ) && \
( echo "Go proxy service handles /api/session*, /api/nftables*;" ) && \
nginx -g 'daemon off;'
