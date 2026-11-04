BUF := go run github.com/bufbuild/buf/cmd/buf@v1.47.2
PROTOC_GEN_GO := google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.0
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0

.PHONY: gen
gen:
	rm -rf internal/gen
	GOBIN=$(CURDIR)/bin go install $(PROTOC_GEN_GO)
	PATH=$(CURDIR)/bin:$$PATH $(BUF) generate

# check is the gate: CI runs exactly this.
.PHONY: check
check: fmt-check vet lint test gen-check build

.PHONY: fmt-check
fmt-check:
	@out="$$(gofmt -l cmd internal)"; test -z "$$out" || { echo "gofmt needed:"; echo "$$out"; exit 1; }

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
