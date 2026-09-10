# Watchman build helpers.
#
# The single source of truth for the product version is the linker variable
# watchman/internal/version.Version (see internal/version/version.go). Pass a
# specific release with `make VERSION=v1.2.3 build-all`; otherwise the version
# is derived from git.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
LDFLAGS := -s -w -X watchman/internal/version.Version=$(VERSION)

GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

.PHONY: all build-server build-agent build-all test vet fmt

all: build-server build-agent

build-server:
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
