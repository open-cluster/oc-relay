package session

import (
	"context"
	"errors"
	"testing"
	"time"

	relayv1 "github.com/OCluster/opencluster-relay/gen/go/opencluster/relay/v1"
)

// fakeStream is a programmable bidi stream: the test feeds control-plane messages on
// incoming (returned by Recv) and reads relay messages the session Sends on outgoing.
type fakeStream struct {
	ctx      context.Context
	incoming chan *relayv1.ControlToRelay
	outgoing chan *relayv1.RelayToControl
	recvErr  chan error
}

// newFakeStream binds to a context, mirroring a real grpc stream whose Recv unblocks
// with an error when its context is cancelled.
func newFakeStream(ctx context.Context) *fakeStream {
	return &fakeStream{
		ctx:      ctx,
		incoming: make(chan *relayv1.ControlToRelay, 32),
		outgoing: make(chan *relayv1.RelayToControl, 32),
		recvErr:  make(chan error, 1),
	}
}

func (f *fakeStream) Send(m *relayv1.RelayToControl) error {
	f.outgoing <- m
	return nil
}

func (f *fakeStream) Recv() (*relayv1.ControlToRelay, error) {
	select {
	case m := <-f.incoming:
		return m, nil
	case err := <-f.recvErr:
		return nil, err
	case <-f.ctx.Done():
		return nil, f.ctx.Err()
	}
}

func (f *fakeStream) accept(sessionID string) {
	f.incoming <- &relayv1.ControlToRelay{
		Message: &relayv1.ControlToRelay_SessionAccepted{
			SessionAccepted: &relayv1.SessionAccepted{SessionId: sessionID, ProtocolVersion: 1},
		},
	}
}

func (f *fakeStream) assign(jobID string, epoch uint64) {
	f.assignWith(jobID, epoch, "org-a", "7")
}

func (f *fakeStream) assignWith(jobID string, epoch uint64, org, registration string) {
	f.incoming <- &relayv1.ControlToRelay{
		Message: &relayv1.ControlToRelay_JobAssignment{
			JobAssignment: &relayv1.JobAssignment{
				JobId: jobID, OrgId: org, RegistrationId: registration, LeaseEpoch: epoch,
				CapabilityId: "kubernetes.workload.runtime", CapabilityVersion: 1,
			},
		},
	}
}

// drain reads outbound messages until cond is satisfied or the deadline passes, counting
// how many matched pred along the way.
func (f *fakeStream) countUntil(pred func(*relayv1.RelayToControl) bool, d time.Duration) int {
	deadline := time.After(d)
	count := 0
	for {
		select {
		case m := <-f.outgoing:
			if pred(m) {
				count++
			}
		case <-deadline:
			return count
		}
	}
}

// nextFrom returns the next non-heartbeat outbound message. Heartbeats are liveness noise
// interleaved on their own interval; the functional assertions skip them.
func (f *fakeStream) nextFrom(t *testing.T) *relayv1.RelayToControl {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case m := <-f.outgoing:
			if m.GetHeartbeat() != nil {
				continue
			}
			return m
		case <-deadline:
			t.Fatal("expected an outbound message")
			return nil
		}
	}
}

// echoExecutor returns a success result echoing the job id and epoch.
type echoExecutor struct{ started chan string }

func (e echoExecutor) Execute(_ context.Context, job *relayv1.JobAssignment) *relayv1.JobResult {
	if e.started != nil {
		e.started <- job.GetJobId()
	}
	return &relayv1.JobResult{
		JobId:      job.GetJobId(),
		LeaseEpoch: job.GetLeaseEpoch(),
		Outcome:    &relayv1.JobResult_Result{Result: &relayv1.CapabilityResult{}},
	}
}

func testSession(exec Executor) *Session {
	return New(Config{
		OrgID:              "org-a",
		RegistrationID:     "7",
		Capabilities:       []*relayv1.CapabilityDescriptor{{CapabilityId: "kubernetes.workload.runtime", CapabilityVersion: 1}},
		MaxConcurrentJobs:  4,
		RelayVersion:       "0.1.0",
		ClusterFingerprint: "ns-uid",
		HeartbeatInterval:  50 * time.Millisecond,
		ResendInterval:     50 * time.Millisecond,
		OutboundBuffer:     16,
	}, exec)
}

func TestConnection_HelloHandshakeThenAssignmentAckStartedResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	s := testSession(echoExecutor{})

	done := make(chan error, 1)
	go func() { done <- s.runConnection(ctx, cancel, stream, nil) }()

	// First outbound is the hello, carrying the roster and capacity.
	hello := stream.nextFrom(t).GetHello()
	if hello == nil || hello.GetMaxConcurrentJobs() != 4 || len(hello.GetCapabilities()) != 1 {
		t.Fatalf("first message must be a well-formed hello, got %+v", hello)
	}

	stream.accept("session-xyz")
	stream.assign("job-1", 1)

	// The relay answers reception, then execution-began, then the result — in that order.
	if ack := stream.nextFrom(t).GetJobAck(); ack == nil || ack.GetJobId() != "job-1" {
		t.Fatal("expected JobAck for job-1")
	}
	if started := stream.nextFrom(t).GetJobStarted(); started == nil || started.GetJobId() != "job-1" {
		t.Fatal("expected JobStarted for job-1")
	}
	result := stream.nextFrom(t).GetJobResult()
	if result == nil || result.GetJobId() != "job-1" || result.GetLeaseEpoch() != 1 {
		t.Fatalf("expected JobResult for job-1 epoch 1, got %+v", result)
	}

	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("graceful cancel should return nil/Canceled, got %v", err)
	}
}

func TestConnection_ResultIsResentUntilAcked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	s := testSession(echoExecutor{})
	go func() { _ = s.runConnection(ctx, cancel, stream, nil) }()

	_ = stream.nextFrom(t) // hello
	stream.accept("session-1")
	stream.assign("job-1", 1)

	_ = stream.nextFrom(t) // ack
	_ = stream.nextFrom(t) // started
	first := stream.nextFrom(t).GetJobResult()
	if first == nil {
		t.Fatal("expected the initial result")
	}

	// No ack yet: the resend loop must re-emit the same result.
	resent := stream.nextFrom(t).GetJobResult()
	if resent == nil || resent.GetJobId() != "job-1" {
		t.Fatalf("expected the result to be resent, got %+v", resent)
	}

	// Ack it: the buffer clears and no further resends occur for this job.
	stream.incoming <- &relayv1.ControlToRelay{
		Message: &relayv1.ControlToRelay_ResultAck{
			ResultAck: &relayv1.ResultAck{
				JobId: "job-1", LeaseEpoch: 1, Disposition: relayv1.ResultAck_DISPOSITION_RECORDED,
			},
		},
	}
	waitFor(t, func() bool { return s.unacked.len() == 0 })
}

func TestConnection_HelloReportsInFlightRosterOnReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	// A job is already in flight from a previous connection generation.
	s := testSession(echoExecutor{})
	s.markInFlight("job-prev", 3)

	go func() { _ = s.runConnection(ctx, cancel, stream, nil) }()

	hello := stream.nextFrom(t).GetHello()
	if len(hello.GetInFlight()) != 1 || hello.GetInFlight()[0].GetJobId() != "job-prev" ||
		hello.GetInFlight()[0].GetLeaseEpoch() != 3 {
		t.Fatalf("hello must carry the in-flight roster, got %+v", hello.GetInFlight())
	}
}
