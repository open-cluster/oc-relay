package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	relayv1 "github.com/OCluster/opencluster-relay/gen/go/opencluster/relay/v1"
)

// The sender is the SINGLE writer to the stream: every outbound message is serialized
// through it, so concurrent producers (worker results, heartbeats, acks) never race the
// gRPC stream's one-writer contract.

type recordingStream struct {
	mu   sync.Mutex
	sent []*relayv1.RelayToControl
	err  error
	gate chan struct{} // if non-nil, Send blocks until it receives
}

func (s *recordingStream) Send(m *relayv1.RelayToControl) error {
	if s.gate != nil {
		<-s.gate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, m)
	return nil
}

func (s *recordingStream) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

func heartbeatMsg(seq uint64) *relayv1.RelayToControl {
	return &relayv1.RelayToControl{
		Message: &relayv1.RelayToControl_Heartbeat{Heartbeat: &relayv1.Heartbeat{Sequence: seq}},
	}
}

func TestSender_SerializesConcurrentEnqueuesThroughOneWriter(t *testing.T) {
	stream := &recordingStream{}
	s := newSender(stream, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.run(ctx)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(seq uint64) {
			defer wg.Done()
			_ = s.enqueue(ctx, heartbeatMsg(seq))
		}(uint64(i))
	}
	wg.Wait()

	waitFor(t, func() bool { return stream.count() == 50 })
}

func TestSender_EnqueueRespectsContextCancellationWhenFull(t *testing.T) {
	// A full buffer plus a blocked Send must not wedge a producer forever: enqueue
	// observes the context and returns when it is cancelled.
	stream := &recordingStream{gate: make(chan struct{})}
	s := newSender(stream, 1)
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	go s.run(runCtx)

	// First message is taken by the sender goroutine and blocks on the gate; the second
	// fills the buffer; the third must block until the enqueue context is cancelled.
	_ = s.enqueue(context.Background(), heartbeatMsg(1))
	_ = s.enqueue(context.Background(), heartbeatMsg(2))

	enqueueCtx, enqueueCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer enqueueCancel()
	err := s.enqueue(enqueueCtx, heartbeatMsg(3))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked enqueue must observe context, got %v", err)
	}
}

func TestSender_StopsAndReportsSendError(t *testing.T) {
	stream := &recordingStream{err: errors.New("stream broken")}
	s := newSender(stream, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- s.run(ctx) }()

	_ = s.enqueue(ctx, heartbeatMsg(1))

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("sender must surface the stream send error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sender did not stop on send error")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
