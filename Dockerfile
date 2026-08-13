# Build stage. The whole web/ tree is embedded into the binary by go:embed, so
# it must be present here -- but tiles/ and history.db are not, and .dockerignore
# keeps them out of the context.
FROM golang:1.26-alpine AS build
WORKDIR /src

# Dependencies first, so editing source does not re-download modules.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /aircraft-tracker .

# Runtime. Alpine rather than scratch: the extra ~8 MB buys a shell, which on a
# home server is worth more than the saving the first time something misbehaves
# and you want to look inside the container.
FROM alpine:3.21

# ca-certificates is not optional -- every provider is HTTPS, and without it the
# poller fails on every request with an opaque x509 error.
RUN apk add --no-cache ca-certificates wget \
 && adduser -D -u 10001 tracker

COPY --from=build /aircraft-tracker /usr/local/bin/aircraft-tracker

# /data holds the mounted config, fleet, tiles and history. Only history is
# written to; see compose.yml.
WORKDIR /data
USER tracker

EXPOSE 8099

# No TZ is set, so logs are in UTC. That suits a system whose quiet-hours window
# is already defined in UTC, and avoids logs shifting an hour twice a year.
HEALTHCHECK --interval=60s --timeout=5s --start-period=10s \
  CMD wget -qO- http://127.0.0.1:8099/healthz || exit 1

ENTRYPOINT ["aircraft-tracker"]
CMD ["-config", "/data/config.json"]
