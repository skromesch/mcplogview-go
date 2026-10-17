# syntax=docker/dockerfile:1

# ---- build ------------------------------------------------------------------
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w \
        -X github.com/skromesch/mcplogview-go/internal/buildinfo.Version=${VERSION} \
        -X github.com/skromesch/mcplogview-go/internal/buildinfo.Commit=${COMMIT} \
        -X github.com/skromesch/mcplogview-go/internal/buildinfo.Date=${DATE}" \
      -o /out/mcplogview ./cmd/mcplogview

# ---- runtime ----------------------------------------------------------------
# The ssh client is needed for `ssh` sources and for docker sources on remote
# hosts; the docker CLI for `docker` sources (it talks to $DOCKER_HOST).
FROM alpine:3.22
ARG UID=10001
RUN apk add --no-cache openssh-client docker-cli ca-certificates tzdata \
 && addgroup -g "${UID}" mcplogview \
 && adduser -D -u "${UID}" -G mcplogview mcplogview \
 && mkdir -p /config /data /home/mcplogview/.ssh \
 && chown mcplogview:mcplogview /config /data /home/mcplogview/.ssh \
 && chmod 700 /home/mcplogview/.ssh

COPY --from=build /out/mcplogview /usr/local/bin/mcplogview

USER mcplogview
# All mcplogview commands run inside the container (serve, list, check, configure,
# ...) work on the mounted config without --config. The self-log goes to /data.
ENV HOME=/home/mcplogview \
    LOG_SOURCES_CONFIG=/config/mcplogview.json \
    XDG_STATE_HOME=/data
WORKDIR /config
VOLUME ["/data"]
EXPOSE 3333

ENTRYPOINT ["mcplogview"]
CMD ["serve"]
