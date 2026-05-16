# CloudDeploy v3 Makefile.
#
# Convenience targets for development. Production deploys go through
# bootstrap.sh, not make.

GO       ?= go
GOFLAGS  ?=
BINARY    = clouddeployctl
PKG       = ./cmd/clouddeployctl
LDFLAGS  ?= -s -w

.PHONY: all build test lint vet fmt tidy clean help

all: build

## build           Compile clouddeployctl into ./bin/.
build:
	mkdir -p bin
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/$(BINARY) $(PKG)

## test            Run unit tests with race detector.
test:
	$(GO) test -race -count=1 ./...

## lint            Run go vet (and golangci-lint if available).
lint: vet
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed; skipping (go vet ran above)"; \
	fi

## vet             Run go vet.
vet:
	$(GO) vet ./...

## fmt             gofmt -s -w on everything.
fmt:
	gofmt -s -w .

## tidy            go mod tidy.
tidy:
	$(GO) mod tidy

## clean           Remove build artefacts.
clean:
	rm -rf bin/ dist/ coverage.out

## help            Show this help.
help:
	@awk 'BEGIN {FS = ":.*## "}; /^## / {sub(/^## /, "", $$0); split($$0, parts, /[[:space:]]+/); printf "  \033[36m%-15s\033[0m %s\n", parts[1], substr($$0, index($$0, " ")+1)}' $(MAKEFILE_LIST)
