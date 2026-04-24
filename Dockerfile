
FROM golang:1.26-alpine AS go-builder
RUN apk add git
WORKDIR /build
COPY go-live /build/go-live
RUN cd /build/go-live && \
    go build -o /out/go-live cmd/server/main.go

COPY go-upload /build/go-upload
RUN cd /build/go-upload && \
    go mod download && \
    go build -o /out/go-upload ./cmd/server

ARG VERSION=unknown
COPY go-proxy /build/go-proxy
RUN cd /build/go-proxy && \
    go build -ldflags "-X main.versionString=${VERSION}" -o /out/go-proxy cmd/server/main.go

FROM alpine:3.23

# Install dependencies first (expensive, rarely changes - gets cached)
RUN \
  apk update && \
  apk add iproute2 iperf nftables openssl && \
  apk add nginx && \
  apk add ffmpeg && \
  apk add python3 py3-pip && \
  apk add libxml2-dev libxslt-dev python3-dev gcc musl-dev bash && \
  apk add ttf-dejavu curl gettext && \
  apk add sqlite ripgrep && \
  rm -f /etc/nginx/conf.d/default.conf && \
  mkdir -p /run/nginx && \
  pip3 install --break-system-packages m3u8==6.0.0 && \
  pip3 install --break-system-packages watchdog && \
  pip3 install --break-system-packages lxml && \
  pip3 install --break-system-packages fastapi==0.109.0 && \
  pip3 install --break-system-packages uvicorn==0.27.0 && \
  pip3 install --break-system-packages python-multipart==0.0.6 && \
  pip3 install --break-system-packages aiofiles==23.2.1 && \
  pip3 install --break-system-packages websockets==12.0 && \
  apk del gcc python3-dev musl-dev && \
  rm -rf /var/cache/apk/*

# Dev-only utilities (keep in a dedicated directory for visibility)
RUN mkdir -p /opt/dev-tools && \
    ln -sf /usr/bin/sqlite3 /opt/dev-tools/sqlite3 && \
    ln -sf /usr/bin/rg /opt/dev-tools/rg

# Download and install Shaka Packager
ARG TARGETARCH
RUN set -eux; \
    case "${TARGETARCH}" in \
      arm64) pkg="packager-linux-arm64" ;; \
      amd64) pkg="packager-linux-x64" ;; \
      *) echo "Unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -L "https://github.com/shaka-project/shaka-packager/releases/download/v3.4.2/${pkg}" \
      -o /usr/local/bin/packager; \
    chmod +x /usr/local/bin/packager


# Configure nginx to log to stderr for interleaved Docker logs
RUN sed -i 's|error_log /var/log/nginx/error.log warn;|error_log stderr warn;|g' /etc/nginx/nginx.conf

# Create directories for uploads, data, and source storage
RUN mkdir -p /tmp/uploads /data /data/sources && \
    chmod 777 /tmp/uploads /data /data/sources

# Copy config and scripts (rarely change)
COPY docker/mime.types /etc/nginx/mime.types
COPY docker/nginx-content.conf.template /etc/nginx/http.d/
COPY docker/launch.sh /sbin/
COPY generate_abr/parse_fmp4_fragments.py /sbin/
COPY --from=go-builder /out/go-live /usr/local/bin/go-live
COPY --from=go-builder /out/go-upload /usr/local/bin/go-upload
COPY --from=go-builder /out/go-proxy /usr/local/bin/go-proxy

# Copy generate_abr tools into container and make writable for encoding output
RUN mkdir -p /generate_abr
COPY generate_abr/create_abr_ladder.sh \
     generate_abr/create_hls_manifests.py \
     generate_abr/convert_to_segmentlist.py \
     /generate_abr/
RUN chmod +x /generate_abr/*.sh /generate_abr/*.py 2>/dev/null || true
RUN chmod -R 777 /generate_abr

# Copy HTML files last (changes frequently during development)
COPY content/*.css /content/
COPY content/*.html /content/
COPY content/shared/ /content/shared/
COPY content/dashboard/ /content/dashboard/
COPY content/testing/ /content/testing/

ENTRYPOINT ["/sbin/launch.sh"]
