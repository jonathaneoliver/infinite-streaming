#!/bin/sh
#
# launch.sh
#
# Nginx listens on INFINITE_STREAM_LISTEN_PORT (or legacy vars), default 30000.


serverport="${INFINITE_STREAM_LISTEN_PORT:-${INFINITE_LISTEN_PORT:-${BOSS_LISTEN_PORT:-30000}}}"

# Generate nginx config from template with environment variable substitution
export INFINITE_STREAM_OUTPUT_DIR="${INFINITE_STREAM_OUTPUT_DIR:-${INFINITE_OUTPUT_DIR:-${BOSS_OUTPUT_DIR}}}"
export INFINITE_STREAM_PROXY_HOST="${INFINITE_STREAM_PROXY_HOST:-127.0.0.1}"
envsubst '${INFINITE_STREAM_OUTPUT_DIR} ${INFINITE_STREAM_PROXY_HOST}' < /etc/nginx/http.d/nginx-content.conf.template > /etc/nginx/http.d/nginx-content.conf

# Start background processes and nginx
# All processes now log to stdout/stderr for proper Docker log interleaving
( echo "Go mode." ) && \
( /usr/local/bin/go-upload & ) && \
( /usr/local/bin/go-live & ) && \
( /usr/local/bin/go-proxy & ) && \
( echo "Go upload service handles /api/*;" ) && \
( echo "Go proxy service handles /api/session*, /api/nftables*;" ) && \
update-nginx-config.sh "$serverport" && \
nginx -g 'daemon off;'
