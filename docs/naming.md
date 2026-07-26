# Identity and name clearance

The component name "OpenCluster Relay" is subject to name clearance before any public
artifact exists. Known collisions recorded: Sentry Relay (getsentry/relay — a
customer-side component between customer infrastructure and a central SaaS; getsentry
owns the `relay` container-image name) and GraphQL Relay (Meta).

The five identity surfaces were provisional and renamed together as one atomic change.
Surface 1 is now settled; the rest remain provisional and still rename together.

| # | Surface | Value | Status |
| --- | --- | --- | --- |
| 1 | Go module path | `github.com/open-cluster/oc-relay` | SETTLED — matches the private repository created 2026-07-26 |
| 2 | Protobuf package | `opencluster.relay.v1` — a wire/namespace contract | Provisional |
| 3 | `csharp_namespace` | `OpenCluster.Relay.Protocol.V1` | Provisional |
| 4 | Container image name | none published | Provisional |
| 5 | Helm chart name | none published | Provisional |

Surfaces 2 and 3 carry the product name rather than the repository name, so the module
move did not force them. They change only if the product itself is renamed.

Deadline for the remaining surfaces: the rename must complete before the FIRST persistent
or design-partner deployment — not merely before a public tag — because a deployment that
pins the proto package converts the rename into a breaking migration.

## Open brand question

"OpenCluster" versus CNCF Open Cluster Management remains a program-level brand question
owned by the founder, and the repository move sharpened rather than resolved it: the
GitHub organization is now `open-cluster`, against the CNCF project's
`open-cluster-management`. Two organization names separated by one word invite confusion
about affiliation, and the risk is now embedded in a URL rather than in prose. Resolve
before the repository becomes public.

Never ship a bare `relay` package, image, or repository name in any public artifact.

## Consumer impact of a module-path change

`go_package` embeds the module path, so moving the module changes the generated descriptor
set. Any consumer holding a pinned copy of `proto/` with a SHA-256 manifest and a committed
descriptor baseline must re-synchronise all three atomically after such a move, even though
the wire contract is unchanged.
