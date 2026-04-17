#!/bin/sh
#
# launch.sh
#
# Nginx listens on INFINITE_STREAM_LISTEN_PORT (or legacy vars), default 30000.


serverport="${INFINITE_STREAM_LISTEN_PORT:-${INFINITE_LISTEN_PORT:-${BOSS_LISTEN_PORT:-30000}}}"

# Generate nginx config from template with environment variable substitution
export INFINITE_STREAM_OUTPUT_DIR="${INFINITE_STREAM_OUTPUT_DIR:-${INFINITE_OUTPUT_DIR:-/media/dynamic_content}}"

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
