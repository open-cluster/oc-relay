# /plan — Create a bounded slice or increment plan

Plans are the source of truth: implementation follows the approved plan exactly, and
deviation requires updating the plan and re-running /plan-review. No implementation
without an approved plan.

## Before planning

1. Run /context first. Do not plan against a stale picture.
2. Restate the goal in your own words; extract explicit requirements and implicit
   expectations; list unknowns. If intent is unclear — stop and ask, do not plan.
3. Read the relevant existing plans in `plans/` and the affected packages. Reuse or
   extend an existing plan rather than duplicating it.

## Plan content (all sections required)

- **Goal** — one or two sentences: what and why.
- **Non-goals** — what this slice explicitly does not do.
- **Files** — packages/files to create or modify, each with the behavioral reason.
- **Contracts** — Protobuf messages touched (or "none"); freeze/versioning impact.
- **Failure semantics** — for every new operation: what happens on disconnect,
  timeout, invalid input, and partial completion; which errors map to which
  protocol outcomes.
- **Security impact** — new surfaces against the AGENTS.md security rules; which
  gates cover them or must be added in this slice.
- **Concurrency impact** — new goroutines with owner and shutdown path; every new
  bound (queue, buffer, retry, limit) with its value or sizing rule.
- **Tests** — behaviors to pin (not test code): the RED list, in order.
- **Documentation** — which existing sections change (never whole-file rewrites).
- **Removal/migration impact** — effect on the parity oracle, cutover gates, or
  anything scheduled for removal.

## Rules

- No code, pseudo-code, or function signatures in plans. Describe behavior.
- Steps are numbered, atomic, and executable without interpretation.
- Public-safe only: a plan in this repository must satisfy the AGENTS.md repository
  boundary. Work touching private control-plane internals is planned in the
  control-plane repository instead.
- Store as `plans/<slice-name>.md` with a Status header; keep it current, not
  append-only.

Then run /plan-review. The plan is not approved until a fresh-context review says so.
