package events

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/open-cluster/oc-relay/internal/capabilities"
	"github.com/open-cluster/oc-relay/internal/kube"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// These tests pin what the control plane can observe about an events read: which events
// the window admitted, whether the read was complete, whether the window reached past the
// retention the operator attested to, and what the effective bound actually was. Nothing
// here asserts how the executor called the port.

// A fixed clock, so the retention judgement is a property of the arguments rather than of
// when the suite happened to run.
var now = time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

type fakeReader struct {
	page *kube.EventPage
	err  error

	gotNamespace string
	gotInvolved  kube.InvolvedObject
	gotLimit     int64
	calls        int
	block        chan struct{}
}

func (f *fakeReader) ListEvents(
	ctx context.Context, namespace string, involved kube.InvolvedObject, limit int64,
) (*kube.EventPage, error) {
	f.calls++
	f.gotNamespace, f.gotInvolved, f.gotLimit = namespace, involved, limit
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.page, nil
}

func event(name, reason string, first, last time.Time) corev1.Event {
	return corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "shop"},
		Type:           "Warning",
		Reason:         reason,
		Message:        "something the cluster said about " + name,
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: name, UID: types.UID("uid-" + name)},
		Source:         corev1.EventSource{Component: "kubelet"},
		Count:          1,
		FirstTimestamp: metav1.NewTime(first),
		LastTimestamp:  metav1.NewTime(last),
	}
}

func job(namespace string, start, end time.Time, maxEvents uint32) *relayv1.JobAssignment {
	return jobNarrowed(namespace, start, end, maxEvents, nil)
}

func jobNarrowed(
	namespace string, start, end time.Time, maxEvents uint32,
	involved *relayv1.KubernetesInvolvedObject,
) *relayv1.JobAssignment {
	return &relayv1.JobAssignment{
		JobId: "job-1", LeaseEpoch: 1,
		DeadlineBudget: durationpb.New(30 * time.Second),
		Arguments: &relayv1.CapabilityArguments{
			Arguments: &relayv1.CapabilityArguments_KubernetesNamespaceEventsV1{
				KubernetesNamespaceEventsV1: &relayv1.KubernetesNamespaceEventsArgsV1{
					Namespace:      namespace,
					InvolvedObject: involved,
					WindowStart:    timestamppb.New(start),
					WindowEnd:      timestamppb.New(end),
					MaxEvents:      maxEvents,
				},
			},
		},
	}
}

func newExecutor(reader kube.EventReader, options ...func(*Options)) *Executor {
	opts := Options{RetentionHorizon: time.Hour, Now: func() time.Time { return now }}
	for _, apply := range options {
		apply(&opts)
	}
	return NewExecutor(reader, opts)
}

func eventsResult(t *testing.T, r *relayv1.JobResult) *relayv1.KubernetesNamespaceEventsResultV1 {
	t.Helper()
	result := r.GetResult().GetKubernetesNamespaceEventsV1()
	if result == nil {
		t.Fatalf("expected a kubernetes.namespace.events result, got %+v", r)
	}
	return result
}

func TestExecutor_ReturnsEventsInTheWindowOldestFirstAndReportsCompleteness(t *testing.T) {
	reader := &fakeReader{page: &kube.EventPage{Events: []corev1.Event{
		event("later", "BackOff", now.Add(-10*time.Minute), now.Add(-5*time.Minute)),
		event("earlier", "FailedScheduling", now.Add(-20*time.Minute), now.Add(-18*time.Minute)),
	}}}

	result := eventsResult(t, newExecutor(reader).Execute(context.Background(),
		job("shop", now.Add(-30*time.Minute), now, 50)))

	if result.GetOutcome() != relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_SUCCESS {
		t.Fatalf("expected SUCCESS, got %v", result.GetOutcome())
	}
	if result.GetReturnedEventCount() != 2 || !result.GetComplete() {
		t.Fatalf("expected 2 events and a complete read, got count=%d complete=%v",
			result.GetReturnedEventCount(), result.GetComplete())
	}
	if got := result.GetEvents()[0].GetReason(); got != "FailedScheduling" {
		t.Fatalf("events must be ordered oldest first, got %q first", got)
	}
	if result.GetEvents()[0].GetInvolvedObject().GetName() != "earlier" {
		t.Fatalf("the involved object must survive mapping: %+v", result.GetEvents()[0])
	}
	if result.GetReadAt() == nil {
		t.Fatal("read_at must be stamped")
	}
}

