ARG BUILDER_IMAGE=caddy:2.11.4-builder@sha256:b8f9c720f13f64c13dd42db28e8f38a3fab54c11fce4d93bda26d710c448dcfd
ARG RUNTIME_IMAGE=caddy:2.11.4-alpine@sha256:5f5c8640aae01df9654968d946d8f1a56c497f1dd5c5cda4cf95ab7c14d58648
ARG NODE_IMAGE=node:24.8.0-alpine3.22@sha256:3e843c608bb5232f39ecb2b25e41214b958b0795914707374c8acc28487dea17

ARG RATELIMIT_VERSION=v0.1.1-0.20260612195517-5625512f24f6
ARG PNPM_VERSION=10.33.0

FROM ${BUILDER_IMAGE} AS build
ARG RATELIMIT_VERSION
RUN xcaddy build --with github.com/mholt/caddy-ratelimit@${RATELIMIT_VERSION}

FROM ${NODE_IMAGE} AS web-build
ARG PNPM_VERSION
WORKDIR /src/web
RUN corepack enable && corepack prepare pnpm@${PNPM_VERSION} --activate
COPY web/package.json web/pnpm-lock.yaml ./
RUN --mount=type=cache,target=/root/.local/share/pnpm/store \
    pnpm install --frozen-lockfile
COPY web ./
RUN pnpm build

FROM ${RUNTIME_IMAGE} AS runtime
COPY --from=build /usr/bin/caddy /usr/bin/caddy
COPY --from=web-build /src/web/dist /srv
COPY caddy/Caddyfile /etc/caddy/Caddyfile
