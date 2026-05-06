FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOFLAGS=-trimpath go build -o /out/forwarder .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/forwarder /forwarder
# Default LLM_PROFILES_PATH points to /config/llm_profiles.yaml. Mount
# a ConfigMap / bind-mount over /config to override per environment
# without rebuilding the image.
COPY config/ /config/
USER nonroot:nonroot
ENTRYPOINT ["/forwarder"]
