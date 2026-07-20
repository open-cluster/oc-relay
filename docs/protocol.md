# Relay protocol v1

Source of truth: `proto/opencluster/relay/v1/`. Protobuf is authoritative for the
transport and every capability schema; there is no competing OpenAPI/JSON contract.

## Services

- `RelayRegistrationService.Register` — one-time bootstrap exchange. The single-use
  bootstrap token rides in call metadata over TLS; the response delivers the relay
  id, org id, registration id, durable credential (once), and the initial SPKI pin
  set.
- `RelaySessionService.Connect` — one outbound bidirectional stream carrying closed
  `oneof` envelopes (`RelayToControl` / `ControlToRelay`). The relay credential rides
  in call metadata on every Connect.

## Load-bearing rules

- The stream is a delivery channel. Durable job state lives in the control plane's
  database; every job is leased and fenced by (job id, server session id, lease
  epoch) under the control-plane clock. A disconnect can neither lose nor silently
  complete a job: pending jobs are found by the server's periodic sweep and the
  on-connect catch-up scan; results are retained in a bounded Relay-side buffer
  until a definitive `ResultAck`.
- `JobAssignment` carries the lease epoch; `JobResult` echoes it. Late results from
  a previous epoch lose the fence and are audited.
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

| Capability | Version | Schema |
| --- | --- | --- |
| `kubernetes.runtime` | 1 | `kubernetes_runtime.proto` — workload runtime read: closed workload-kind vocabulary, complete selector semantics with refuse-on-unrepresentable, per-kind replica counters, pod/container states with OOM detection, single bounded page with continuation-free completeness basis |

Each capability's documentation states exactly what data leaves the cluster.