func TestExecutor_WindowExcludesEventsWhoseIntervalDoesNotOverlapIt(t *testing.T) {
	reader := &fakeReader{page: &kube.EventPage{Events: []corev1.Event{
		event("inside", "BackOff", now.Add(-10*time.Minute), now.Add(-9*time.Minute)),
		event("before", "Old", now.Add(-50*time.Minute), now.Add(-45*time.Minute)),
		event("straddling", "Ongoing", now.Add(-50*time.Minute), now.Add(-2*time.Minute)),
	}}}

	result := eventsResult(t, newExecutor(reader).Execute(context.Background(),
		job("shop", now.Add(-30*time.Minute), now, 50)))

	if result.GetReturnedEventCount() != 2 {
		t.Fatalf("the window must admit the event inside it and the one still recurring "+
			"into it, and exclude the one that stopped before it; got %d", result.GetReturnedEventCount())
	}
	for _, e := range result.GetEvents() {
		if e.GetReason() == "Old" {
			t.Fatal("an event whose last occurrence precedes the window must be excluded")
		}
	}
}

func TestExecutor_TruncatedPageIsNotComplete(t *testing.T) {
	reader := &fakeReader{page: &kube.EventPage{
		Events:    []corev1.Event{event("one", "BackOff", now.Add(-time.Minute), now)},
		Truncated: true,
	}}

	result := eventsResult(t, newExecutor(reader).Execute(context.Background(),
		job("shop", now.Add(-30*time.Minute), now, 50)))

	if result.GetComplete() {
		t.Fatal("a page with a continuation token must not be complete: the window filter " +
			"over an arbitrary subset proves nothing")
	}
}

func TestExecutor_EmptyWindowIsCompleteSoAbsenceCanBeCertified(t *testing.T) {
	reader := &fakeReader{page: &kube.EventPage{}}

	result := eventsResult(t, newExecutor(reader).Execute(context.Background(),
		job("shop", now.Add(-30*time.Minute), now, 50)))

	if result.GetOutcome() != relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_SUCCESS {
		t.Fatalf("an empty namespace is a successful read, got %v", result.GetOutcome())
	}
	if result.GetReturnedEventCount() != 0 || !result.GetComplete() {
		t.Fatal("an empty complete read is what a certified absence is minted from")
	}
	if result.GetWindowPredatesRetention() {
		t.Fatal("a window inside the retention horizon must not be flagged")
	}
}

func TestExecutor_WindowReachingPastRetentionIsFlaggedAndCitesTheHorizon(t *testing.T) {
	reader := &fakeReader{page: &kube.EventPage{}}

	result := eventsResult(t, newExecutor(reader).Execute(context.Background(),
		job("shop", now.Add(-3*time.Hour), now, 50)))

	if !result.GetWindowPredatesRetention() {
		t.Fatal("a window beginning before the retention horizon must say so: an empty " +
			"result there cannot tell nothing-happened from already-discarded")
	}
	if result.GetAppliedRetentionHorizon().AsDuration() != time.Hour {
		t.Fatalf("the horizon the judgement rests on must be reported, got %v",
			result.GetAppliedRetentionHorizon().AsDuration())
	}
	if !result.GetComplete() {
		t.Fatal("the retention flag is a separate fact from completeness and must not " +
			"overwrite it")
	}
}

func TestExecutor_UnauthorizedIsNeverReportedAsEmpty(t *testing.T) {
	reader := &fakeReader{err: &kube.ReadError{Outcome: kube.OutcomeUnauthorized}}

	result := eventsResult(t, newExecutor(reader).Execute(context.Background(),
		job("shop", now.Add(-30*time.Minute), now, 50)))

	if result.GetOutcome() != relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_UNAUTHORIZED {
		t.Fatalf("expected UNAUTHORIZED, got %v", result.GetOutcome())
	}
	if result.GetComplete() {
		t.Fatal("a read that was refused is never complete; conflating the two is how an " +
			"RBAC mistake becomes a certified negative")
	}
}

