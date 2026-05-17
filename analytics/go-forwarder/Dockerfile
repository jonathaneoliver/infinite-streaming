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
WORKDIR /src/analytics/go-forwarder
RUN CGO_ENABLED=0 GOFLAGS=-trimpath go build -o /out/forwarder .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/forwarder /forwarder
USER nonroot:nonroot
ENTRYPOINT ["/forwarder"]
