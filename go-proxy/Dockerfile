FROM golang:1.21-alpine as build
WORKDIR /app

RUN go mod init github.com/boss/go-proxy

COPY . .
RUN go get github.com/gorilla/mux
RUN go get github.com/bradfitz/gomemcache/memcache
RUN go get github.com/grafov/m3u8

RUN go build -o go-proxy cmd/server/main.go

FROM alpine:3.18
WORKDIR /app
RUN apk add --no-cache iproute2 nftables
COPY --from=build /app/go-proxy /usr/local/bin/go-proxy
COPY --from=build /app/go.mod /app/go.sum /app/
EXPOSE 8080 20080 20081 20180 20181 20280 20281 20380 20381 20480 20481 20580 20581 20680 20681 20780 20781 20880 20881
ENTRYPOINT ["/usr/local/bin/go-proxy"]
