# syntax=docker/dockerfile:1.9

# Build the gateway. BUILDPLATFORM keeps the compile native even when
# cross-building, since Go cross-compiles far faster than QEMU emulates.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=""
ARG BUILD_DATE=""

WORKDIR /src

# Dependencies first, so a source-only change reuses the download layer.
COPY src/go.mod src/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY src/ ./

ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -buildvcs=false \
      -ldflags="-s -w \
        -X github.com/tokaco/pg-k8s-proxy/internal/version.Version=${VERSION} \
        -X github.com/tokaco/pg-k8s-proxy/internal/version.Commit=${COMMIT} \
        -X github.com/tokaco/pg-k8s-proxy/internal/version.BuildDate=${BUILD_DATE}" \
      -o /out/pg-k8s-proxy ./cmd/pg-k8s-proxy

# Distroless static: no shell, no package manager, nothing to exploit. The
# health checks are HTTP probes run by the kubelet, so no in-image tooling is
# needed. nonroot runs as uid/gid 65532, matching the chart's securityContext.
#
# The Debian generation is pinned deliberately. Upstream's floating tags follow
# whatever Debian is current, so an unpinned base would change out from under a
# release; bump this explicitly when moving generations.
FROM gcr.io/distroless/static-debian13:nonroot AS runtime

ARG VERSION=dev
ARG COMMIT=""
ARG BUILD_DATE=""

LABEL org.opencontainers.image.title="pg-k8s-proxy" \
      org.opencontainers.image.description="PostgreSQL gateway and Kubernetes operator routing connections by database name" \
      org.opencontainers.image.source="https://github.com/tokaco/pg-k8s-proxy" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"

COPY --from=builder /out/pg-k8s-proxy /usr/local/bin/pg-k8s-proxy

USER 65532:65532
EXPOSE 5432 8080 9090

ENTRYPOINT ["/usr/local/bin/pg-k8s-proxy"]
