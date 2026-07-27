package session

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

func isShed(m *relayv1.RelayToControl) bool {
	pe := m.GetProtocolError()
	return pe != nil && pe.GetCode() == relayv1.ProtocolError_CODE_INTAKE_SHED
}

// A hostile or buggy control plane that floods assignments and never acks must not grow
// the unacked buffer without bound: the relay sheds once the buffer that durably retains
// results is full, and the buffer stays within its capacity.
func TestConnection_ShedsAssignmentsWhenUnackedIsFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	s := testSession(echoExecutor{}) // MaxConcurrentJobs = 4 → unacked capacity 4
	go func() { _ = s.runConnection(ctx, cancel, stream, nil) }()

	_ = stream.nextFrom(t) // hello
	stream.accept("session-1")
	for i := 0; i < 40; i++ {
		stream.assign(fmt.Sprintf("job-%d", i), 1)
	}

	// The buffer never exceeds its capacity, and shed signals are emitted for the excess.
	sheds := stream.countUntil(isShed, 700*time.Millisecond)
	if sheds == 0 {
		t.Fatal("expected INTAKE_SHED for assignments beyond buffer capacity")
	}
	if got := s.unacked.len(); got > 4 {
		t.Fatalf("unacked buffer must stay within capacity 4, got %d", got)
	}
}

// The relay verifies org id AND registration id on every assignment against its
// bootstrap-bound identity; a mismatch is refused with KIND_IDENTITY_MISMATCH and never
// executed — a compromised control plane cannot make this relay run another org's job.
func TestConnection_RefusesAssignmentWithIdentityMismatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	exec := &countingExecutor{}
	s := testSession(exec)
	go func() { _ = s.runConnection(ctx, cancel, stream, nil) }()

	_ = stream.nextFrom(t) // hello
	stream.accept("session-1")
	stream.assignWith("job-x", 1, "org-INTRUDER", "7")

	result := stream.nextFrom(t).GetJobResult()
	if result == nil || result.GetFailure().GetKind() != relayv1.JobFailure_KIND_IDENTITY_MISMATCH {
		t.Fatalf("mismatch must report KIND_IDENTITY_MISMATCH, got %+v", result)
	}
	if exec.calls.Load() != 0 {
		t.Fatal("a mismatched assignment must never reach the executor")
	}
}

// A redelivered assignment (sweep / on-connect catch-up) is re-acked but never executed a
// second time.
func TestConnection_DuplicateAssignmentIsNotReexecuted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	exec := &blockingCountingExecutor{entered: make(chan struct{}, 4)}
	s := testSession(exec)
	go func() { _ = s.runConnection(ctx, cancel, stream, nil) }()

	_ = stream.nextFrom(t) // hello
	stream.accept("session-1")
	stream.assign("job-1", 1)
	<-exec.entered // job-1 is executing

	stream.assign("job-1", 1) // duplicate redelivery
	// Give the loop time to process the duplicate.
	time.Sleep(150 * time.Millisecond)
	if exec.calls.Load() != 1 {
		t.Fatalf("duplicate assignment must not re-execute, executor calls = %d", exec.calls.Load())
	}
}

// A connection teardown (not an explicit Cancellation) must NOT manufacture a
// KIND_CANCELLED result: the in-flight job stays leased and the control plane recovers it
// on lease expiry. Delivering a spurious cancellation would race that recovery and could
// record a still-running job as failed.
func TestConnection_TeardownDoesNotDeliverCancelledResults(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exec := blockingExecutor{entered: make(chan struct{}, 1)}
	s := testSession(exec)

	connCtx, connCancel := context.WithCancel(ctx)
	stream := newFakeStream(connCtx)
	go func() { _ = s.runConnection(connCtx, connCancel, stream, nil) }()

	_ = stream.nextFrom(t) // hello
	stream.accept("session-1")
	stream.assign("job-1", 1)
	_ = stream.nextFrom(t) // ack
	_ = stream.nextFrom(t) // started
	<-exec.entered

	// Tear the connection down while the job is in flight.
	connCancel()

	// Wait for the worker to actually finish (its slot is released only after Execute
	// returns and the job is abandoned), THEN assert nothing was retained — so the test
	// cannot pass merely because unacked began empty.
	waitFor(t, func() bool { return s.heldSlots() == 0 })
	if s.unacked.len() != 0 {
		t.Fatalf("an abandoned job must not be retained for resend, unacked = %d", s.unacked.len())
	}
}

// A DrainInstruction stops new intake, reports DrainState, and ends the connection when
// in-flight work drains or the deadline elapses — abandoned work stays leased and recovers.
func TestConnection_DrainStopsIntakeReportsAndEndsGracefully(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	exec := blockingExecutor{entered: make(chan struct{}, 1)}
	s := testSession(exec)

	done := make(chan error, 1)
	go func() { done <- s.runConnection(ctx, cancel, stream, nil) }()

	_ = stream.nextFrom(t) // hello
	stream.accept("session-1")
	stream.assign("job-1", 1) // in flight, blocks
	_ = stream.nextFrom(t)    // ack
	_ = stream.nextFrom(t)    // started
	<-exec.entered

	// Drain with a short deadline; then a new assignment must be shed and DrainState sent.
	stream.incoming <- &relayv1.ControlToRelay{
		Message: &relayv1.ControlToRelay_DrainInstruction{
			DrainInstruction: &relayv1.DrainInstruction{
				Deadline: durationpb.New(300 * time.Millisecond),
			},
		},
	}
	stream.assign("job-2", 1) // arrives during drain → must be shed

	sawDrainState := false
	sawShed := false
	deadline := time.After(2 * time.Second)
	for !sawDrainState || !sawShed {
		select {
		case m := <-stream.outgoing:
			if m.GetDrainState().GetDraining() {
				sawDrainState = true
			}
			if isShed(m) {
				sawShed = true
			}
		case <-deadline:
			t.Fatalf("drain must emit DrainState (%v) and shed new intake (%v)", sawDrainState, sawShed)
		}
	}

	// The connection ends gracefully at the deadline; the abandoned in-flight job is not
	// retained for resend (recovery is the control plane's).
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("drain must end the connection gracefully, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not end the connection")
	}
	waitFor(t, func() bool { return s.heldSlots() == 0 })
	if s.unacked.len() != 0 {
		t.Fatalf("an abandoned in-flight job must not be retained, unacked = %d", s.unacked.len())
	}
}

type countingExecutor struct{ calls atomic.Int32 }

func (e *countingExecutor) Execute(_ context.Context, job *relayv1.JobAssignment) *relayv1.JobResult {
	e.calls.Add(1)
	return &relayv1.JobResult{JobId: job.GetJobId(), LeaseEpoch: job.GetLeaseEpoch(),
		Outcome: &relayv1.JobResult_Result{Result: &relayv1.CapabilityResult{}}}
}

type blockingCountingExecutor struct {
	calls   atomic.Int32
	entered chan struct{}
}

func (e *blockingCountingExecutor) Execute(ctx context.Context, job *relayv1.JobAssignment) *relayv1.JobResult {
	e.calls.Add(1)
	e.entered <- struct{}{}
	<-ctx.Done()
	return &relayv1.JobResult{JobId: job.GetJobId(), LeaseEpoch: job.GetLeaseEpoch(),
		Outcome: &relayv1.JobResult_Failure{Failure: &relayv1.JobFailure{Kind: relayv1.JobFailure_KIND_CANCELLED}}}
}
