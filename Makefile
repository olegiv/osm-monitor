SHELL := /bin/bash
.DEFAULT_GOAL := help

BINARY_NAME := osm-monitor
BUILD_DIR := bin
GO := go
ENV_FILE ?= .env
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

LDFLAGS_VERSION := -X main.version=$(VERSION) -X main.gitCommit=$(GIT_COMMIT) -X main.buildTime=$(BUILD_TIME)

RUN_WITH_ENV = set -a; if [[ -f "$(ENV_FILE)" ]]; then . "$(ENV_FILE)"; fi; set +a;

.PHONY: help build build-prod build-linux-amd64 build-darwin-arm64 build-all-platforms test coverage fmt-check vet lint staticcheck sec vuln check run dry-run clean

help:
	@printf '%s\n' \
		'Available targets:' \
		'  make build                Build bin/osm-monitor' \
		'  make build-prod           Build optimized production binary' \
		'  make build-linux-amd64    Build Linux AMD64 release binary' \
		'  make build-darwin-arm64   Build macOS ARM64 release binary' \
		'  make build-all-platforms  Build all production platform binaries' \
		'  make test                 Run Go unit tests' \
		'  make coverage             Run tests with coverage report' \
		'  make fmt-check            Verify gofmt formatting' \
		'  make vet                  Run go vet' \
		'  make lint                 Run golangci-lint' \
		'  make staticcheck          Run staticcheck' \
		'  make sec                  Run gosec security scanner' \
		'  make vuln                 Run govulncheck vulnerability scanner' \
		'  make check                Run all quality gates (fmt, build, test, vet, lint, staticcheck, sec, vuln)' \
		'  make run                  Run one monitoring cycle using .env when present' \
		'  make dry-run              Run one monitoring cycle without sending alerts or saving state' \
		'  make clean                Remove build artifacts'

build:
	@mkdir -p $(BUILD_DIR)
	$(GO) build -ldflags="$(LDFLAGS_VERSION)" -o $(BUILD_DIR)/$(BINARY_NAME) .

build-prod:
	@mkdir -p $(BUILD_DIR)
	$(GO) build -ldflags="-s -w $(LDFLAGS_VERSION)" -trimpath -o $(BUILD_DIR)/$(BINARY_NAME) .

build-linux-amd64:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 $(GO) build -ldflags="-s -w $(LDFLAGS_VERSION)" -trimpath -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 .

build-darwin-arm64:
	@mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=arm64 $(GO) build -ldflags="-s -w $(LDFLAGS_VERSION)" -trimpath -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 .

build-all-platforms: build-linux-amd64 build-darwin-arm64
	@ls -lh $(BUILD_DIR)/$(BINARY_NAME)-*

test:
	$(GO) test ./...

coverage:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [[ -n "$$unformatted" ]]; then \
		echo "gofmt needed on:" >&2; \
		echo "$$unformatted" >&2; \
		exit 1; \
	fi

vet:
	$(GO) vet ./...

lint:
	golangci-lint run

staticcheck:
	staticcheck ./...

sec:
	gosec -quiet ./...

vuln:
	govulncheck ./...

check: fmt-check build test vet lint staticcheck sec vuln
	@echo "All quality gates passed."

run: build
	@$(RUN_WITH_ENV) ./$(BUILD_DIR)/$(BINARY_NAME)

dry-run: build
	@$(RUN_WITH_ENV) ./$(BUILD_DIR)/$(BINARY_NAME) --dry-run --verbose

clean:
	rm -rf $(BUILD_DIR) coverage.out
