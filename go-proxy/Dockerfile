FROM golang:1.21-alpine AS build
WORKDIR /app

ARG VERSION=unknown

RUN go mod init github.com/boss/go-proxy

COPY . .
RUN go get github.com/gorilla/mux
RUN go get github.com/bradfitz/gomemcache/memcache
RUN go get github.com/grafov/m3u8
RUN go get modernc.org/sqlite@v1.29.0

RUN go build -ldflags "-X main.versionString=${VERSION}" -o go-proxy cmd/server/main.go

FROM alpine:3.18
WORKDIR /app
RUN apk add --no-cache iproute2 nftables
COPY --from=build /app/go-proxy /usr/local/bin/go-proxy
COPY --from=build /app/go.mod /app/go.sum /app/
EXPOSE 30081 30181 30281 30381 30481 30581 30681 30781 30881
ENTRYPOINT ["/usr/local/bin/go-proxy"]
