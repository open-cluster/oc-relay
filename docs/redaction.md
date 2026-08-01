# Redaction

Applications print secrets into logs constantly, and nobody notices until something reads them.

The container-logs capability exists because the decisive artifact in most workload failures is
the application's own account of what happened, and that account routinely contains a database
connection string, a bearer token, or an API key echoed by a misconfigured client. Redaction is
what stops that text leaving the cluster.

> **Installation gate.** No cluster containing real data may be connected until redaction is in
> force. The logs capability ships first and is used against synthetic scenario clusters; this is
> the gate on installations, and the person who onboards a design partner is the one who has to
> know it.

## Overview

| Property | Behaviour |
| --- | --- |
| Enforcement point | One, between a capability's typed result and its serialization. No capability applies its own masking and none can opt out. |
| Default policy | On from the first install, no configuration. High-confidence secret shapes only. |
| Customer policy | May only **add**. It cannot disable a built-in rule, widen a bound, or be sent from the control plane. |
| Masked value | Replaced by a fixed marker. Never partially revealed, never hashed, never length-preserving. |
| Reporting | Per field: how many occurrences were masked and which rules matched. Never the value. |
| On a policy fault | Fails closed. Capabilities that could emit free text are refused; it never falls back to the defaults. |
| Determinism | The same input and policy always produce the same output. |

## Quick start

Nothing is required. A Relay with no configuration masks private key blocks, authorization
headers, bearer tokens, JSON Web Tokens, cloud access key identifiers and secret keys,
credentialed connection strings, and `password=`-style assignments.

To add your own shapes, write a policy file and point the Relay at it:

```bash
RELAY_REDACTION_POLICY_FILE=/etc/opencluster/redaction.yaml
```

## The policy file

Nested under a `redaction:` root. Decoding is **strict**: a key this build does not know fails the
parse. That is what makes "a policy may only add" enforceable rather than aspirational — an
operator who writes `disabled_rules:` expecting it to weaken the defaults learns at startup that
there is no such thing, instead of believing a built-in rule is off when it is not.

```yaml
redaction:
  version: 1

  # Your own secret shapes. Evaluated in addition to the built-in set, never instead of it.
  patterns:
    - id: acme.session_cookie
      pattern: 'ACME_SESSION=[A-Za-z0-9]{20,}'
    - id: acme.internal_token
      pattern: 'ACME-[0-9a-f]{32}'

  # Fields excluded categorically rather than by pattern. Named as the coverage gap names them.
  masked_fields:
    - kubernetes_container_logs_v1.lines.content

  # Bounds. Both may only be LOWERED.
  limits:
    sweep_budget: 500ms
    max_input_bytes: 262144
```

| Key | Meaning |
| --- | --- |
| `version` | Format version. Only `1` is understood; an unrecognised version is refused rather than interpreted. |
| `patterns[].id` | Stable identifier, reported in every coverage gap. May not begin `builtin.`. |
| `patterns[].pattern` | RE2 syntax. Matching is linear in the input; no pattern can be made to backtrack. |
| `masked_fields` | Declared free-text fields to replace whole. An unrecognised name is refused. |
| `limits.sweep_budget` | Wall-clock ceiling for one result. Default 2s, may only be lowered. |
| `limits.max_input_bytes` | Total free text one result may present. Default 4 MiB, may only be lowered. |

### File location and permissions

The file governs what may leave the cluster, so treat it as a security control:

- Mount it **read-only**, owned by the account the Relay runs as, mode `0400`.
- A world-readable policy tells any local process exactly which shapes are **not** masked.
- The path is configuration; the file is never read from an environment value, so it cannot leak
  through a process listing or a diagnostic dump of the environment.
- A path that is set and cannot be read is a **fault**, not a fallback: an operator who named a
  file meant it to be enforced.

## Testing a policy before it costs you an investigation

A policy whose breadth is only discoverable by losing an investigation is a policy nobody will
tune. The dry run evaluates the effective policy against your own sample text, locally, with no
control plane, no cluster and no identity:

```bash
opencluster-relay -redaction-dry-run ./sample.log
cat /var/log/app.log | opencluster-relay -redaction-dry-run -
```

It prints the rules in force, how many occurrences each one claimed, and what would actually
leave. It prints the **masked** text and never the original — a tool whose purpose is to stop a
secret being printed must not print it back at you.

## What is masked, and what is never masked

Only **free-text** fields are swept: text some other software wrote and this one only carries.
Today that is a container's log lines and a cluster's own event messages.

Statuses, reasons, phases, exit codes, counts, timestamps, resource names, namespaces, images and
identifiers are the substance of an investigation and carry no secret. They are never swept, and
that is a design constraint on what may become a rule rather than merely a default — a policy that
masked a reason code would destroy the answer while reporting that it protected something.

Which fields are which is **declared**, in `internal/redaction/declared.go`, and a build gate fails
if a capability message adds a string field nobody classified. That is the structural half of the
guarantee: a capability added next year cannot emit unredacted text by forgetting something,
because the code that would have emitted it does not compile.

## What is reported

Each result carries, per masked field, the number of occurrences and the identifiers of the rules
that matched. The control plane records a CoverageGap per masked field on the EvidenceItem that
carried it, so an investigation can say it could not read something because your policy masks it —
and a masked field can never support a certified absence.

The occurrence count is a **deliberate, bounded side channel**, and it deserves one honest
sentence. Reporting that a field contained four masked values tells the platform something it
would not otherwise know. The alternative — reporting nothing — makes masking indistinguishable
from absence, which is the more serious failure: a masked field would read as an empty one, your
own privacy policy would quietly degrade your investigations, and the product would be blamed for
the hole.

## Failure modes

| Situation | What happens |
| --- | --- |
| Policy file does not parse | Fault. Capabilities that could emit free text are refused. |
| A pattern does not compile | Fault, naming the rule. The pattern is never echoed. |
| A rule id begins `builtin.` | Refused at parse time. |
| An unknown key appears | Refused at parse time. |
| A `masked_fields` name is unrecognised | Refused, listing the names that can be used. |
| A limit is raised rather than lowered | Refused. |
| A result exceeds a bound | Refused. The result is dropped whole — a pattern that is skipped is a secret that is sent. |

A refusal reaches the control plane as `KIND_LOCAL_POLICY_REFUSED`, and the reason is in the
Relay's own diagnostics where the operator who wrote the policy can read it. Capabilities whose
results carry no free-text field — `kubernetes.workload.runtime` — keep working under a fault.

## Verifying what is in force

The Relay reports its effective policy at startup: the source, the rules by identifier, the
categorically masked fields, and the bounds. It reports identifiers and counts and never a
pattern — the file governs disclosure, so printing it would make diagnosing the Relay a disclosure
channel of its own.

Redaction runs **before** the Relay's own logging. What the audit line measures is the result
after masking, so the Relay never logs what it just removed and never leaks the length of it.
