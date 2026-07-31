package capabilities

import (
	"context"
	"testing"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

type recordingExecutor struct {
	label string
	calls int
}

func (e *recordingExecutor) Execute(_ context.Context, job *relayv1.JobAssignment) *relayv1.JobResult {
	e.calls++
	return &relayv1.JobResult{JobId: job.GetJobId() + ":" + e.label}
}

func descriptor(id string, version uint32) *relayv1.CapabilityDescriptor {
	return &relayv1.CapabilityDescriptor{CapabilityId: id, CapabilityVersion: version}
}

func assignment(id string, version uint32) *relayv1.JobAssignment {
	return &relayv1.JobAssignment{JobId: "job", CapabilityId: id, CapabilityVersion: version}
}

func TestRegistry_RoutesEachAssignmentToItsOwnCapability(t *testing.T) {
	events := &recordingExecutor{label: "events"}
	logs := &recordingExecutor{label: "logs"}
	registry := NewRegistry()
	registry.Register(descriptor("kubernetes.namespace.events", 1), events)
	registry.Register(descriptor("kubernetes.container.logs", 1), logs)

	if got := registry.Execute(context.Background(),
		assignment("kubernetes.container.logs", 1)); got.GetJobId() != "job:logs" {
		t.Fatalf("the assignment reached the wrong capability: %v", got.GetJobId())
	}
	if events.calls != 0 {
		t.Fatal("a capability must not see an assignment addressed to another")
	}
}

func TestRegistry_AVersionItDoesNotCarryIsRefusedRatherThanRunUnderAnother(t *testing.T) {
	registry := NewRegistry()
	registry.Register(descriptor("kubernetes.container.logs", 1), &recordingExecutor{label: "v1"})

	result := registry.Execute(context.Background(), assignment("kubernetes.container.logs", 2))

	if result.GetFailure().GetKind() != relayv1.JobFailure_KIND_UNSUPPORTED_CAPABILITY_VERSION {
		t.Fatalf("schema versions are frozen, so v2 semantics are not v1's; got %+v", result)
	}
}

func TestRegistry_AnUnknownCapabilityIsRefused(t *testing.T) {
	registry := NewRegistry()
	registry.Register(descriptor("kubernetes.container.logs", 1), &recordingExecutor{})

	result := registry.Execute(context.Background(), assignment("kubernetes.node.shell", 1))

	if result.GetFailure().GetKind() != relayv1.JobFailure_KIND_UNSUPPORTED_CAPABILITY_VERSION {
		t.Fatalf("the compiled set is closed; got %+v", result)
	}
}

func TestRegistry_DescriptorsAreStableSoAReconnectIsNotAChange(t *testing.T) {
	registry := NewRegistry()
	registry.Register(descriptor("kubernetes.workload.runtime", 1), &recordingExecutor{})
	registry.Register(descriptor("kubernetes.container.logs", 1), &recordingExecutor{})
	registry.Register(descriptor("kubernetes.namespace.events", 1), &recordingExecutor{})

	want := []string{
		"kubernetes.container.logs",
		"kubernetes.namespace.events",
		"kubernetes.workload.runtime",
	}
	for attempt := range 5 {
		got := registry.Descriptors()
		if len(got) != len(want) {
			t.Fatalf("attempt %d advertised %d capabilities, want %d", attempt, len(got), len(want))
		}
		for index, id := range want {
			if got[index].GetCapabilityId() != id {
				t.Fatalf("attempt %d position %d was %q, want %q",
					attempt, index, got[index].GetCapabilityId(), id)
			}
		}
	}
}
