# syntax=docker/dockerfile:1

# Base images are overridable so that local builds can use a registry mirror,
# e.g. --build-arg GO_IMAGE=docker.1ms.run/library/golang:1.27
ARG GO_IMAGE=golang:1.27
ARG BASE_IMAGE=debian:trixie-slim

# --- build stage -----------------------------------------------------------
# The builder uses a supported Go release; go.mod keeps a lower `go` directive
# so that downstream consumers can still build with older toolchains.
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS builder

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=unknown
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown

WORKDIR /src

# Cache module downloads separately from the source tree.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0
RUN GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath \
      -ldflags "-s -w \
        -X github.com/gitlayzer/csi-driver-glusterfs/pkg/version.Version=${VERSION} \
        -X github.com/gitlayzer/csi-driver-glusterfs/pkg/version.GitCommit=${GIT_COMMIT} \
        -X github.com/gitlayzer/csi-driver-glusterfs/pkg/version.BuildDate=${BUILD_DATE}" \
      -o /out/glusterfs-csi ./cmd/glusterfs-csi

# --- runtime stage ---------------------------------------------------------
# glusterfs-client provides mount.glusterfs for the FUSE data path;
# ca-certificates is needed for the CSI sidecars and outbound TLS.
# trixie ships glusterfs-client 11.x, matching the supported server versions.
FROM ${BASE_IMAGE}

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      glusterfs-client \
      ca-certificates \
 && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/glusterfs-csi /glusterfs-csi

ENTRYPOINT ["/glusterfs-csi"]
