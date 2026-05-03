FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 GOFLAGS=-trimpath go build -o /out/forwarder .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/forwarder /forwarder
USER nonroot:nonroot
ENTRYPOINT ["/forwarder"]
