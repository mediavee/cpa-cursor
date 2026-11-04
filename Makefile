BUF := go run github.com/bufbuild/buf/cmd/buf@v1.47.2
PROTOC_GEN_GO := google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.0

.PHONY: gen
gen:
	rm -rf internal/gen
	GOBIN=$(CURDIR)/bin go install $(PROTOC_GEN_GO)
	PATH=$(CURDIR)/bin:$$PATH $(BUF) generate
