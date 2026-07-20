# /security-review — Apply the Relay threat model

Run when a change touches a security surface: capability code, the kube client
port, credentials/identity, session/transport, manifests/Helm, CI, or
dependencies. Spawn `kubernetes-security-reviewer` as a fresh-context subagent for
the Kubernetes/no-command surface; add `oss-supply-chain-reviewer` when the change
touches dependencies, CI, or public-facing content.

## The Relay threat model (what the reviewers must test the change against)

- The control plane is a powerful but not blindly trusted peer: a compromised or
  buggy control plane must still be unable to make the Relay execute anything
  outside the frozen capability set, exceed its bounds, or exfiltrate beyond the
  typed result schemas.
- The no-command guarantee is mechanical: banned APIs (os/exec, plugin, unsafe,
  //go:linkname, import "C", kube exec/attach/port-forward, write verbs) stay
  impossible, and every new surface lands with its build-failing gate in the same
  slice.
- Remote input (job arguments) may never select a path, verb, resource kind,
  destination, or executable — only values inside the frozen typed contract.
- Credentials: the Relay's own Secret only; never logged, never in results, never
  in error strings; comparison on the hot path is a fast digest, never a
  memory-hard KDF (DoS).
- Egress: results are typed, bounded projections; size caps enforced Relay-side
  before send; completeness basis recorded when a bound truncates.
- Availability: reconnect storms, unbounded buffers, and decompression/stream
  floods are denial-of-service surfaces — bounds and backoff are security
  controls, not tuning.

## Output

Findings by severity with `file:line`, attack scenario, and fix — per the reviewer
agents' formats. Fix BLOCKERs before the increment proceeds; record the review and
its verdict in the plan's Status header.
