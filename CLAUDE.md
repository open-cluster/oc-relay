# Claude Code Adapter

Read `AGENTS.md` first — it is the canonical authority for this repository (product
boundary, architecture invariants, Go/protocol/security rules, and the real
verification commands). This file only adds how Claude Code specifically operates
here. Do not duplicate `AGENTS.md` content into this file.

## Operating rules

- Follow the `AGENTS.md` workflow loop via the `.claude/commands/` files:
  `/context`, `/plan`, `/plan-review`, `/tdd`, `/verify`, `/security-review`,
  `/exit-review`.
- Plan and exit reviews run in fresh-context subagents (`.claude/agents/`), never
  in the implementing context. The implementing context is never the only authority
  declaring work VERIFIED. Pick the reviewer whose scope matches the change; do not
  stack overlapping reviewers.
- Evidence before claims: quote the actual command output for every build, test,
  lint, or gate result you assert. If you did not run it, say so.
- Debugging discipline: reproduce first; trace the call chain instead of guessing;
  state the root cause with evidence before fixing; make the minimal fix; then look
  for sibling instances of the same defect.
- Never edit files under `gen/` — `make gen` owns them (also enforced by settings
  deny rules).
- Never publish: no push to public remotes, no releases, no image/module
  publication. Founder authorization is required first.
- Preserve user work; no destructive Git operations without explicit authorization.
- Plan integrity: if implementation diverges from the approved plan, stop, update
  the plan, and re-run `/plan-review` before continuing.
