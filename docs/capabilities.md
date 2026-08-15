# Capability design

Source of truth for schemas: `proto/opencluster/relay/v1/`. This document records what
the capabilities are and the design decisions behind them — the decisions are settled
and are not relitigated per change.

## The capability model

Every capability is a compiled Go handler behind a frozen Protobuf schema, served
through `internal/capabilities`. The package owns what every capability obeys:

- the budget discipline separating a timeout from a cancellation;
- typed failure shapes per capability;
- per-field caps, applied after the schema's floor and ceiling, never before;
- local operator policy;
- a closed registry that routes an assignment and advertises exactly the set it
  serves.

Each capability has its own outcome enum. A closed taxonomy whose values are not all
reachable is not closed: an events read cannot produce a container-not-found, a log
read cannot produce an unrepresentable selector. Sharing one enum would be fewer
lines and a weaker claim.

## `kubernetes.workload.runtime` v1

Bounded runtime state of workloads: what is running, its restart counts, exit codes,
and waiting reasons. It reports state, not causes — that is what the two capabilities
below exist for.

## `kubernetes.namespace.events` v1

Bounded, time-windowed Events in one namespace, optionally narrowed to one involved
object by identifier. One bounded page; the continuation-free flag is the
completeness basis.

- **The time window is required at both ends.** An absent bound read as the beginning
  of time would turn a bounded read into an unbounded one through an omission rather
  than a decision.
- **There is no namespace-not-found outcome, deliberately.** The Kubernetes API
  answers a list in a nonexistent namespace with an empty list and a 200.
  Distinguishing the two needs a cluster-scoped namespace read, and
  namespace-scopeable RBAC is worth more than the distinction: the caller already
  knows the namespace exists, because a workload read established it.
- **Event retention is attested, not inferred.** Nobody but the operator knows their
  apiserver's `--event-ttl`, so `RELAY_EVENT_RETENTION` is customer-authored. The
  result reports both the horizon applied and whether the window reached past it.
  The default is the Kubernetes default of one hour — the conservative direction: it
  can cost a certified absence and can never fabricate one.

## `kubernetes.container.logs` v1

Bounded output from one container of one pod, with an explicit flag selecting the
previous terminated instance.

- **Both bounds are enforced and neither substitutes for the other.** A line cap
  alone is defeated by one very long line; a byte cap alone makes the result
  unpredictable in shape.
- **The container is resolved from the pod, not from the log endpoint's error
  message.** A wrong container name and a container with no previous instance both
  come back as a 400 with different free text; a taxonomy decided by string matching
  silently reclassifies itself the first time a Kubernetes release rewords one.
- **The read fetches more than it emits.** The API seeks to the tail and streams
  forward, so a byte limit passed straight through cuts the end of the log — the part
  that explains a crash. The Relay fetches a bounded multiple and keeps the newest
  lines that fit. Nothing beyond the emitted bound leaves the cluster; the overdraft
  bounds memory, not disclosure.

## The gate on real data

These capabilities may run against synthetic scenario clusters immediately, and
against no cluster containing real data until Relay-side redaction exists
(`docs/redaction.md`). Applications print secrets into logs constantly and nobody
notices until something reads them. The ordering is deliberate and lives in the
release checklist, not in anyone's memory.

## Structurally absent

Not deferred — absent by design, permanently:

- no follow, stream, or tail-forever mode;
- no log search, content filter, or regular expression — a content filter is a query
  language, and a query language is the generic surface a closed registry exists to
  prevent;
- no metrics, no traces, no cross-namespace or cluster-wide event queries.
