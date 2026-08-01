package redaction

import (
	"context"
	"log/slog"
	"os"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// Executor runs one capability job to a typed result. Declared here as well as in the session
// and capabilities packages because this one both consumes an Executor and is one: taking the
// interface locally is what lets the enforcement point sit between them without either side
// depending on redaction.
type Executor interface {
	Execute(ctx context.Context, job *relayv1.JobAssignment) *relayv1.JobResult
}

// Guard is the loaded policy, or the fault that stops it being used. It is one type rather than
// a policy and an error because the composition root must not be able to hold a fault and
// forget to act on it — there is no way to obtain an enforcing executor without going through
// the thing that knows about the fault.
type Guard struct {
	policy *Policy
	fault  *Fault
}

// Load reads the customer's policy from the path the operator configured.
//
// An empty path is an install nobody has configured, and it gets the built-in defaults: the
// first install is the one most likely to be reading a cluster nobody audited, so it must be
// safe before anyone tunes it. A path that WAS configured and cannot be read is a fault,
// because an operator who named a file meant it to be enforced and a missing one is a
// deployment error rather than a decision.
func Load(path string) *Guard {
	if path == "" {
		return &Guard{policy: Default()}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return &Guard{fault: faultf("%s cannot be read; a policy that was configured and is "+
			"not there stops evidence leaving rather than falling back to the defaults", path)}
	}
	policy, fault := Parse(raw, path)
	if fault != nil {
		return &Guard{fault: fault}
	}
	return &Guard{policy: policy}
}

// Policy is the effective policy, or nil when this Guard holds a fault.
func (g *Guard) Policy() *Policy { return g.policy }

// Fault is what stops the policy being enforced, or nil when there is none.
func (g *Guard) Fault() *Fault { return g.fault }

// Describe writes the effective policy to the Relay's own diagnostics: which rules are in
// force, which fields are excluded categorically, and where the policy came from. What is
// actually enforced has to be verifiable rather than assumed.
//
// It carries identifiers and counts and never a pattern. The file governs disclosure, so
// printing it would make diagnosing the Relay a disclosure channel of its own.
func (g *Guard) Describe(logger *slog.Logger) {
	if g.fault != nil {
		logger.Error("redaction policy is faulted; capabilities that could emit free text "+
			"are refused", slog.String("reason", g.fault.Reason))
		return
	}
	logger.Info("redaction policy in force",
		slog.String("source", g.policy.Source()),
		slog.Int("rule_count", len(g.policy.Rules())),
		slog.Any("rules", g.policy.Rules()),
		slog.Any("masked_fields", g.policy.AlwaysMasked()),
		slog.String("sweep_budget", g.policy.Budget().String()),
		slog.Int("max_input_bytes", g.policy.MaxInputBytes()),
	)
}

// Enforce puts this Guard between a capability and the wire.
func (g *Guard) Enforce(inner Executor) *Enforcer {
	return &Enforcer{inner: inner, guard: g}
}

// Enforcer is the single point every result passes through on its way to being serialized.
//
// It is a decorator rather than a step each capability calls, which is the structural half of
// the guarantee: a capability added next year emits its result through whatever the composition
// root wrapped it in, and there is no argument it can pass to opt out.
type Enforcer struct {
	inner Executor
	guard *Guard
}

func (e *Enforcer) Execute(
	ctx context.Context, job *relayv1.JobAssignment,
) *relayv1.JobResult {
	// Refused BEFORE the read rather than after it. A faulted policy that let the cluster be
	// read and then declined to send the answer would still have gathered it, and the point of
	// failing closed is that the text is never held in the first place.
	if e.guard.fault != nil &&
		carriesFreeText(job.GetCapabilityId(), job.GetCapabilityVersion()) {
		return refusal(job)
	}

	result := e.inner.Execute(ctx, job)
	if result == nil {
		return nil
	}

	payload := result.GetResult()
	if payload == nil {
		// A typed failure carries a kind and nothing else — this build never populates a
		// failure's detail — so there is no free text on this path to sweep.
		return result
	}

	// A capability with no free text still passes through the sweep under a faulted policy: it
	// masks nothing, and it is what verifies that every string field in the result is one
	// somebody classified. The check is worth more than the branch that would skip it.
	policy := e.guard.policy
	if policy == nil {
		policy = Default()
	}
	if _, fault := policy.Sweep(payload); fault != nil {
		// The whole result is dropped. Sending the part that was already swept would present a
		// partial read as a complete one, and the fields not yet reached would be unswept.
		return refusal(job)
	}
	return result
}

// refusal is the one shape a redaction refusal takes. It reuses the local-policy kind because
// that is what this is: the customer's own configuration declining a read. No free text reaches
// it — the kind is the whole of what leaves the cluster, and the reason lives in the Relay's
// own diagnostics where the operator who wrote the policy can read it.
func refusal(job *relayv1.JobAssignment) *relayv1.JobResult {
	return &relayv1.JobResult{
		JobId:      job.GetJobId(),
		LeaseEpoch: job.GetLeaseEpoch(),
		Outcome: &relayv1.JobResult_Failure{
			Failure: &relayv1.JobFailure{Kind: relayv1.JobFailure_KIND_LOCAL_POLICY_REFUSED},
		},
	}
}
