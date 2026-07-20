# /tdd — RED → GREEN → REFACTOR

No production code without a failing test that demanded it. One failing test at a
time. Applies to behavior changes; comment-only or documentation-only edits do not
require artificial tests.

## RED

1. Take the next behavior from the approved plan's RED list.
2. Write ONE failing test that pins it. Table-driven when the case list is the
   point; the test name describes the scenario.
3. Run it: `go test ./<package>/ -run <TestName>` and confirm it fails **for the
   expected reason** (missing behavior — not a compile typo, not setup error).
   Quote the failure output. If it passes in RED, the test is invalid: fix the
   test first.

## GREEN

4. Implement the minimum code that makes this test pass. No anticipation of future
   increments, no drive-by refactors.
5. Run the focused test, then the package: `go test ./<package>/`. Quote output.

## REFACTOR

6. Only while green: improve names, extract duplication, tighten bounds. Behavior
   unchanged — no test edits during refactor (if a test must change, that is a
   behavior change; go back to RED).
7. Full verification: `make test` (race detector) and `make lint`. Quote output.

## Adversarial check

For non-trivial logic, before declaring the increment done, attack your own tests:
which mutation of the implementation (flipped comparison, dropped guard, off-by-one
on a bound, swapped precedence) would every test still pass through? Add the
missing RED for any survivor that matters. For concurrency code, the check is a
test that exercises cancellation/disconnection mid-flight under `-race`.

## Rules

- Tests are independent: no ordering, no shared mutable state, no sleep-based
  timing.
- Never edit `gen/` to make a test pass; fix the source (`proto/` + `make gen`) or
  the code.
- A skipped or ignored test is a failing test.
- Refusal paths get asserted as refusals (explicit refusal outcome), not as "does
  not crash".
