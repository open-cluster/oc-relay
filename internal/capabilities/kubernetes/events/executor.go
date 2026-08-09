package events

import (
	"context"
	"errors"
	"sort"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"

	"github.com/open-cluster/oc-relay/internal/capabilities"
	"github.com/open-cluster/oc-relay/internal/kube"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// Event-list bounds: one bounded page. The customer's local cap may lower the effective
// value but never raise it above MaxEvents, and never below MinEvents — a cap of zero is
// an operator who configured nothing, not an operator who asked for silence.
const (
	MinEvents = 10
	MaxEvents = 200
)

// DefaultRetentionHorizon is what this Relay assumes a cluster keeps events for when the
// operator has not said. It matches the Kubernetes default event TTL, and assuming it is
// the conservative direction: it can only cause a window to be reported as reaching past
// retention, which costs a certified absence, never fabricates one.
const DefaultRetentionHorizon = time.Hour

// Options is the executor's customer-authored configuration.
type Options struct {
	// Policy may refuse a namespace outright.
	Policy capabilities.LocalPolicy
	// LocalMaxEvents lowers the event bound. Zero means the operator set none.
	LocalMaxEvents int64
	// RetentionHorizon is how long the operator attests this cluster keeps events. It is
	// an ATTESTATION rather than a permission: the operator is the only party who knows
	// their apiserver's --event-ttl, and the control plane treats a Relay-attested
	// completeness basis under a confidence cap for exactly this reason.
	RetentionHorizon time.Duration
	// Now is the clock the retention judgement is made against. Injected so the judgement
	// is a property of the arguments rather than of when the process happened to run.
	Now func() time.Time
}

// Executor runs the kubernetes.namespace.events v1 capability through the read-only kube
// port. It holds this capability's semantic contract and nothing else: arguments
// re-validated before any cluster contact, a narrowing built only from identifiers, one
// bounded page whose continuation-free flag is the completeness basis, a window applied
// over that page, and a retention judgement stated rather than inferred.
type Executor struct {
	reader  kube.EventReader
	options Options
}

// NewExecutor builds the capability executor, filling in the defaults an operator who
// configured nothing should get.
func NewExecutor(reader kube.EventReader, options Options) *Executor {
	if options.RetentionHorizon <= 0 {
		options.RetentionHorizon = DefaultRetentionHorizon
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Executor{reader: reader, options: options}
}

// Execute runs one events job to a typed result.
func (e *Executor) Execute(ctx context.Context, job *relayv1.JobAssignment) *relayv1.JobResult {
	args := job.GetArguments().GetKubernetesNamespaceEventsV1()
	if !validArgs(args) {
		return capabilities.Failure(job, relayv1.JobFailure_KIND_ARGUMENTS_REJECTED)
	}
	// Policy is enforced before the cluster is touched, because a namespace the operator
	// excluded must not be reachable even by a read that would have failed anyway.
	if !e.options.Policy.AllowsNamespace(args.GetNamespace()) {
		return capabilities.Failure(job, relayv1.JobFailure_KIND_LOCAL_POLICY_REFUSED)
	}

	return capabilities.Run(ctx, job, func(execCtx context.Context) capabilities.Reading {
		result := e.read(execCtx, args)
		return capabilities.Reading{
			Result: &relayv1.CapabilityResult{
				Result: &relayv1.CapabilityResult_KubernetesNamespaceEventsV1{
					KubernetesNamespaceEventsV1: result,
				},
			},
			Reached: result.GetOutcome() != relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_UNREACHABLE,
		}
	})
}

// read performs the single bounded LIST and assembles the result under the closed outcome
// taxonomy.
func (e *Executor) read(
	ctx context.Context, args *relayv1.KubernetesNamespaceEventsArgsV1,
) *relayv1.KubernetesNamespaceEventsResultV1 {
	applied := capabilities.Lower(args.GetMaxEvents(), e.options.LocalMaxEvents, MinEvents, MaxEvents)
	windowStart := args.GetWindowStart().AsTime()

	basis := &relayv1.KubernetesNamespaceEventsResultV1{
		AppliedMaxEvents: uint32(applied),
		// The horizon travels whatever the outcome, so a reader never has to know whether
		// this Relay was configured in order to interpret the flag beside it.
		AppliedRetentionHorizon: durationpb.New(e.options.RetentionHorizon),
		WindowPredatesRetention: windowStart.Before(e.options.Now().Add(-e.options.RetentionHorizon)),
	}

	page, err := e.reader.ListEvents(ctx, args.GetNamespace(), involvedFrom(args.GetInvolvedObject()), applied)
	if err != nil {
		// A refused or unreachable read is never complete. Leaving Complete false here is
		// the single most important line in this function: a failed read that claimed
		// completeness would let an RBAC mistake be minted as a certified absence.
		basis.Outcome = wireOutcome(outcomeOf(err))
		return basis
	}

	admitted := make([]*relayv1.KubernetesEvent, 0, len(page.Events))
	windowEnd := args.GetWindowEnd().AsTime()
	for index := range page.Events {
		source := &page.Events[index]
		first, last := occurrence(source)
		if !overlapsWindow(first, last, windowStart, windowEnd) {
			continue
		}
		admitted = append(admitted, mapEvent(source, first, last))
	}
	// Oldest first, tie-broken by reason so the order is total and a result is reproducible
	// rather than dependent on however etcd returned the page.
	sort.SliceStable(admitted, func(i, j int) bool {
		left, right := admitted[i].GetLastSeenAt().AsTime(), admitted[j].GetLastSeenAt().AsTime()
		if left.Equal(right) {
			return admitted[i].GetReason() < admitted[j].GetReason()
		}
		return left.Before(right)
	})

	basis.Outcome = relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_SUCCESS
	basis.Events = admitted
	basis.ReturnedEventCount = int32(len(admitted))
	// The completeness basis: a page with no continuation token means every event the
	// namespace held was seen, so the window filter applied to it is a statement about the
	// namespace rather than about an arbitrary subset of it.
	basis.Complete = !page.Truncated
	basis.ReadAt = timestamppb.Now()
	return basis
}

// overlapsWindow reports whether an event's occurrence interval meets the half-open
// window. Overlap rather than containment: an event that began before the window and is
// still recurring into it is exactly the one an investigator wants, and one that stopped
// before the window began is exactly the one they do not.
func overlapsWindow(first, last, start, end time.Time) bool {
	return !last.Before(start) && first.Before(end)
}

// occurrence reports when an event was first and last seen, across the two schemas
// Kubernetes has for saying it. The newer EventTime is used when the legacy timestamps are
// absent; an event carrying neither reports the zero time, which no window admits.
func occurrence(source *corev1.Event) (first, last time.Time) {
	first, last = source.FirstTimestamp.Time, source.LastTimestamp.Time
	if last.IsZero() && !source.EventTime.IsZero() {
		last = source.EventTime.Time
	}
	if first.IsZero() {
		first = last
	}
	if last.IsZero() {
		last = first
	}
	return first, last
}

func mapEvent(source *corev1.Event, first, last time.Time) *relayv1.KubernetesEvent {
	event := &relayv1.KubernetesEvent{
		// Not defaulted to Normal: an unrecognised type must survive as what it was rather
		// than be reported as the benign one.
		Type:    capabilities.CapReason(orUnknown(source.Type)),
		Reason:  capabilities.CapReason(source.Reason),
		Message: capabilities.CapMessage(source.Message),
		InvolvedObject: &relayv1.KubernetesInvolvedObject{
			Kind: capabilities.CapIdentifier(source.InvolvedObject.Kind),
			Name: capabilities.CapIdentifier(source.InvolvedObject.Name),
			Uid:  capabilities.CapIdentifier(string(source.InvolvedObject.UID)),
		},
		SourceComponent: capabilities.CapIdentifier(source.Source.Component),
		// One event folded 300 times and 300 events are different facts. Reporting the
		// count is what keeps them different downstream.
		Count: source.Count,
	}
	if !first.IsZero() {
		event.FirstSeenAt = timestamppb.New(first)
	}
	if !last.IsZero() {
		event.LastSeenAt = timestamppb.New(last)
	}
	return event
}

func orUnknown(value string) string {
	if value == "" {
		return "Unknown"
	}
	return value
}

// involvedFrom converts the wire narrowing into the port's. Every field has already passed
// validation, so nothing here can reach the field-selector encoder as an expression.
func involvedFrom(involved *relayv1.KubernetesInvolvedObject) kube.InvolvedObject {
	return kube.InvolvedObject{
		Kind: involved.GetKind(),
		Name: involved.GetName(),
		UID:  involved.GetUid(),
	}
}

// validArgs re-validates on receipt. The control plane validated before dispatch; neither
// side trusts the other, which is why this exists twice.
//
// The window is required at both ends. An absent bound read as the beginning of time would
// turn a bounded read into an unbounded one through an omission rather than a decision.
func validArgs(args *relayv1.KubernetesNamespaceEventsArgsV1) bool {
	if args == nil || !capabilities.ValidNamespace(args.GetNamespace()) {
		return false
	}
	if args.GetWindowStart() == nil || args.GetWindowEnd() == nil {
		return false
	}
	if !args.GetWindowStart().AsTime().Before(args.GetWindowEnd().AsTime()) {
		return false
	}
	return validNarrowing(args.GetInvolvedObject())
}

// validNarrowing checks each field that was set. A field left empty narrows nothing and is
// not validated; a field that was set must be an identifier, because it is about to be
// rendered into a field selector and an identifier is the only thing that cannot carry a
// separator into one.
func validNarrowing(involved *relayv1.KubernetesInvolvedObject) bool {
	if involved == nil {
		return true
	}
	if involved.GetKind() != "" && !capabilities.ValidKind(involved.GetKind()) {
		return false
	}
	if involved.GetName() != "" && !capabilities.ValidObjectName(involved.GetName()) {
		return false
	}
	return involved.GetUid() == "" || capabilities.ValidUID(involved.GetUid())
}

func outcomeOf(err error) kube.ReadOutcome {
	var readErr *kube.ReadError
	if errors.As(err, &readErr) {
		return readErr.Outcome
	}
	return kube.OutcomeUnreachable
}

// wireOutcome maps the port's taxonomy onto this capability's, which is smaller. A
// not-found from the port has no meaning for a namespaced event list — the API answers one
// in a namespace that does not exist with an empty list — so anything that is neither a
// refusal nor a success is reported as unreachable rather than invented into a value this
// read cannot produce.
func wireOutcome(outcome kube.ReadOutcome) relayv1.KubernetesEventsOutcome {
	switch outcome {
	case kube.OutcomeSuccess:
		return relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_SUCCESS
	case kube.OutcomeUnauthorized:
		return relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_UNAUTHORIZED
	default:
		return relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_UNREACHABLE
	}
}
