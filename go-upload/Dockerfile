FROM golang:1.21-alpine as build
WORKDIR /app/go-upload
COPY go-upload/go.mod go-upload/go.sum ./
RUN go mod download
COPY go-upload ./
RUN go build -o go-upload ./cmd/server

FROM alpine:3.18
WORKDIR /app
RUN apk add --no-cache bash python3 ffmpeg curl ttf-dejavu
RUN curl -L https://github.com/shaka-project/shaka-packager/releases/download/v3.4.2/packager-linux-arm64 \
    -o /usr/local/bin/packager && chmod +x /usr/local/bin/packager
COPY --from=build /app/go-upload/go-upload /usr/local/bin/go-upload
COPY parse_fmp4_fragments.py /sbin/parse_fmp4_fragments.py
COPY generate_abr /generate_abr
RUN chmod +x /generate_abr/*.sh /generate_abr/*.py 2>/dev/null || true
EXPOSE 8003
CMD ["/usr/local/bin/go-upload"]
