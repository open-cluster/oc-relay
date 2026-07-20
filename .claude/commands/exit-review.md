# /exit-review — Independent verification before declaring done

The implementing context is never the only authority declaring work VERIFIED. When
an increment or slice claims completion, a fresh-context reviewer compares the
claim against reality.

## Procedure

1. Spawn the scope-matching reviewer(s) (see /plan-review's routing table) as
   fresh-context subagents. Give them only: the approved plan file, the commit
   range or diff, and this checklist — not the implementing context's summary.
2. The reviewer compares, with evidence for every line:
   - **Approved plan vs actual diff** — every planned step present; every
     unplanned change flagged (unplanned = BLOCKER until the plan is updated and
     re-reviewed).
   - **Tests** — each RED-list behavior has a test; the reviewer re-runs
     `make test` and `make lint` themselves and quotes the output; claimed counts
     match observed counts.
   - **Generated contract** — `make gen-check` clean; `make breaking` clean; no
     hand edits under `gen/`.
   - **Security rules** — the AGENTS.md banned list grepped against the new code;
     new surfaces have their gates.
   - **Docs** — affected sections updated, nothing overclaimed, no dead command
     references.
   - **Remaining scope** — what the plan still owes; nothing silently dropped.
3. Verdict: VERIFIED, or REQUEST CHANGES with findings by severity. Fix and
   re-review BLOCKERs; record the verdict, reviewer, and evidence in the plan's
   Status header.

## Rules

- A claim without command output is not evidence.
- "Tests pass" means the reviewer ran them, not that the implementer said so.
- Completion language is bounded: report exactly which gates passed and which
  remain — never "done" while any approved gate is open.
