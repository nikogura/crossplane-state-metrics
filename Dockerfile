# Build stage. Go cross-compiles natively via GOOS/GOARCH, so the builder runs
# on the native BUILDPLATFORM and no QEMU emulation is needed for the compile.
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS builder

# TARGETOS / TARGETARCH are supplied automatically by `docker buildx build`
# when invoked with --platform. They are declared WITHOUT defaults on purpose:
# a bare `docker build` then fails loudly rather than silently producing a
# wrong-architecture binary inside a per-arch manifest tag.
ARG TARGETOS
ARG TARGETARCH

# VERSION is stamped into the binary's telemetry as service.version. CI passes
# the computed semver; a local build defaults to "dev".
ARG VERSION=dev

WORKDIR /build

# Dependencies first, so the module layer caches across source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags="-X main.serviceVersion=${VERSION}" \
    -o /bin/crossplane-state-metrics ./cmd/crossplane-state-metrics

# Runtime stage. The exporter is a pure-Go static binary that talks only to the
# Kubernetes API, so the image carries the binary plus CA certificates and
# nothing else: no shell, no package manager, non-root by default (UID 65532).
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /bin/crossplane-state-metrics /crossplane-state-metrics

# The admin listener carries /metrics, /healthz and /readyz. There is no
# client-facing listener to separate it from.
EXPOSE 8080

ENTRYPOINT ["/crossplane-state-metrics"]
