# Changelog

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). This project has no
releases yet; nothing here fabricates a release history.

## [Unreleased]

### Added

- Protocol v1 contracts under `proto/opencluster/relay/v1/`: registration
  (`RelayRegistrationService`), the bidirectional session stream
  (`RelaySessionService` with closed `oneof` envelopes), and the
  `kubernetes.runtime` v1 capability schema.
- Buf toolchain (lint, breaking-change baseline, deterministic generation with pinned
  plugins) and committed generated Go under `gen/go`.
- Repository governance documents (Apache-2.0, security policy, contributing/DCO,
  code of conduct, support, this changelog).
