# Changelog

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). This project has no
releases yet; nothing here fabricates a release history.

## [Unreleased]

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

- Protocol v1 contracts under `proto/opencluster/relay/v1/`: registration
  (`RelayRegistrationService`), the bidirectional session stream
  (`RelaySessionService` with closed `oneof` envelopes), and the
  `kubernetes.workload.runtime` v1 capability schema.
- Buf toolchain (lint, breaking-change baseline, deterministic generation with pinned
  plugins) and committed generated Go under `gen/go`.
- Repository governance documents (Apache-2.0, security policy, contributing/DCO,
  code of conduct, support, this changelog).