func TestExecutor_PortFailuresMapToTheClosedTaxonomy(t *testing.T) {
	for outcome, want := range map[kube.ReadOutcome]relayv1.KubernetesEventsOutcome{
		kube.OutcomeUnauthorized: relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_UNAUTHORIZED,
		kube.OutcomeUnreachable:  relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_UNREACHABLE,
		// A not-found from anywhere in the port is still not a fact about the namespace.
		kube.OutcomeWorkloadNotFound: relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_UNREACHABLE,
	} {
		reader := &fakeReader{err: &kube.ReadError{Outcome: outcome}}
		result := eventsResult(t, newExecutor(reader).Execute(context.Background(),
			job("shop", now.Add(-30*time.Minute), now, 50)))
		if result.GetOutcome() != want {
			t.Fatalf("port outcome %v must map to %v, got %v", outcome, want, result.GetOutcome())
		}
	}
}

func TestExecutor_LocalCapLowersTheBoundAndIsReported(t *testing.T) {
	reader := &fakeReader{page: &kube.EventPage{}}
	exec := newExecutor(reader, func(o *Options) { o.LocalMaxEvents = 25 })

	result := eventsResult(t, exec.Execute(context.Background(),
		job("shop", now.Add(-30*time.Minute), now, 500)))

	if result.GetAppliedMaxEvents() != 25 {
		t.Fatalf("applied_max_events must cite the effective local bound, got %d",
			result.GetAppliedMaxEvents())
	}
	if reader.gotLimit != 25 {
		t.Fatalf("the list must be issued at the effective bound, got %d", reader.gotLimit)
	}
}

func TestExecutor_LocalCapCannotBeRaisedByADispatchedBound(t *testing.T) {
	reader := &fakeReader{page: &kube.EventPage{}}
	exec := newExecutor(reader, func(o *Options) { o.LocalMaxEvents = 25 })

	// A dispatched bound above the schema ceiling must not become a way past the local cap.
	result := eventsResult(t, exec.Execute(context.Background(),
		job("shop", now.Add(-30*time.Minute), now, 1_000_000)))

	if result.GetAppliedMaxEvents() != 25 {
		t.Fatalf("no dispatched value may raise a local cap, got %d", result.GetAppliedMaxEvents())
	}
}

func TestExecutor_NarrowingIsPassedToThePortAsIdentifiers(t *testing.T) {
	reader := &fakeReader{page: &kube.EventPage{}}

	newExecutor(reader).Execute(context.Background(), jobNarrowed(
		"shop", now.Add(-30*time.Minute), now, 50,
		&relayv1.KubernetesInvolvedObject{Kind: "Pod", Name: "checkout-7f", Uid: "abc-123"}))

	want := kube.InvolvedObject{Kind: "Pod", Name: "checkout-7f", UID: "abc-123"}
	if reader.gotInvolved != want {
		t.Fatalf("the narrowing must reach the port intact, got %+v", reader.gotInvolved)
	}
}

func TestExecutor_RejectsArgumentsTheControlPlaneShouldNeverHaveSent(t *testing.T) {
	for name, assignment := range map[string]*relayv1.JobAssignment{
		"uppercase namespace":          job("Shop", now.Add(-time.Hour), now, 50),
		"empty namespace":              job("", now.Add(-time.Hour), now, 50),
		"window ends before it starts": job("shop", now, now.Add(-time.Hour), 50),
		"window is a point":            job("shop", now, now, 50),
	} {
		reader := &fakeReader{page: &kube.EventPage{}}
		result := newExecutor(reader).Execute(context.Background(), assignment)
		if result.GetFailure().GetKind() != relayv1.JobFailure_KIND_ARGUMENTS_REJECTED {
			t.Fatalf("%s must be rejected, got %+v", name, result)
		}
		if reader.calls != 0 {
			t.Fatalf("%s must be rejected before the cluster is touched", name)
		}
	}
}

func TestExecutor_MissingWindowIsRejectedRatherThanTreatedAsUnbounded(t *testing.T) {
	reader := &fakeReader{page: &kube.EventPage{}}
	assignment := job("shop", now.Add(-time.Hour), now, 50)
	assignment.GetArguments().GetKubernetesNamespaceEventsV1().WindowStart = nil

	result := newExecutor(reader).Execute(context.Background(), assignment)

	if result.GetFailure().GetKind() != relayv1.JobFailure_KIND_ARGUMENTS_REJECTED {
		t.Fatalf("an absent window bound must be rejected, not read as the beginning of "+
			"time; got %+v", result)
	}
}

