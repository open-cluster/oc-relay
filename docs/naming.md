# Provisional identity and name clearance

The component name "OpenCluster Relay" is subject to name clearance before any public
artifact exists. Known collisions recorded: Sentry Relay (getsentry/relay — a
customer-side component between customer infrastructure and a central SaaS; getsentry
owns the `relay` container-image name) and GraphQL Relay (Meta). Separately,
"OpenCluster" vs CNCF Open Cluster Management is a program-level brand question owned
by the founder.

Until clearance completes, five surfaces carry a PROVISIONAL identity and rename
together as ONE atomic change:

1. Go module path (`github.com/OCluster/opencluster-relay`)
2. Protobuf package (`opencluster.relay.v1` — a wire/namespace contract)
3. `csharp_namespace` option (`OpenCluster.Relay.Protocol.V1`)
4. Container image name (none published yet)
5. Helm chart name (none published yet)

Deadline: the rename must complete before the FIRST persistent or design-partner
deployment — not merely before a public tag — because a deployment that pins the
proto package converts the rename into a breaking migration.

Never ship a bare `relay` package/image/repo name in any public artifact.
