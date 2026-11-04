BUF := go run github.com/bufbuild/buf/cmd/buf@v1.47.2
PROTOC_GEN_GO := google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.0
PLUGIN_IMAGE ?= golang:1.26-bookworm
PLUGIN_ARCHS ?= arm64 amd64
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0

.PHONY: gen
gen:
	rm -rf internal/gen
	GOBIN=$(CURDIR)/bin go install $(PROTOC_GEN_GO)
	PATH=$(CURDIR)/bin:$$PATH $(BUF) generate

# check is the gate: CI runs exactly this.
.PHONY: check
check: fmt-check vet lint test gen-check build plugin

.PHONY: fmt-check
fmt-check:
	@out="$$(gofmt -l cmd internal plugin)"; test -z "$$out" || { echo "gofmt needed:"; echo "$$out"; exit 1; }

.PHONY: vet
vet:
	go vet ./...

.PHONY: lint
lint:
	$(GOLANGCI_LINT) run ./...

.PHONY: test
test:
	go test -race -count=1 ./...

# gen-check regenerates into a temporary directory and compares, so it does
# not depend on the state of the git tree.
.PHONY: gen-check
gen-check:
	GOBIN=$(CURDIR)/bin go install $(PROTOC_GEN_GO)
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	PATH=$(CURDIR)/bin:$$PATH $(BUF) generate --output "$$tmp" && \
	diff -r "$$tmp/internal/gen" internal/gen >/dev/null || { echo "internal/gen is stale: run 'make gen' and commit"; exit 1; }

.PHONY: build
build:
	go build ./...

# plugin builds the CPA plugin for linux in the image CPA's own release uses (glibc, cgo). A c-shared
# library must be built for its target by a native toolchain, so each architecture gets its own
# container; the module cache is kept in a volume.
.PHONY: plugin
plugin:
	@for arch in $(PLUGIN_ARCHS); do \
		echo "plugin linux/$$arch"; \
		docker run --rm --platform linux/$$arch \
			-v "$(CURDIR)":/src -w /src \
			-v cpa-cursor-gomod:/go/pkg/mod -v cpa-cursor-gocache-$$arch:/root/.cache/go-build \
			-e CGO_ENABLED=1 -e GOFLAGS=-buildvcs=false -e OWNER=$$(id -u):$$(id -g) \
			$(PLUGIN_IMAGE) \
			sh -c 'go build -trimpath -buildmode=c-shared \
				-ldflags "-s -w -X github.com/mediavee/cpa-cursor/plugin.Version=$(VERSION)" \
				-o bin/plugins/linux/'$$arch'/cursor.so ./cmd/plugin \
				&& rm -f bin/plugins/linux/'$$arch'/cursor.h; \
				status=$$?; chown -R "$$OWNER" bin; exit $$status' || exit 1; \
	done