func TestExecutor_MalformedNarrowingIsRejected(t *testing.T) {
	for name, involved := range map[string]*relayv1.KubernetesInvolvedObject{
		"kind carrying a selector separator": {Kind: "Pod,involvedObject.name=x"},
		"name that is not an identifier":     {Name: "Checkout Pod"},
		"uid carrying an equals sign":        {Uid: "abc=123"},
	} {
		reader := &fakeReader{page: &kube.EventPage{}}
		result := newExecutor(reader).Execute(context.Background(), jobNarrowed(
			"shop", now.Add(-time.Hour), now, 50, involved))
		if result.GetFailure().GetKind() != relayv1.JobFailure_KIND_ARGUMENTS_REJECTED {
			t.Fatalf("%s must be rejected, got %+v", name, result)
		}
		if reader.calls != 0 {
			t.Fatalf("%s must never reach the field-selector encoder", name)
		}
	}
}

func TestExecutor_NamespaceOutsideTheLocalAllowlistIsRefusedByPolicy(t *testing.T) {
	reader := &fakeReader{page: &kube.EventPage{}}
	exec := newExecutor(reader, func(o *Options) {
		o.Policy = capabilities.LocalPolicy{AllowedNamespaces: map[string]bool{"shop": true}}
	})

	result := exec.Execute(context.Background(), job("payments", now.Add(-time.Hour), now, 50))

	if result.GetFailure().GetKind() != relayv1.JobFailure_KIND_LOCAL_POLICY_REFUSED {
		t.Fatalf("a namespace outside the allowlist must be refused by local policy, got %+v", result)
	}
	if reader.calls != 0 {
		t.Fatal("policy is enforced before the cluster is touched")
	}
}

func TestExecutor_FreeTextIsCappedAtTheContractCeilings(t *testing.T) {
	long := make([]byte, capabilities.MaxMessageChars*2)
	for i := range long {
		long[i] = 'a'
	}
	hostile := event("one", "BackOff", now.Add(-time.Minute), now)
	hostile.Message = string(long)
	hostile.Reason = string(long)
	reader := &fakeReader{page: &kube.EventPage{Events: []corev1.Event{hostile}}}

	result := eventsResult(t, newExecutor(reader).Execute(context.Background(),
		job("shop", now.Add(-30*time.Minute), now, 50)))

	got := result.GetEvents()[0]
	if len(got.GetMessage()) != capabilities.MaxMessageChars {
		t.Fatalf("an event message must be capped, got %d chars", len(got.GetMessage()))
	}
	if len(got.GetReason()) != capabilities.MaxReasonChars {
		t.Fatalf("a reason must be capped, got %d chars", len(got.GetReason()))
	}
}

func TestExecutor_BudgetExpiryIsTimeoutAndCancellationIsCancellation(t *testing.T) {
	blocked := &fakeReader{block: make(chan struct{})}
	assignment := job("shop", now.Add(-time.Hour), now, 50)
	assignment.DeadlineBudget = durationpb.New(50 * time.Millisecond)

	if got := newExecutor(blocked).Execute(context.Background(), assignment); got.GetFailure().GetKind() !=
		relayv1.JobFailure_KIND_TIMEOUT {
		t.Fatalf("a budget expiry must be KIND_TIMEOUT, got %+v", got)
	}

	stalled := &fakeReader{block: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *relayv1.JobResult, 1)
	go func() {
		done <- newExecutor(stalled).Execute(ctx, job("shop", now.Add(-time.Hour), now, 50))
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()

	if got := <-done; got.GetFailure().GetKind() != relayv1.JobFailure_KIND_CANCELLED {
		t.Fatalf("a caller cancellation must be KIND_CANCELLED, got %+v", got)
	}
}

func TestDescriptor_NamesTheCompiledIdentityAndVersion(t *testing.T) {
	descriptor := Descriptor()
	if descriptor.GetCapabilityId() != "kubernetes.namespace.events" ||
		descriptor.GetCapabilityVersion() != 1 {
		t.Fatalf("the advertised descriptor is the contract: %+v", descriptor)
	}
}
