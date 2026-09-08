ARG GO_IMAGE=golang:1.27.1-trixie@sha256:9baa6b4187bbb98d240372a8a235ac0bb6b5ddd52bba1431dc2f7c0705862728
ARG RUNTIME_IMAGE=gcr.io/distroless/static:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7

FROM ${GO_IMAGE} AS deps
ENV GOTOOLCHAIN=auto \
    CGO_ENABLED=0 \
    GOFLAGS=-buildvcs=false
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

FROM deps AS build
COPY api ./api
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/api ./api/cmd/api && \
    go build -trimpath -ldflags="-s -w" -o /out/healthcheck ./api/cmd/healthcheck

FROM deps AS goose-build
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/goose github.com/pressly/goose/v3/cmd/goose

FROM ${RUNTIME_IMAGE} AS runtime
COPY --from=build /out/api /usr/local/bin/api
COPY --from=build /out/healthcheck /usr/local/bin/healthcheck
USER nonroot:nonroot
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=5s --start-period=10s --retries=6 \
    CMD ["/usr/local/bin/healthcheck", "http://127.0.0.1:8080/readyz"]
ENTRYPOINT ["/usr/local/bin/api"]

FROM ${RUNTIME_IMAGE} AS migrate
COPY --from=goose-build /out/goose /usr/local/bin/goose
COPY migrations /migrations
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/goose", "-dir", "/migrations"]
CMD ["up"]
