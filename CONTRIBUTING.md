# Contributing

This repository is a private pre-release; external contributions are not yet
accepted. This document exists so the ground rules are set before the first external
PR, not after.

## Ground rules (effective at publication)

- Developer Certificate of Origin (DCO): every commit must carry a `Signed-off-by`
  line (`git commit -s`). No CLA.
- License: contributions are accepted under Apache-2.0 only.
- Every change runs the full CI gate locally first: `make lint gen build test`.
- Protocol changes: `proto/` is the source of truth; `buf breaking` must pass against
  the committed baseline; released capability messages are frozen — semantic changes
  mint a new versioned message, never mutate an existing one.
- Security-sensitive surfaces (capability code, the kube client port, identity,
  policy enforcement) require maintainer review regardless of size; the banned-API
  gates are not negotiable and `//nolint` on them is rejected.
- No generated-code edits by hand; `make gen` owns the generated sources under `gen/go`.
  Its `go.mod` and `go.sum` are the exception: they define the contract module and are
  maintained by hand, so keep them to what the generated code actually needs.
- A contract change reaches consumers only when it is tagged `gen/go/vX.Y.Z`. Merging one
  without a tag leaves every consumer on the previous contract with nothing reporting it,
  so tag in the same change that lands the proto edit.

## Commit style

`<type>(<scope>): <description>` — types: feat, fix, refactor, test, docs, chore,
perf, security.
