# Changelog

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). This project has no
releases yet; nothing here fabricates a release history.

## [Unreleased]

### Changed

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
