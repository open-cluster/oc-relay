package session

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	relayv1 "github.com/OCluster/opencluster-relay/gen/go/opencluster/relay/v1"
)

// blockingExecutor blocks until its job context is cancelled, then reports a cancelled
// failure — the totality contract keeps cancel distinct from a source fault.
type blockingExecutor struct {
	entered chan struct{}
}

func (e blockingExecutor) Execute(ctx context.Context, job *relayv1.JobAssignment) *relayv1.JobResult {
	if e.entered != nil {
		e.entered <- struct{}{}
	}
	<-ctx.Done()
	return &relayv1.JobResult{
		JobId:      job.GetJobId(),
		LeaseEpoch: job.GetLeaseEpoch(),
		Outcome: &relayv1.JobResult_Failure{
			Failure: &relayv1.JobFailure{Kind: relayv1.JobFailure_KIND_CANCELLED},
		},
	}
}

func TestConnection_CancellationCancelsTheRunningJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	exec := blockingExecutor{entered: make(chan struct{}, 1)}
	s := testSession(exec)
	go func() { _ = s.runConnection(ctx, cancel, stream, nil) }()

	_ = stream.nextFrom(t) // hello
	stream.accept("session-1")
	stream.assign("job-1", 1)
	_ = stream.nextFrom(t) // ack
	_ = stream.nextFrom(t) // started

	// The executor is now blocked in Execute; a Cancellation must cancel its context so
	// it returns the cancelled failure.
	<-exec.entered
	stream.incoming <- &relayv1.ControlToRelay{
		Message: &relayv1.ControlToRelay_Cancellation{
			Cancellation: &relayv1.Cancellation{JobId: "job-1", LeaseEpoch: 1},
		},
	}

	// The relay answers with a CancelAck (ABORTED) and the terminal KIND_CANCELLED result;
	// read past the ack to the result.
	sawCancelAck := false
	deadline := time.After(2 * time.Second)
	for {
		select {
		case m := <-stream.outgoing:
			if m.GetCancelAck().GetDisposition() == relayv1.CancelAck_DISPOSITION_ABORTED {
				sawCancelAck = true
			}
			if r := m.GetJobResult(); r != nil {
				if r.GetFailure().GetKind() != relayv1.JobFailure_KIND_CANCELLED {
					t.Fatalf("cancelled job must report KIND_CANCELLED, got %+v", r)
				}
				if !sawCancelAck {
					t.Fatal("a Cancellation must be answered with a CancelAck")
				}
				return
			}
		case <-deadline:
			t.Fatal("expected a CancelAck and a KIND_CANCELLED result")
		}
	}
}

func TestRun_ReconnectsWithBackoffUntilEstablished(t *testing.T) {
	var attempts atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var live *fakeStream

	connect := func(connectCtx context.Context) (bidiStream, error) {
		if attempts.Add(1) <= 2 {
			return nil, context.DeadlineExceeded // first two attempts fail
		}
		mu.Lock()
		live = newFakeStream(connectCtx)
		stream := live
		mu.Unlock()
		return stream, nil
	}

	s := testSession(echoExecutor{})
	s.config.ReconnectBase = 5 * time.Millisecond
	s.config.ReconnectMax = 20 * time.Millisecond

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, connect) }()

	// The third attempt establishes: a hello arrives on the live stream.
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return live != nil && len(live.outgoing) > 0
	})
	if attempts.Load() < 3 {
		t.Fatalf("expected at least 3 connect attempts, got %d", attempts.Load())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run must return nil on context cancel, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRun_StopsCleanlyOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	connect := func(connectCtx context.Context) (bidiStream, error) {
		return newFakeStream(connectCtx), nil
	}
	s := testSession(echoExecutor{})
	s.config.ReconnectBase = 5 * time.Millisecond
	s.config.ReconnectMax = 20 * time.Millisecond

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, connect) }()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("clean shutdown must return nil, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}
