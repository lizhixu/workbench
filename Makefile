# Watchman build helpers.
#
# The single source of truth for the product version is the linker variable
# watchman/internal/version.Version (see internal/version/version.go). Pass a
# specific release with `make VERSION=v1.2.3 build-all`; otherwise the version
# is derived from git.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X watchman/internal/version.Version=$(VERSION) -X watchman/internal/version.Commit=$(COMMIT) -X watchman/internal/version.BuildTime=$(BUILD_TIME)

GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

.PHONY: all build-web build-server build-server-with-ui build-agent build-all test vet fmt

all: build-server build-agent

# Web console build. Its output is copied into server/web/dist/ so the Go
# build embeds it (see DESIGN.md §8.1).
build-web:
	cd web && npm ci && npm run build

# Fast Go-only server build for development: embeds whatever is currently
# in server/web/dist/ (empty on a fresh checkout → the binary answers 503
# for page requests; the API is unaffected). Use build-server-with-ui for
# a shippable binary.
build-server:
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o bin/watchman ./server/cmd/watchman

build-server-with-ui: build-web
	rm -rf server/web/dist
	mkdir -p server/web/dist
	cp -r web/dist/. server/web/dist/
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o bin/watchman ./server/cmd/watchman

build-agent:
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o bin/watchman-agent ./agent/cmd/watchman-agent

build-all: build-server build-agent

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w agent internal proto server
