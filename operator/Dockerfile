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
COPY operator ./operator
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/operator ./operator/cmd/operator

FROM ${RUNTIME_IMAGE} AS runtime
COPY --from=build /out/operator /usr/local/bin/operator
USER nonroot:nonroot
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/operator"]
