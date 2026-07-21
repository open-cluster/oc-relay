# Relay protocol v1

Source of truth: `proto/opencluster/relay/v1/`. Protobuf is authoritative for the
transport and every capability schema; there is no competing OpenAPI/JSON contract.

## Services

- `RelayRegistrationService.Register` — one-time bootstrap exchange. The response
  delivers the org id, registration id, durable credential (once), and the initial
  SPKI pin set.
- `RelaySessionService.Connect` — one outbound bidirectional stream carrying closed
  `oneof` envelopes (`RelayToControl` / `ControlToRelay`).

## Call metadata

Identity scope and secrets ride in call metadata; message fields carry attestations
only. Servers never log or trace call metadata on these services.

| Key | Call | Content |
| --- | --- | --- |
| `opencluster-bootstrap-token` | `Register` | Single-use bootstrap token (secret) |
| `opencluster-org-id` | `Register`, `Connect` | Claimed organization id (scope, not proof) |
| `opencluster-registration-id` | `Connect` | Registration id being authenticated |
| `opencluster-relay-credential` | `Connect` | Durable relay credential (secret) |

Register refusals — malformed, unknown, expired, consumed, revoked — all surface as
one `FAILED_PRECONDITION` with identical detail; clients treat it as terminal
(re-bootstrap). An intake shed (flood limit) is distinct: `RESOURCE_EXHAUSTED`,
retryable, applied before the token is examined so it carries no validity signal.

## The durable-truth rule

PostgreSQL on the control plane owns durable job truth. The gRPC stream is only a
delivery channel. Every job is leased and fenced by (job id, server-minted session id,
lease epoch) under the control-plane clock. A disconnect can neither lose nor silently
complete a job: leases expire on the control-plane clock, the periodic sweep re-queues
or dead-letters expired work, the on-connect catch-up scan redelivers, and the Relay's
bounded unacked-result buffer resends until a definitive `ResultAck`.

## Message semantics

Correlation: every job-scoped message carries `job_id` + `lease_epoch` — the fence's
wire round-trip. The server validates (job id, session id as lease owner, epoch) on
every job-scoped Relay message; mismatches lose the fence and are audited, never
recorded. `correlation_id` is a random audit-only token with no tenant semantics.
Session identity is established once from `Connect` metadata; no per-message
credentials exist.

### Relay → control plane

| Message | Sent when | Durable job-state effect | Retry / idempotency | Proves — and does not prove |
| --- | --- | --- | --- | --- |
| `Hello` | First message on every stream | None directly; `in_flight` entries may renew leases for jobs already owned under a valid epoch | Once per stream | Attests the Relay's capabilities, policy hash, and in-flight view; proves nothing about capability health or job completion |
| `Heartbeat` | Every `heartbeat_interval` | None | Not retried; the next interval supersedes | Session liveness only — never job progress; leases expire regardless |
| `JobAck` | On receiving `JobAssignment` | None (the job was durably leased at claim time) | Duplicates ignored | Reception only — not execution |
| `JobStarted` | When execution begins | None | Duplicates ignored | Execution began — not that it will finish |
| `JobResult` | On completion or typed failure | None by itself — durable only when the recording transaction commits | Resent from the unacked buffer until a definitive `ResultAck`; recording is idempotent (terminal row → `ALREADY_RECORDED`) | Carries an outcome; final only after the commit |
| `CancelAck` | After processing `Cancellation` | None — an aborted job's terminal outcome still arrives as a `JobResult` failure (`KIND_CANCELLED`): one write path into truth | Duplicates ignored | The cancel was processed — not that the job is durably closed |
| `DrainState` | Periodically while draining | None | Superseded by the next report | Progress report only |
| `RotationConfirm` | After durably persisting the successor credential | Server revokes the predecessor only after this | Resend safe (revocation idempotent) | Successor persisted — not that the predecessor is unused elsewhere |
| `ProtocolError` | Before disconnecting on a violation | None | None | Reports why; leases and the sweep own job recovery |

### Control plane → relay

| Message | Sent when | Durable job-state effect | Retry / idempotency | Proves — and does not prove |
| --- | --- | --- | --- | --- |
| `SessionAccepted` | After credential verification and `Hello` validation | None (the minted session id becomes the lease owner for later claims) | Once per stream | Session established — no job promises |
| `JobAssignment` | Only after the durable claim transaction commits (queued → leased) | The claim preceded it; the message is pure delivery | Redelivered by sweep / on-connect catch-up; Relay dedups by (job id, epoch) | Delivery attempted — reception is `JobAck`'s claim |
| `Cancellation` | When the control plane wants the job stopped | None; the job stays leased until recorded or expired | May resend; duplicates ignored | A request only — a produced result still wins |
| `ResultAck` | ONLY after the recording transaction commits (`RECORDED` / `ALREADY_RECORDED`) or a post-commit stale classification (`STALE_STOP_RESENDING`) | The commit preceded it | Re-answering a resent result is idempotent | The job's durable fate is settled |
| `CredentialRotation` | Rotation window open | Predecessor stays valid until `RotationConfirm` | Resent until confirmed | Delivery of the successor — not its adoption |
| `DrainInstruction` | Before shutdown or rebalancing | None; unfinished work recovers via lease expiry | May resend | Instruction delivered |
| `CapabilityRequirements` | On connect and on change | None | Superseded by the next | Advisory minimums; a missing capability surfaces centrally as a coverage gap, never a silent skip |
| `GracefulReconnect` | Deploys, rebalancing | None | n/a | Server-directed reconnect with `retry_after`; an unexplained GOAWAY re-enters normal backoff and never resets it |

## Load-bearing rules

- Envelope evolution is additive; capability messages are strict and frozen — any
  semantic change mints a new versioned message. Receivers refuse capability
  payloads carrying unknown fields (recursive check; `DiscardUnknown` forbidden).
- Banned constructs (CI schema-shape gate): `google.protobuf.Any`, `Struct`,
  `Value`, map-based argument payloads, raw JSON payloads, command strings, scripts,
  executable paths, dynamic method names.
- The Relay verifies org id AND registration id on every assignment against its
  bootstrap-bound identity; mismatches are refused and audited.
- All Relay-supplied timestamps are provenance only; deadlines are duration budgets
  measured on the Relay's monotonic clock.

## Capabilities

### Naming convention

- Identifier: two or more lowercase dot-separated segments, general → specific —
  `kubernetes.workload.runtime`, `kubernetes.container.logs`, `diagnostics.dns`.
- Version: the separate integer `capability_version`; the canonical rendered form
  appends `.v<version>` — `kubernetes.workload.runtime.v1`.
- Messages: PascalCase segments + `{Args,Result}V<N>` —
  `KubernetesWorkloadRuntimeArgsV1`.
- Envelope `oneof` fields: snake_case segments + `_v<n>` —
  `kubernetes_workload_runtime_v1`.
- One proto file per capability family — `kubernetes_workload_runtime.proto`.
- Frozen once released: a semantic change mints `V<N+1>` messages and a new version
  integer, never an edit in place.

### Registry

| Capability | Version | Schema |
| --- | --- | --- |
| `kubernetes.workload.runtime` | 1 | `kubernetes_workload_runtime.proto` — workload runtime read: closed workload-kind vocabulary, complete selector semantics with refuse-on-unrepresentable, per-kind replica counters, pod/container states with OOM detection, single bounded page with continuation-free completeness basis |

Each capability's documentation states exactly what data leaves the cluster.
