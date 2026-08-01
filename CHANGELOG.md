# Changelog

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). This project has no
releases yet; nothing here fabricates a release history.

## [Unreleased]

### Added

- **Relay-side redaction.** One enforcement point between a capability's typed result and its
  serialization; nothing reaches the wire without passing through it. Built-in defaults are on
  from the first install and cover high-confidence shapes only — private key blocks, authorization
  headers, bearer tokens, JSON Web Tokens, cloud access key identifiers and secret keys,
  credentialed connection strings, and `password=`-style assignments. A customer-authored policy
  under a `redaction:` root may add patterns, exclude named fields categorically, and lower the
  bounds; it cannot disable a built-in rule, widen a bound, or be sent from the control plane, and
  strict decoding makes those refusals structural rather than conventional. A policy that does not
  parse, names an unknown field, or carries a pattern that will not compile fails closed: every
  capability whose result could carry free text is refused, and it never falls back to the
  defaults. Masking replaces the value with a fixed marker carrying the rule identifier — never a
  partial reveal, a hash, or a length-preserving placeholder. Redaction runs before the Relay's own
  logging, so the audit line measures the masked result. See `docs/redaction.md`.

- Free-text fields are **declared** rather than inferred, and a build gate fails when a capability
  message adds a string field nobody classified. That is what makes it impossible for a capability
  added later to emit unredacted text by forgetting something.

- `-redaction-dry-run` evaluates the effective policy against operator-supplied sample text
  locally — no control plane, no cluster, no identity — and reports what would be masked. It
  prints the masked text and never the original.

- Contract: `CapabilityResult` carries a `RedactionReport` of per-field masked counts and matching
  rule identifiers, and never a value. The control plane turns each into a CoverageGap on the
  EvidenceItem that carried it, so masking is visible rather than indistinguishable from absence,
  and a masked field can never support a certified absence. The occurrence count is a deliberate,
  bounded side channel and is stated as one. The change is additive and `buf breaking` passes
  against `main`. **The contract is not yet tagged**: consumers need `gen/go/v0.3.0`, and until it
  is pushed the control plane resolves the module through a `replace` directive pointing at a
  sibling checkout.

- `RELAY_REDACTION_POLICY_FILE` names the policy. A path that is set and cannot be read is a fault,
  not a fallback.

### Fixed

- A customer-authored volume cap below a capability's own compiled floor was being raised by
  that floor, so an operator who asked for fewer pods than the schema's minimum silently got
  the minimum. Bounds are now clamped to the schema range first and lowered by the local cap
  last, which is the only order in which local policy can never widen a read.

### Changed

- The generated contract under `gen/go` is now its own Go module,
  `github.com/open-cluster/oc-relay/gen/go`, requiring only gRPC and protobuf. A consumer
  that speaks the protocol no longer inherits client-go and the Kubernetes libraries, so
  advisories and license obligations from those stop propagating to it. Import paths are
  unchanged because the module path is the directory that holds it. A build-failing gate
  fails if the contract ever reaches `k8s.io/`, and the per-module gates in the Makefile
  and CI name the nested module explicitly, since `./...` does not cross a module
  boundary. Contract versions are tagged `gen/go/vX.Y.Z`.

- Pre-release protocol taxonomy pass (no deployment existed): the capability is now
  `kubernetes.workload.runtime` (was `kubernetes.runtime` — "runtime" alone collides
  with the container-runtime meaning in Kubernetes vocabulary); its proto file and
  envelope `oneof` fields renamed to match; the redundant `RegisterResponse.relay_id`
  removed (the identity is the pair org id + registration id) with field 1 reserved;
  the claimed organization id now rides in `Register` call metadata
  (`opencluster-org-id`) and clients verify the response's org echo. The capability
  naming convention and per-message semantics are documented in `docs/protocol.md`.

### Added

- Two capabilities, `kubernetes.namespace.events` v1 and `kubernetes.container.logs` v1,
  each with frozen typed messages, its own closed outcome enum, and a completeness basis the
  control plane consumes. Events carry a required half-open window, optional narrowing to one
  involved object by identifier, and the operator's attested retention horizon so an empty
  window is never read as an absence. Logs are bounded in lines AND in bytes with both
  effective values reported, select the previous terminated container explicitly, and have no
  follow, stream, or tail-forever field anywhere in the schema. The envelope gains two
  additive `oneof` variants; nothing in the existing capability changed.
- `internal/capabilities`: the contract every capability obeys — the execution-budget
  discipline that separates a timeout from a cancellation, the typed failure shapes, the
  per-field caps on strings admitted from a source, the customer-authored local policy, and
  the closed registry that routes an assignment to its compiled executor while advertising
  exactly the set it can serve.
- Customer-authored local policy: `RELAY_LOCAL_MAX_EVENTS`, `RELAY_LOCAL_MAX_LOG_LINES`,
  `RELAY_LOCAL_MAX_LOG_BYTES`, `RELAY_ALLOWED_NAMESPACES` and `RELAY_EVENT_RETENTION`. Every
  one narrows and none widens, and the ceilings are pinned by test against the capability
  schema maximums they mirror.
- Protocol v1 contracts under `proto/opencluster/relay/v1/`: registration
  (`RelayRegistrationService`), the bidirectional session stream
  (`RelaySessionService` with closed `oneof` envelopes), and the
  `kubernetes.workload.runtime` v1 capability schema.
- Buf toolchain (lint, breaking-change baseline, deterministic generation with pinned
  plugins) and committed generated Go under `gen/go`.
- Repository governance documents (Apache-2.0, security policy, contributing/DCO,
  code of conduct, support, this changelog).
