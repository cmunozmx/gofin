FROM golang:1.25-alpine AS builder
WORKDIR /build
COPY . .
RUN CGO_ENABLED=0 go build -o gofin ./cmd/gofin

# The web interface. Jellyfin Desktop, the mobile wrappers and a browser all
# load it from the server, so an image without it is one those clients cannot
# open. It comes from the upstream release archive -- the same files a real
# Jellyfin server of this version serves -- and only that folder is kept.
#
# To upgrade, change the version and the checksum together:
#   curl -fsSL <url> | sha256sum
# Stay on the release line gofin reports itself as (10.11): the interface and
# the server version are expected to match.
FROM alpine:latest AS web
ARG JELLYFIN_VERSION=10.11.11
ARG JELLYFIN_SHA256=633edb3c01cf4face38f257a03d4d20fe9294ec10a1d239ef06f364be2d249bc
RUN apk add --no-cache curl tar xz
RUN curl -fsSL -o /tmp/jellyfin.tar.xz \
      "https://repo.jellyfin.org/files/server/portable/stable/v${JELLYFIN_VERSION}/any/jellyfin_${JELLYFIN_VERSION}.tar.xz" && \
    echo "${JELLYFIN_SHA256}  /tmp/jellyfin.tar.xz" | sha256sum -c - && \
    mkdir /jellyfin-web && \
    tar -xJf /tmp/jellyfin.tar.xz -C /jellyfin-web --strip-components=2 jellyfin/jellyfin-web && \
    test -f /jellyfin-web/index.html && \
    rm /tmp/jellyfin.tar.xz

FROM alpine:latest
RUN apk add --no-cache ca-certificates tini
COPY --from=web /jellyfin-web /usr/share/jellyfin-web
# Naming the directory also switches the interface on (see gofin.example.json,
# "web"). Run with -no-web for an API-only container.
ENV JELLYFIN_WEB_DIR=/usr/share/jellyfin-web
COPY --from=builder /build/gofin /usr/local/bin/
COPY --from=builder /build/gofin.example.json /etc/gofin/gofin.json.example

# Create working directory with proper permissions
RUN mkdir -p /app/data /app/cache /app/logs /app/config && \
    chmod 755 /app /app/data /app/cache /app/logs /app/config

WORKDIR /app

EXPOSE 8096
ENTRYPOINT ["/sbin/tini", "--"]
CMD ["gofin", "serve", "--log-level", "info"]
