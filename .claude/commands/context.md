# /context — Reconstruct working state

Rebuild an accurate picture of the repository before planning or implementing.
Evidence only — run the commands, do not recall from memory.

## Steps

1. Git state: `git status`, `git log --oneline -10`, current branch. Note any
   uncommitted user work — it must be preserved.
2. Plan state: list `plans/` (if present) and read the status header of the active
   slice plan. Identify which increments are marked done and which are next.
3. Protocol state: `ls proto/opencluster/relay/` for the current version;
   `make gen-check` for generated-code drift; `make breaking` if contract work is
   in flight.
4. Code state: list packages (`go list ./...`); note which capability and session
   packages exist versus which the plan still expects.
5. Health: `make lint` and `make test` — record pass/fail with output. A failing
   baseline blocks new work until explained.
6. Outstanding work: open findings from the last review, unfinished approved tasks,
   and anything the plan marks as blocked.

## Output

A short state summary: branch and HEAD, dirty files, active plan and its next
increment, protocol version and drift status, test/lint baseline (green or the
exact failures), and open blockers. Flag any contradiction between the plan's
claims and what the commands actually show.
