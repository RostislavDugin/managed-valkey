ARG BUILDER_IMAGE=caddy:2.11.4-builder@sha256:b8f9c720f13f64c13dd42db28e8f38a3fab54c11fce4d93bda26d710c448dcfd
ARG RUNTIME_IMAGE=caddy:2.11.4-alpine@sha256:5f5c8640aae01df9654968d946d8f1a56c497f1dd5c5cda4cf95ab7c14d58648

ARG RATELIMIT_VERSION=v0.1.1-0.20260612195517-5625512f24f6

FROM ${BUILDER_IMAGE} AS build
ARG RATELIMIT_VERSION
RUN xcaddy build --with github.com/mholt/caddy-ratelimit@${RATELIMIT_VERSION}

FROM ${RUNTIME_IMAGE} AS runtime
COPY --from=build /usr/bin/caddy /usr/bin/caddy
COPY caddy/Caddyfile /etc/caddy/Caddyfile
