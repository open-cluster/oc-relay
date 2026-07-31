// Package capabilities holds what every compiled capability obeys regardless of which
// bounded read it performs: the execution-budget discipline, the typed failure shapes, the
// per-field caps on strings admitted from a source, and the customer-authored local policy
// that may narrow a read and never widen one.
//
// It exists because these rules are subtle and identical everywhere. The distinction
// between a budget that expired and a caller that cancelled is one line of reasoning that
// three executors would otherwise each hold a copy of, and a copy is where a distinction
// goes to drift.
package capabilities

import (
	"context"
	"errors"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// Reading is what one bounded read produced before it becomes a job result. Reached
// reports whether the read actually got an answer from the cluster, which is what
// separates a budget that ran out mid-read from a budget that ran out after one.
type Reading struct {
	Result *relayv1.CapabilityResult
	// Reached is true when the source answered — including when it answered "not found" or
	// "unauthorized", both of which are answers. It is false when nothing came back.
	Reached bool
}

// Run applies the assignment's deadline budget to one read and turns the outcome into a
// job result.
//
// The budget is a fresh timeout derived from the job context, so it is measured from
// receipt on the Relay's own clock rather than compared against a timestamp minted
// elsewhere. Totality at the boundary: a cancellation of the PARENT context is a
// cancellation — the control plane asked, or the process is going down — while an expiry of
// the derived budget over a read that never reached its source is a timeout. The two are
// never confused, because they call for opposite actions from whoever reads the record.
func Run(
	ctx context.Context,
	job *relayv1.JobAssignment,
	read func(context.Context) Reading,
) *relayv1.JobResult {
	execCtx := ctx
	var cancel context.CancelFunc
	if budget := job.GetDeadlineBudget().AsDuration(); budget > 0 {
		execCtx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}

	reading := read(execCtx)

	if ctx.Err() != nil {
		return Failure(job, relayv1.JobFailure_KIND_CANCELLED)
	}
	if errors.Is(execCtx.Err(), context.DeadlineExceeded) && !reading.Reached {
		return Failure(job, relayv1.JobFailure_KIND_TIMEOUT)
	}

	return &relayv1.JobResult{
		JobId:      job.GetJobId(),
		LeaseEpoch: job.GetLeaseEpoch(),
		Outcome:    &relayv1.JobResult_Result{Result: reading.Result},
	}
}

// Failure is the one shape a typed execution failure takes. No free text from a source
// fault ever reaches it: the kind is the whole of what leaves the cluster.
func Failure(job *relayv1.JobAssignment, kind relayv1.JobFailure_Kind) *relayv1.JobResult {
	return &relayv1.JobResult{
		JobId:      job.GetJobId(),
		LeaseEpoch: job.GetLeaseEpoch(),
		Outcome:    &relayv1.JobResult_Failure{Failure: &relayv1.JobFailure{Kind: kind}},
	}
}
