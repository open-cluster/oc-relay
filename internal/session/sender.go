package session

import (
	"context"

	relayv1 "github.com/OCluster/opencluster-relay/gen/go/opencluster/relay/v1"
)

// sendStream is the outbound half of the Connect stream. grpc's generated
// BidiStreamingClient satisfies it; tests substitute a fake.
type sendStream interface {
	Send(*relayv1.RelayToControl) error
}

// sender is the SINGLE writer to the stream. Every outbound message — job acks, results,
// heartbeats, drain reports — is enqueued onto one bounded channel that exactly one
// goroutine drains and writes, honoring gRPC's one-concurrent-sender-per-direction
// contract. The bound applies backpressure: a producer blocks (observing its context)
// rather than growing memory when the peer is slow.
type sender struct {
	stream sendStream
	out    chan *relayv1.RelayToControl
}

func newSender(stream sendStream, capacity int) *sender {
	return &sender{
		stream: stream,
		out:    make(chan *relayv1.RelayToControl, capacity),
	}
}

// enqueue offers a message to the single writer, blocking on backpressure until either
// the buffer accepts it or the context is cancelled.
func (s *sender) enqueue(ctx context.Context, message *relayv1.RelayToControl) error {
	select {
	case s.out <- message:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run drains the outbound channel on a single goroutine until the context is cancelled or
// a Send fails. A send error is returned so the connection can be torn down and retried;
// it is never swallowed.
func (s *sender) run(ctx context.Context) error {
	for {
		select {
		case message := <-s.out:
			if err := s.stream.Send(message); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
