# The CI gates ARE this file: contributors reproduce CI locally with these targets.

BUF_VERSION := v1.47.2
PROTOC_GEN_GO_VERSION := v1.36.5
PROTOC_GEN_GO_GRPC_VERSION := v1.5.1
STATICCHECK_VERSION := 2025.1.1
GOLANGCI_LINT_VERSION := v2.12.2

# The generated contract is its own module so consumers can speak the protocol without
# inheriting the Kubernetes dependency graph. Nested modules are not reached by "./...",
# so every per-module gate below has to name it: a gate that stops covering part of the
# repository the moment that part moves is worse than no gate, because it still reports
# green.
PROTOCOL_MODULE := gen/go

.PHONY: tools lint gen gen-check build test breaking descriptor

tools:
	go install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
	go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
	go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

lint:
	buf lint
	buf format --diff --exit-code
	gofmt -l . | (! grep .)
	go vet ./...
	cd $(PROTOCOL_MODULE) && go vet ./...
	staticcheck ./...
	cd $(PROTOCOL_MODULE) && staticcheck ./...
	golangci-lint run

gen:
	buf generate

# CI drift gate: regeneration must produce no uncommitted changes.
gen-check: gen
	git diff --exit-code -- gen/

build:
	CGO_ENABLED=0 go build -trimpath -buildvcs=false ./...
	cd $(PROTOCOL_MODULE) && CGO_ENABLED=0 go build -trimpath -buildvcs=false ./...

# The race detector requires cgo, so tests run with CGO_ENABLED=1. This is distinct from
# the shipped artifact: `build` stays CGO_ENABLED=0 for a static, reproducible binary.
test:
	CGO_ENABLED=1 go test -race ./...

# Breaking-change gate against the committed baseline (main).
breaking:
	buf breaking --against '.git#branch=main'

# Descriptor set: the committed baseline the schema-shape gate reads, and the artifact a
# consumer generating for another language builds from.
descriptor:
	buf build -o gen/descriptor/opencluster-relay-v1.binpb
