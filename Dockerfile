# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.27-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/atelier ./cmd/atelier

# ---- runtime ----
FROM alpine:3.22

# git: the real git binary Atelier shells out to
# git-daemon: provides git-http-backend on Alpine (smart HTTP push/clone)
RUN apk add --no-cache git git-daemon ca-certificates tzdata \
 && git config --system safe.directory '*' \
 && addgroup -S -g 1000 atelier \
 && adduser -S -D -H -u 1000 -G atelier -h /data atelier \
 && mkdir -p /data /backups \
 && chown atelier:atelier /data /backups

COPY --from=build /out/atelier /usr/local/bin/atelier

ENV ATELIER_LISTEN=:3000 \
    ATELIER_SSH_LISTEN=:2222 \
    ATELIER_DATA=/data

USER atelier
WORKDIR /data
VOLUME ["/data", "/backups"]
# 3000: web and git over HTTPS; 2222: git over SSH
EXPOSE 3000 2222

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:3000/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/atelier"]
