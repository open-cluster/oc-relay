# /plan-review — Independent adversarial review of a plan

A plan is approved only by a fresh-context reviewer that actively tried to disprove
it — not by the context that wrote it.

## Procedure

1. Spawn the reviewer(s) whose scope matches the plan, as fresh-context subagents
   pointed at the plan file and the repository:
   - Go structure/tests → `go-reviewer`
   - session, leases, acks, reconnect → `grpc-distributed-systems-reviewer`
   - kube access, credentials, manifests, no-command surface →
     `kubernetes-security-reviewer`
   - anything under `proto/` → `protocol-compatibility-reviewer`
   - dependencies, CI, docs, publication surface → `oss-supply-chain-reviewer`
   Pick the minimal set that covers the plan; do not stack overlapping reviewers.
2. Instruct each reviewer to disprove the plan, not to polish it:
   - Is every section present and concrete (a missing failure-semantics or
     security-impact section is an automatic REQUEST CHANGES)?
   - Is any step vague enough to be implemented two different ways?
   - What failure sequence, restart, or malicious input does the plan not account
     for?
   - Does the plan contradict AGENTS.md, docs/protocol.md, or an earlier approved
     plan?
   - Is the RED list sufficient to pin the claimed behavior?
   - Does anything violate the public-repository boundary?
3. Fold results back: fix every BLOCKER in the plan, re-run the affected reviewer
   on the revised plan, and record the review round in the plan's Status header
   (reviewer, verdict, revision).

## Verdict

APPROVE or REVISE REQUIRED, with findings listed by severity. Implementation may
start only at APPROVE, and only on the reviewed revision.
