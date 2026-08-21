# Reproducible development tasks for mysql-mcp.
# Override variables on the command line when needed, for example:
#   make build VERSION=v1.2.3 COMMIT=0123456789abcdef
#   make docker-build IMAGE=megaxcn/mysql-mcp TAG=v1.2.3

GO ?= go
GOTOOLCHAIN ?= local
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || printf '%s' development)
COMMIT ?= $(shell git rev-parse --verify HEAD 2>/dev/null || printf '%s' unknown)
BINARY ?= bin/mysql-mcp
COVERAGE_FILE ?= coverage.out
FUZZ_TIME ?= 10s
IMAGE ?= megaxcn/mysql-mcp
TAG ?= local
RELEASE_DIR ?= dist
TARGET_OS ?= $(shell $(GO) env GOOS)
TARGET_ARCH ?= $(shell $(GO) env GOARCH)
TARGET_ARM ?=

# Build metadata can originate in Git refs. Export values and reference them
# through the shell environment in recipes so their contents are never parsed
# as shell source after Make expands them.
export GOTOOLCHAIN VERSION COMMIT BINARY COVERAGE_FILE IMAGE TAG RELEASE_DIR TARGET_OS TARGET_ARCH TARGET_ARM

# Go source names conventionally contain no whitespace. Keeping this list in
# make lets the formatting targets work with both BSD make and GNU make.
GO_FILES := $(shell find cmd internal scripts -type f -name '*.go' -print 2>/dev/null)

.DEFAULT_GOAL := help

.PHONY: help fmt fmt-check tidy tidy-check deps-verify vet test test-race cover fuzz build release-build check docker-build clean

help: ## Show the available targets.
	@awk 'BEGIN {FS = ":.*## "; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z0-9_-]+:.*## / {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

fmt: ## Format all Go source files with gofmt.
	@test -n "$(GO_FILES)" || { echo "no Go source files found" >&2; exit 1; }
	@gofmt -w $(GO_FILES)

fmt-check: ## Fail if any Go source file is not gofmt-formatted.
	@test -n "$(GO_FILES)" || { echo "no Go source files found" >&2; exit 1; }
	@files="$$(gofmt -l $(GO_FILES))"; \
	if [ -n "$$files" ]; then \
		echo "The following files are not gofmt-formatted:" >&2; \
		echo "$$files" >&2; \
		exit 1; \
	fi

tidy: ## Update go.mod and go.sum to match the source tree.
	$(GO) mod tidy

tidy-check: ## Check that go.mod and go.sum are already tidy.
	$(GO) mod tidy -diff

deps-verify: ## Download modules and verify their checksums.
	$(GO) mod download
	$(GO) mod verify

vet: ## Run the Go static analyzer.
	$(GO) vet ./...

test: ## Run all unit tests once.
	$(GO) test -count=1 ./...

test-race: ## Run all unit tests with the race detector.
	$(GO) test -count=1 -race -timeout=10m ./...

cover: ## Run tests and write an atomic coverage profile.
	$(GO) test -count=1 -covermode=atomic -coverprofile="$${COVERAGE_FILE}" ./...
	$(GO) tool cover -func="$${COVERAGE_FILE}"

fuzz: ## Fuzz the SQL read-policy boundary for FUZZ_TIME (default: 10s).
	$(GO) test ./internal/policy -run '^$$' -fuzz=FuzzValidateReadQuery -fuzztime="$(FUZZ_TIME)" -timeout=5m

build: ## Build the mysql-mcp CLI into bin/.
	@mkdir -p "$$(dirname "$${BINARY}")"
	$(GO) build -mod=readonly -trimpath -ldflags "-s -w -X main.version=$${VERSION} -X main.commit=$${COMMIT}" -o "$${BINARY}" ./cmd/mysql-mcp

release-build: ## Package one release target (set VERSION and optional TARGET_* variables).
	VERSION="$${VERSION}" COMMIT="$${COMMIT}" GOOS="$${TARGET_OS}" GOARCH="$${TARGET_ARCH}" GOARM="$${TARGET_ARM}" OUT_DIR="$${RELEASE_DIR}" ./scripts/package-release.sh

check: fmt-check tidy-check deps-verify vet test-race build ## Run the standard local verification suite.

docker-build: ## Build the runtime image as IMAGE:TAG.
	docker build --build-arg VERSION="$${VERSION}" --build-arg COMMIT="$${COMMIT}" --tag "$${IMAGE}:$${TAG}" .

clean: ## Remove generated binaries and coverage output.
	rm -f "$${BINARY}" "$${COVERAGE_FILE}"
	@rmdir "$$(dirname "$${BINARY}")" 2>/dev/null || true
