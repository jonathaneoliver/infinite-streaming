FROM golang:1.21-alpine as build
WORKDIR /app

# Initialize go module
RUN go mod init github.com/jonathaneoliver/infinite-streaming/go-live

# Copy source and install dependencies
COPY . .
RUN go get github.com/gorilla/mux
RUN go get github.com/bluenviron/gohlslib@v1.4.0
RUN go get github.com/google/uuid
RUN go get github.com/beevik/etree@v1.2.0
RUN go get modernc.org/sqlite@v1.29.0

# Build
RUN go build -o go-live cmd/server/main.go

FROM alpine:3.18
WORKDIR /app
RUN mkdir -p /go-live
COPY --from=build /app/go-live /usr/local/bin/go-live
COPY --from=build /app/go.mod /app/go.sum /app/
EXPOSE 8010
ENTRYPOINT ["/usr/local/bin/go-live"]
