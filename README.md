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
enforced by build-failing gates rather than convention — the cgo-disabled build is
wired today and the remaining banned-API gates land with the capability surfaces
they guard, before any release.

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
- `gen/go/` — committed generated Go (drift-gated in CI)
- `docs/` — protocol and design documentation
- Go implementation packages arrive with the first implementation slice (`cmd/`, `internal/`)

## Development

Requires Go 1.26+, Buf 1.47.x, and the pinned protoc plugins:

```
make tools   # install pinned buf/protoc-gen-go/protoc-gen-go-grpc
make lint    # buf lint + format check
make gen     # regenerate gen/go (CI fails on uncommitted drift)
make build   # go build ./...
make test    # go test -race ./...
```

## License

Apache-2.0 (see LICENSE). Third-party notices ship with every released artifact.
