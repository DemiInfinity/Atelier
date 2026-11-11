# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.27-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/atelier ./cmd/atelier

# ---- atelier-runner image: docker build --target runner -t atelier-runner . ----
# Kept before the server's runtime stage so a plain `docker build` still
# produces the Atelier server image.
FROM build AS runner-build
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/atelier-runner ./cmd/atelier-runner

FROM alpine:3.22 AS runner
# git for checkout, bash for run steps, nodejs for JavaScript actions,
# docker-cli for container: jobs (mount the host's /var/run/docker.sock),
# docker-cli-buildx so `docker build` uses BuildKit (the legacy builder is deprecated)
RUN apk add --no-cache git ca-certificates tzdata bash nodejs docker-cli docker-cli-buildx \
 && git config --system safe.directory '*' \
 && mkdir -p /runner
# Extra trusted CA roots (deploy/ca/*.crt, e.g. a homelab mkcert root).
COPY deploy/ca/ /usr/local/share/ca-certificates/extra/
RUN find /usr/local/share/ca-certificates/extra -type f ! -name '*.crt' -delete && update-ca-certificates 2>/dev/null; true
COPY --from=runner-build /out/atelier-runner /usr/local/bin/atelier-runner
# The config (with the runner token) and the action cache live in /runner.
# Job workspaces go to the --work-dir chosen at register time; when jobs use
# docker, mount it at the same path on the host (the host resolves bind mounts).
ENV HOME=/runner
VOLUME ["/runner"]
ENTRYPOINT ["/usr/local/bin/atelier-runner"]
CMD ["run", "--config", "/runner/config.json"]

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

# Extra trusted CA roots (deploy/ca/*.crt), as in the runner image.
COPY deploy/ca/ /usr/local/share/ca-certificates/extra/
RUN find /usr/local/share/ca-certificates/extra -type f ! -name '*.crt' -delete && update-ca-certificates 2>/dev/null; true
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
