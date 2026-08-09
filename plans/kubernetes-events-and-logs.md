# Plan — Kubernetes events and container logs capabilities

Status: IMPLEMENTED.
Scope: this repository's half — the protocol contract and the two executors. Dispatch,
argument validation before dispatch, and evidence minting are the control plane's half and
are planned there.

## Why

One capability existed. `kubernetes.workload.runtime` reports that a pod is in
`CrashLoopBackOff`, that a container exited 137, and that it has restarted eleven times. It
cannot report why any of that happened, so anything built on it alone reaches the same
conclusion for every failure — the one the engineer already had when they were paged.

The two reads that carry the answer in the overwhelming majority of Kubernetes workload
failures are the cluster's own account of what it did, and the application's own account of
what it said before it died.

## What was built

**`kubernetes.namespace.events` v1.** Bounded, time-windowed Events in one namespace,
optionally narrowed to one involved object by identifier. One bounded page; the
continuation-free flag is the completeness basis. The window is required at both ends — an
absent bound read as the beginning of time would turn a bounded read into an unbounded one
through an omission rather than a decision.

**`kubernetes.container.logs` v1.** Bounded output from one container of one pod, with an
explicit flag selecting the previous terminated instance. Both bounds are enforced and
neither substitutes for the other: a line cap alone is defeated by one very long line, and a
byte cap alone makes the result unpredictable in shape.

**`internal/capabilities`.** What every capability obeys: the budget discipline separating a
timeout from a cancellation, the typed failure shapes, the per-field caps, the local policy,
and the closed registry that routes an assignment and advertises the same set it serves.

## Decisions worth not relitigating

**Each capability has its own outcome enum.** A closed taxonomy whose values are not all
reachable is not closed. An events read cannot produce a container-not-found; a log read
cannot produce an unrepresentable selector. Sharing one enum would have been fewer lines and
a weaker claim.

**Events has no namespace-not-found, deliberately.** The Kubernetes API answers a list in a
namespace that does not exist with an empty list and a 200. Distinguishing the two needs a
cluster-scoped namespace read, and namespace-scopeable RBAC is worth more than the
distinction: the caller already knows the namespace exists, because a workload read
established it.

**Event retention is attested, not inferred.** Nobody but the operator knows their
apiserver's `--event-ttl`, so `RELAY_EVENT_RETENTION` is customer-authored and the result
reports both the horizon applied and whether the window reached past it. The default is the
Kubernetes default of one hour, which is the conservative direction: it can cost a certified
absence and can never fabricate one.

**The container is resolved from the pod, not from the log endpoint's error message.** A
wrong container name and a container with no previous instance both come back as a 400 with
different free text. A taxonomy decided by string matching silently reclassifies itself the
first time a Kubernetes release rewords one of them.

**The log read fetches more than it emits.** The API seeks to the tail and streams forward,
so a byte limit passed straight through cuts the END of the log — the part that explains a
crash. The Relay fetches a bounded multiple and keeps the newest lines that fit. Nothing
beyond the emitted bound leaves the cluster; the overdraft bounds memory, not disclosure.

**Local caps are applied last.** After the schema's floor and ceiling, never before. This
was a real defect in the existing workload capability: a cap below the compiled floor was
being raised by it.

## The gate on real data

These capabilities may be used against synthetic scenario clusters immediately and against
no cluster containing real data until Relay-side redaction exists. Applications print
secrets into logs constantly and nobody notices until something reads them. The order is
deliberate and belongs in the release checklist rather than in someone's memory.

## What is not here

No follow, stream, or tail-forever mode — structurally absent and permanently so. No log
search, content filter, or regular expression: a content filter is a query language, and a
query language is the generic surface a closed registry exists to prevent. No metrics, no
traces, no cross-namespace or cluster-wide event queries. No third capability.
