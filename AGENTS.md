# Repository Agent Contract

This file is the canonical instruction authority for AI agents and automation working
in this repository. It is tool-agnostic. `CLAUDE.md` is a thin adapter on top of this
file; the `.claude/` directory holds the workflow commands and reviewer definitions
that implement it. This repository's harness is self-contained: nothing here imports
from, references, or depends on any private repository.

---

## Product boundary

- The Relay executes typed, versioned, bounded, read-only capability jobs. Nothing else.
- The Relay does not investigate, does not reason, and holds no incident state.
- The Relay never receives AI prompts, hypotheses, or model output of any kind.
- The Relay has no model-provider access and no AI dependencies.
- The Relay never becomes a remote shell: no command strings, no scripts, no plugins,
  no dynamic code paths. Every capability is a compiled Go handler with a frozen
  Protobuf schema. This is enforced mechanically (see Security rules), not by
  convention.

## Repository boundary

- This repository is open-source by design (Apache-2.0). Write every file as if it
  were public today, even while pre-release: no private repository paths, no internal
  hostnames, no credentials, no customer names, no unpublished product strategy.
- The OpenCluster control plane is a separate, private codebase. No source file is
  ever copied from it into this repository. Concepts and contracts may be
  reimplemented cleanly.
- Protobuf under `proto/` is the public wire boundary and the only coordination
  artifact shared with the control plane (consumed there via a synced copy with a
  digest manifest). Private investigation internals must not leak into this
  repository's schemas, code, or documentation.
- Publication is founder-gated: no push to a public remote, no releases, no image or
  module publication, and no registry writes without explicit authorization. CI is
  verification-only by design.

## Architecture invariants

- Go (this repository) owns customer-local Kubernetes execution.
- The control plane owns scheduling, authorization, durable truth, evidence
  validation, and all AI. PostgreSQL on the control plane is the durable job source
  of truth.
- The gRPC connection is outbound from the Relay to the control plane, over TLS 443.
- The gRPC session is a delivery channel only. Losing the stream must never lose or
  silently complete a job; correctness lives in the durable store and its lease
  fencing (server-clock leases, lease epochs, terminal-status guards,
  record-before-ack), not in the stream.
- The control plane's own Kubernetes read path is temporary parity infrastructure
  for this migration and will be removed. Do not design anything here that assumes
  it stays.
- Execution locality vocabulary is `control_plane` | `relay`. The word `direct` is
  banned from new public contracts.

## Go engineering rules

- Idiomatic Go, formatted by `gofmt`, passing `go vet`. Follow effective-Go naming;
  no stuttering package names.
- Packages are cohesive and small, grouped by responsibility. Forbidden package
  names and shapes: `utils`, `common`, `base`, `helpers`, generic managers,
  service locators, abstract factories, repository wrappers with no persistence
  behavior.
- Define interfaces at the consumer, only when a second implementation or a test
  seam is actually needed. Prefer concrete types. No interface-per-struct.
- No mutable global application state. No hidden work in `init`. No
  reflection-driven dependency injection — the composition root in `cmd/` wires
  dependencies explicitly and readably.
- Errors: no panic for expected failures; wrap causes with `%w`; error strings are
  lowercase without trailing punctuation. Errors translate into stable protocol
  outcomes at exactly one boundary (the capability/session mapping layer), nowhere
  else.
- Concurrency: every goroutine has an owning struct or function and a proven
  shutdown path; every queue, retry loop, result buffer, and concurrency limit is
  explicitly bounded; `context.Context` is the first parameter of blocking work and
  is never stored in a struct unless a framework boundary forces it (design-reviewed
  exception). Streams follow a single-sender-goroutine discipline.
- No goroutine is created without either a lifecycle test or clearly documented
  ownership in the owning type's doc comment.
- Tests: table-driven where the case list is the point; behavior over
  implementation; independent (no shared mutable state, no ordering, no `sleep`
  timing); names describe the scenario. The race detector runs on every test
  invocation (`make test`).
- Comments state constraints the code cannot show. No narration of the next line,
  no session history, no plan-section markers, no commented-out code.

## Protocol rules

