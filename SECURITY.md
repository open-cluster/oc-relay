# Security Policy

This repository is a private pre-release. There are no published releases, no public
artifacts, and no CVE process yet. This policy will be rewritten honestly at
publication; nothing here fakes a coordinated-disclosure program that does not exist.

## Reporting

Report suspected vulnerabilities privately to the maintainer (repository owner). Do
not open public issues for security reports. There is no published response SLA or
PGP key at this stage.

## Design posture (summary)

- Read-only execution: compiled capability registry; no shell, no subprocesses, no
  dynamic code. Enforcement is mechanical and build-failing: the cgo-disabled build
  is wired today, and the remaining gates (import allowlist, symbol bans,
  kubeconfig credential-plugin refusal) land with the capability surfaces they
  guard, before any release.
- Outbound-only connectivity: one TLS stream to a pinned (SPKI) control-plane
  endpoint; no inbound listeners.
- Least privilege: read-only Kubernetes RBAC with a single narrow exception (the
  Relay's own pre-created credential Secret, get/update only).
- Customer-authored local policy: destination allowlist, hard rate/volume caps, and
  masking rules the control plane cannot write.
- Local audit log: every executed job and every egress byte count, customer-visible.

The full threat model lives with the control-plane architecture documentation and is
published alongside the Relay when the repository goes public.
