FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOFLAGS=-trimpath go build -o /out/forwarder .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/forwarder /forwarder
# Default LLM_PROFILES_PATH points to /config/llm_profiles.yaml.
# Default SESSION_CHAT_PROMPT_PATH points to /config/prompts/session_chat.md.
# Mount a ConfigMap / bind-mount over /config to override per
# environment without rebuilding the image; the prompt file
# specifically can be edited live and the next request picks it up
# (mtime-cached re-read).
COPY config/ /config/
COPY prompts/ /config/prompts/
USER nonroot:nonroot
ENTRYPOINT ["/forwarder"]
