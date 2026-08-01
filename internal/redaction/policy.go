package redaction

import (
	"fmt"
	"time"
)

// Bounds on what one sweep may cost. They are stated here rather than left implicit because a
// pathological result must degrade into a refusal, never into a capability that hangs.
const (
	// DefaultBudget is the wall-clock ceiling for sweeping one result. It is far above what the
	// capability ceilings can produce — the largest permitted log read is 256 KiB — so reaching
	// it means something is wrong rather than something is large.
	DefaultBudget = 2 * time.Second
	// DefaultMaxInputBytes bounds the total free text one result may present for sweeping. The
	// capability volume caps already bound a result; this is the backstop that does not depend
	// on them staying correct.
	DefaultMaxInputBytes = 4 << 20
)

// Policy is the effective redaction policy: the built-in defaults, whatever the customer added
// to them, and the bounds the sweep runs under.
//
// It is immutable once built. Nothing on the session stream can reach it, and there is no message
// by which a control plane could send one: the guarantee that a server-side change can never
// widen what leaves a customer's cluster rests on the contract having no such field rather than
// on a check somebody could remove. A gate in internal/gates keeps that true as the protocol
// grows.
type Policy struct {
	rules []Rule
	// alwaysMasked names declared free-text fields the customer excluded categorically, by the
	// same dotted name the report uses.
	alwaysMasked map[string]bool
	budget       time.Duration
	maxInput     int
	// source is what produced this policy, for diagnostics. It never carries the policy's own
	// patterns: the Relay must not print the file that governs disclosure.
	source string
}

// Default is the policy every install has before anyone configures anything. It is safe by
// default rather than safe once configured, because the install that has not been tuned yet is
// the one most likely to be reading a cluster nobody audited.
func Default() *Policy {
	return &Policy{
		rules:        BuiltIn(),
		alwaysMasked: map[string]bool{},
		budget:       DefaultBudget,
		maxInput:     DefaultMaxInputBytes,
		source:       "built-in defaults",
	}
}

// Rules is the effective rule set in the order it is applied. Identifiers only: the patterns
// stay inside this package so that no diagnostic, log line or error can print them.
func (p *Policy) Rules() []string {
	ids := make([]string, 0, len(p.rules))
	for _, rule := range p.rules {
		ids = append(ids, rule.ID)
	}
	return ids
}

// AlwaysMasked is the set of fields the customer excluded categorically, ordered for stable
// diagnostics.
func (p *Policy) AlwaysMasked() []string { return sortedKeys(p.alwaysMasked) }

// Source says where this policy came from, for the Relay's own diagnostics. What is actually
// in force has to be verifiable rather than assumed.
func (p *Policy) Source() string { return p.source }

// Budget and MaxInputBytes report the bounds in force.
func (p *Policy) Budget() time.Duration { return p.budget }
func (p *Policy) MaxInputBytes() int    { return p.maxInput }

// Fault is a policy that cannot be enforced. It is not an ordinary error: while one is held,
// every capability that could emit free text is refused, and the refusal names the fault so an
// operator learns what to fix rather than that something is broken.
//
// It never carries the offending text. A fault about a pattern names the rule, not the pattern,
// and a fault about a field names the field, not its contents.
type Fault struct {
	// Reason is what is wrong, in a sentence an operator can act on.
	Reason string
}

func (f *Fault) Error() string { return "redaction policy fault: " + f.Reason }

func faultf(format string, args ...any) *Fault {
	return &Fault{Reason: fmt.Sprintf(format, args...)}
}
