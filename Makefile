# Makefile for glusterfs-csi.
#
# Common targets:
#   make build   - compile bin/glusterfs-csi with version information
#   make test    - run unit tests with the race detector
#   make lint    - run golangci-lint
#   make image   - build a multi-arch image (amd64 + arm64, no push)
#   make clean   - remove build artifacts

BINARY     ?= glusterfs-csi
BIN_DIR    ?= bin
CMD        ?= ./cmd/glusterfs-csi
IMAGE      ?= ghcr.io/gitlayzer/glusterfs-csi
TAG        ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PLATFORMS  ?= linux/amd64,linux/arm64
PKG        := github.com/gitlayzer/csi-driver-glusterfs

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo v0.0.0-dev)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(PKG)/pkg/version.Version=$(VERSION) \
	-X $(PKG)/pkg/version.GitCommit=$(GIT_COMMIT) \
	-X $(PKG)/pkg/version.BuildDate=$(BUILD_DATE)

GO        ?= go
GOFLAGS   ?=
GOLANGCI  ?= golangci-lint
DOCKER    ?= docker

.PHONY: all build test lint vet fmt image clean help

all: build

## build: compile the binary into $(BIN_DIR)/$(BINARY)
build:
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) $(CMD)

## test: run unit tests with the race detector
test:
	$(GO) test -race $(GOFLAGS) ./...

## vet: run go vet
vet:
	$(GO) vet ./...

## lint: run golangci-lint
lint:
	$(GOLANGCI) run

## fmt: format the source tree
fmt:
	gofmt -w $$(git ls-files '*.go')

## image: build a multi-arch image without pushing
image:
	$(DOCKER) buildx build --platform $(PLATFORMS) \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(IMAGE):$(TAG) .

## clean: remove build artifacts
clean:
	rm -rf $(BIN_DIR) coverage.out coverage.html

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //'
