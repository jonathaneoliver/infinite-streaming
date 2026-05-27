FROM golang:1.25-alpine AS build
WORKDIR /src
# Build context is the repo root (set in docker-compose.yml). Copy
# both modules into the workspace so the forwarder's `replace
# github.com/.../go-proxy => ../../go-proxy` resolves to a real path.
# go.work joins the two modules; copying it lets the workspace
# resolver inside the build container behave the same as on the dev
# host — `go build` then doesn't need to hit GOPROXY for the local
# go-proxy package.
COPY go.work ./
COPY go-proxy/ ./go-proxy/
COPY analytics/go-forwarder/ ./analytics/go-forwarder/
# Drop ./tools/harness-cli from the workspace — that module is in the
# dev host's go.work for editor convenience but isn't part of the
# forwarder build. Skipping the COPY (instead of pulling the whole
# tools/ tree into the image) keeps the build context tight.
RUN go work edit -dropuse ./tools/harness-cli || true
WORKDIR /src/analytics/go-forwarder
RUN CGO_ENABLED=0 GOFLAGS=-trimpath go build -o /out/forwarder .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/forwarder /forwarder
USER nonroot:nonroot
ENTRYPOINT ["/forwarder"]
