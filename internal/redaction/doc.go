// Package redaction is the one point between a capability producing a typed result and that
// result being serialized onto the session stream. Nothing reaches the wire without passing
// through it.
//
// Applications print secrets into logs constantly and nobody notices until something reads
// them. The container-logs capability exists because the decisive artifact in most workload
// failures is the application's own account of what happened, and that account routinely
// carries a connection string, a bearer token, or a credential echoed by a misconfigured
// client. This package is what stops that text leaving the customer's cluster.
//
// # Why one point rather than one per capability
//
// A capability applies no masking of its own and none can opt out. The consequence to design
// for is that free-text fields are DECLARED (see declared.go) rather than guessed from the
// type, so the sweep knows what to read rather than inferring it — and a capability message
// that adds a string field nobody classified fails the build in internal/gates and fails
// closed here. Safety is structural rather than a checklist item at review time.
//
// # What is reported, and what is never reported
//
// Every masked occurrence is counted and attributed to the rule that matched it. The value is
// never sent, never hashed, and never partially revealed: a partial reveal leaks the part that
// identifies the secret, a digest of a low-entropy value is recoverable by brute force, and a
// length-preserving placeholder leaks length. The occurrence count is a deliberate, bounded
// side channel — reporting that a field held four masked values tells the platform something
// it would not otherwise know, and the alternative makes masking indistinguishable from
// absence, which is the more serious failure.
//
// # Fail closed
//
// A policy that does not parse, names an unknown field, or carries a pattern that will not
// compile is a Fault. A Fault stops execution of every capability with a declared free-text
// field rather than falling back to the built-in defaults: a customer who wrote a policy has
// signalled that the defaults are insufficient for them, so silently serving the defaults
// would send out exactly what they asked to keep in.
//
// # Deterministic and pure
//
// The same input and the same policy always produce the same output. Recorded model
// transcripts depend on it, and so does comparing two runs of one scenario. The cost bounds
// preserve this rather than breaking it: a result that exceeds them produces a Fault and no
// output at all, never a partially swept result — skipping a pattern is silent disclosure.
package redaction