- `proto/opencluster/relay/v1/` is authoritative. Generated code under `gen/` is
  never edited by hand; `make gen` owns it and CI fails on drift (`make gen-check`).
- No hand-written Go or C# transport DTOs duplicating Protobuf messages. Domain
  mapping happens at one boundary.
- No `google.protobuf.Any`, `Struct`, `Value`, or arbitrary map payloads in
  capability schemas. Closed `oneof` envelopes only.
- Capability request/response messages are versioned; released capability schemas
  are frozen — a semantic change mints a new versioned message, never mutates an
  existing one. `make breaking` gates this against the committed baseline.
- Unknown fields in capability payloads are refused (recursive unknown-field walk;
  `DiscardUnknown` is banned). Transport envelopes evolve additively.

## Security rules

The no-command guarantee is mechanical. The following are forbidden in Relay code
and enforced by build-failing gates as they land (each gate arrives with the slice
that introduces the surface it guards — see Verification):

- `os/exec`, shell interpreters, dynamic plugins (`plugin`), scripts, command
  templates, arbitrary executable or filesystem paths, arbitrary URLs;
- Kubernetes `exec`, `attach`, `port-forward`, and every write-capable Kubernetes
  verb; host mounts; general Secret access (only the Relay's own credential Secret);
- unbounded Kubernetes lists (every list is capped and its completeness basis
  recorded);
- `unsafe`, `//go:linkname`, `import "C"` (the shipped binary builds with
  `CGO_ENABLED=0`);
- credentials, tokens, or raw source excerpts in logs, errors, or results.

Additional standing rules: dependencies are pinned and license-gated; GitHub Actions
are pinned by commit SHA; secrets never live in this repository (`.gitignore` guards
`.env`; gitleaks runs in CI); Helm/manifests (when they land) follow the hardening
baseline in the architecture documentation, not ad-hoc defaults.

## Verification commands

These are the real, existing commands. The Makefile is the executable authority;
agent commands delegate to it.

| Purpose | Command |
| --- | --- |
| Install pinned proto toolchain | `make tools` |
| Lint: buf lint + buf format + gofmt + go vet | `make lint` |
| Regenerate protocol code | `make gen` |
| Generated-code drift gate | `make gen-check` |
| Build (CGO disabled, trimpath) | `make build` |
| Tests with race detector | `make test` |
| Protobuf breaking-change gate | `make breaking` |
| Descriptor set for control-plane sync | `make descriptor` |

CI (`.github/workflows/ci.yml`) additionally runs `govulncheck`, a dependency
license check (advisory until first-party packages land, then hard-fail), and a
full-history secret scan.

Not yet present (do not claim or invoke them): golangci-lint config and the
banned-API gates, Helm chart and its lint, container build, k3s integration tests,
differential-parity harness, SBOM generation. Each arrives with its owning slice and
must be added to this table in the same change.

## Workflow

The execution loop, implemented by the `.claude/commands/` files:

`/context` → `/plan` → `/plan-review` → `/tdd` (RED → GREEN → REFACTOR) →
`/verify` → `/security-review` (when the change touches a security surface) →
`/exit-review`

- No approved plan → no implementation. One failing test at a time; if a test
  passes in RED, fix the test first.
- The plan is the source of truth. If implementation must deviate, stop, update the
  plan, and re-run `/plan-review`.
- Slice plans for work in this repository live in `plans/` here and must satisfy
  the repository boundary above (public-safe). Work that touches private
  control-plane internals is planned in the control-plane repository, not here.
- Every completion claim carries evidence: the command run and its actual output.
  No claim without test output.
- Preserve user work. No reset, clean, stash, history rewrite, force push, or
  destructive Git action without explicit authorization.
- Self-check before declaring any increment done: approved plan? (YES/NO) — tests
  written first and failing for the right reason? (YES/NO) — any deviation from
  the plan? (YES/NO). A wrong answer invalidates the claim; stop and repair the
  workflow, not the wording.
- Commit style and contribution rules: `CONTRIBUTING.md`. Documentation changes
  update only affected sections and never regenerate whole documents; no
  placeholders, no filler.
