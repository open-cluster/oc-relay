# The CI gates ARE this file: contributors reproduce CI locally with these targets.

BUF_VERSION := v1.47.2
PROTOC_GEN_GO_VERSION := v1.36.5
PROTOC_GEN_GO_GRPC_VERSION := v1.5.1

.PHONY: tools lint gen gen-check build test breaking descriptor

tools:
	go install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
	go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)

lint:
	buf lint
	buf format --diff --exit-code
	gofmt -l . | (! grep .)
	go vet ./...

gen:
	buf generate

# CI drift gate: regeneration must produce no uncommitted changes.
gen-check: gen
	git diff --exit-code -- gen/

build:
	CGO_ENABLED=0 go build -trimpath -buildvcs=false ./...

test:
	CGO_ENABLED=0 go test -race ./...

# Breaking-change gate against the committed baseline (main).
breaking:
	buf breaking --against '.git#branch=main'

# Descriptor set for the control-plane sync (copy + manifest + descriptor updated
# atomically by the sync script on the consuming side).
descriptor:
	buf build -o gen/descriptor/opencluster-relay-v1.binpb
