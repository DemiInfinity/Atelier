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
COPY deploy/ca/ /tmp/ca/
RUN find /tmp/ca -maxdepth 1 -type f -name '*.crt' -exec cp {} /usr/local/share/ca-certificates/ \; ; rm -rf /tmp/ca; update-ca-certificates 2>/dev/null; true
COPY --from=runner-build /out/atelier-runner /usr/local/bin/atelier-runner
# The config (with the runner token) and the action cache live in /runner.
# Job workspaces go to the --work-dir chosen at register time; when jobs use
# docker, mount it at the same path on the host (the host resolves bind mounts).
ENV HOME=/runner
VOLUME ["/runner"]
ENTRYPOINT ["/usr/local/bin/atelier-runner"]
CMD ["run", "--config", "/runner/config.json"]

# ---- cloudflared, for remote access (Settings → Remote access) ----
# A pinned release, checked against the SHA-256 sums Cloudflare publishes in
# the release notes (and GitHub's asset digests) for
# https://github.com/cloudflare/cloudflared/releases/tag/2026.10.4
# To upgrade: change the version and both sums together.
FROM alpine:3.22 AS cloudflared
ARG TARGETARCH
ARG CLOUDFLARED_VERSION=2026.10.4
ARG CLOUDFLARED_SHA256_AMD64=ae5e8add7884b0531d66bbca4570d1de491ff6da51e4ebe7eb160cdf241d9e42
ARG CLOUDFLARED_SHA256_ARM64=2b7203b4fb0ba28a0c1e12f8855ce2c25f76d266a8d5edafac5540b6812acff6
RUN set -eu; \
    arch="${TARGETARCH:-amd64}"; \
    case "$arch" in \
      amd64) sum="$CLOUDFLARED_SHA256_AMD64" ;; \
      arm64) sum="$CLOUDFLARED_SHA256_ARM64" ;; \
      *) echo "cloudflared: no pinned build for $arch" >&2; exit 1 ;; \
    esac; \
    wget -q -O /tmp/cloudflared "https://github.com/cloudflare/cloudflared/releases/download/${CLOUDFLARED_VERSION}/cloudflared-linux-${arch}"; \
    echo "$sum  /tmp/cloudflared" | sha256sum -c -; \
    install -m 0755 /tmp/cloudflared /usr/local/bin/cloudflared

# ---- tailscale and tailscaled, the second remote access provider ----
# A pinned stable release from Tailscale's own static tarballs, checked
# against the SHA-256 published beside each tarball
# (https://pkgs.tailscale.com/stable/tailscale_<version>_<arch>.tgz.sha256).
# To upgrade: change the version and both sums together.
FROM alpine:3.22 AS tailscale
ARG TARGETARCH
ARG TAILSCALE_VERSION=1.104.6
ARG TAILSCALE_SHA256_AMD64=82a85429f733e52fd1a95dedb2c8dd8cd46d6603504e566a56b1779c9a7a4809
ARG TAILSCALE_SHA256_ARM64=d0e4f297da3c428d02162c6235b9948e4ddc245c64502b7b05647da9b96a0971
RUN set -eu; \
    arch="${TARGETARCH:-amd64}"; \
    case "$arch" in \
      amd64) sum="$TAILSCALE_SHA256_AMD64" ;; \
      arm64) sum="$TAILSCALE_SHA256_ARM64" ;; \
      *) echo "tailscale: no pinned build for $arch" >&2; exit 1 ;; \
    esac; \
    name="tailscale_${TAILSCALE_VERSION}_${arch}"; \
    wget -q -O /tmp/tailscale.tgz "https://pkgs.tailscale.com/stable/${name}.tgz"; \
    echo "$sum  /tmp/tailscale.tgz" | sha256sum -c -; \
    tar -xzf /tmp/tailscale.tgz -C /tmp "${name}/tailscale" "${name}/tailscaled"; \
    install -m 0755 "/tmp/${name}/tailscale" /usr/local/bin/tailscale; \
    install -m 0755 "/tmp/${name}/tailscaled" /usr/local/bin/tailscaled

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
COPY deploy/ca/ /tmp/ca/
RUN find /tmp/ca -maxdepth 1 -type f -name '*.crt' -exec cp {} /usr/local/share/ca-certificates/ \; ; rm -rf /tmp/ca; update-ca-certificates 2>/dev/null; true
COPY --from=build /out/atelier /usr/local/bin/atelier
# Remote access is off until turned on in Settings; Atelier runs cloudflared
# itself and it connects back to 127.0.0.1:3001 inside the container only.
COPY --from=cloudflared /usr/local/bin/cloudflared /usr/local/bin/cloudflared
# The Tailscale provider: tailscaled runs with --tun=userspace-networking as
# the atelier user (no TUN device, no NET_ADMIN, no root), its state in
# /data/tailscale, and only while remote access is on with Tailscale chosen.
COPY --from=tailscale /usr/local/bin/tailscale /usr/local/bin/tailscaled /usr/local/bin/

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
