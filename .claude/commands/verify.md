# /verify — Run the real verification gates

Run the gates relevant to the change and quote actual output. Never report a gate
you did not run; never summarize away a failure. The Makefile is the executable
authority — see the AGENTS.md verification table for what exists.

## Always (any code change)

```
make lint    # buf lint + buf format + gofmt + go vet
make build   # CGO_ENABLED=0, trimpath — the shipped-artifact build
make test    # go test -race ./... (requires a C toolchain: -race needs cgo)
```

On a machine without a C compiler, run `go test ./...` locally and state that the
race gate ran in CI only — never report `make test` as passed when it did not run.

## When proto/ changed

```
make gen-check   # regeneration produces no uncommitted drift
make breaking    # against the committed baseline
make descriptor  # refresh the control-plane sync descriptor
```

## When dependencies changed

```
go mod tidy && git diff --exit-code go.mod go.sum   # tidy is committed
go mod verify
```

CI additionally runs govulncheck, the license check (non-blocking until first-party
packages exist), and the secret scan; run locally when touching dependencies or CI
itself.

## Reporting

State each gate run with its exact result. On failure: stop, quote the output,
diagnose the root cause — do not retry blindly, do not weaken the gate, and do not
proceed to further increments on a red baseline. Gates not yet present in this
repository (golangci-lint, Helm lint, k3s/parity suites) must not be claimed as
run; they arrive with their owning slices per AGENTS.md.
