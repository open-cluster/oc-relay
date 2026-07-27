# OpenCluster Relay

Status: private pre-release. Not published, no releases, no public artifacts. The
module and image identity are provisional pending name clearance (docs/naming.md).

## Overview

The OpenCluster Relay is the customer-side execution component of the OpenCluster
investigation platform. It runs inside a customer environment (in-cluster, or beside a
self-hosted control plane), maintains one outbound bidirectional gRPC stream over TLS
to the OpenCluster control plane, and executes typed, versioned, bounded, read-only
capabilities compiled into the binary — returning bounded structured results.

The Relay contains no model code, no AI reasoning, no incident state, no shell, no
scripts, no dynamic plugins, and no generic remote-execution mechanism. Every
capability is a compiled Go handler with a frozen Protobuf argument/result schema,
enforced by build-failing gates rather than convention: the cgo-disabled build, an
import-graph and AST gate suite (no process spawning, no dynamic loading, no
exec/port-forward surfaces, capability packages confined to the read-only Kubernetes
port), a descriptor schema-shape gate that keeps dynamic payload types and
command-like fields out of the protocol, and a pinned depguard/forbidigo lint layer.

## What data leaves the cluster

Only bounded, typed capability results (for example: workload runtime projections —
replica counters, pod/container states, restart counts, images, resource requests).
Result size caps are enforced Relay-side before send. A structured local audit log
records every executed job and every result's byte count, so operators can verify
egress independently of the vendor. The customer-authored local policy (destination
allowlist, hard rate/volume caps, masking rules) cannot be modified by the control
plane. The full per-capability egress statement ships with each capability's
documentation.

## Repository layout

- `proto/opencluster/relay/v1/` — the protocol source of truth (Buf-managed)
- `gen/go/` — committed generated Go (drift-gated in CI), and its own Go module so the
  contract can be imported without this repository's Kubernetes dependencies
- `docs/` — protocol and design documentation
- `cmd/opencluster-relay/` — the composition root
- `internal/` — identity, session runtime, read-only Kubernetes port, capabilities,
  configuration, pinned transport, and the local audit trail

## Development

Requires Go 1.26+, Buf 1.47.x, and the pinned protoc plugins:

```
make tools   # install pinned buf/protoc-gen-go/protoc-gen-go-grpc
make lint    # buf lint + format check
make gen     # regenerate gen/go (CI fails on uncommitted drift)
make build   # go build ./...
make test    # go test -race ./...
```

The repository holds two Go modules: the Relay itself, and the generated contract under
`gen/go`. Nested modules are not reached by `./...`, so the Makefile targets run each gate
against both.

## Consuming the protocol

Speak the protocol by importing the contract module at a pinned version:

```
go get github.com/open-cluster/oc-relay/gen/go@v0.1.0
```

It requires only gRPC and protobuf. The Relay's own module pulls in client-go and the
Kubernetes libraries because it reads clusters; a consumer that merely speaks the protocol
must not inherit that graph, and a build-failing gate keeps it out. Contract versions are
tagged `gen/go/vX.Y.Z`.

While this repository is private, a consumer needs `GOPRIVATE=github.com/open-cluster/*`
so the module is fetched directly rather than through the public proxy and checksum
database. `go.sum` still pins and verifies what was fetched; what is lost until the
repository is public is the transparency log's independent cross-check.

## License

Apache-2.0 (see LICENSE). Third-party notices ship with every released artifact.
