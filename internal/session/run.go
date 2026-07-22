package session

import (
	"context"
	"math/rand"
	"time"
)

// Connector opens one Connect stream. The real implementation presents the durable
// credential in call metadata and enforces SPKI pinning; tests substitute a fake. It
// returns an error when a connection cannot be established, which drives backoff.
type Connector func(ctx context.Context) (bidiStream, error)

// Run is the reconnect supervisor: it repeatedly connects and runs one connection
// generation, reconnecting with capped jittered backoff after any failure, until the
// process context is cancelled (graceful shutdown → returns nil). A dropped connection is
// never a lost job — in-flight work stays leased in the control plane and the unacked
// buffer resends results on the next connection.
func (s *Session) Run(ctx context.Context, connect Connector) error {
	base := s.config.ReconnectBase
	if base <= 0 {
		base = time.Second
	}
	max := s.config.ReconnectMax
	if max < base {
		max = 30 * time.Second
	}
	//nolint:gosec // jitter spread, not a security decision.
	b := newBackoff(backoffConfig{
		base:   base,
		max:    max,
		jitter: fullJitter(rand.New(rand.NewSource(time.Now().UnixNano()))),
	})

	for {
		if ctx.Err() != nil {
			return nil
		}

		// Own the per-attempt context so the stream is opened from it: cancelling connCtx
		// (on teardown or a completed drain) then unblocks the stream's Recv.
		connCtx, cancel := context.WithCancel(ctx)
		stream, err := connect(connCtx)
		if err != nil {
			cancel()
			if !sleep(ctx, b.next()) {
				return nil
			}
			continue
		}

		// Reset backoff only after the SESSION is accepted (inside runConnection), not on
		// mere transport connect — otherwise a control plane that accepts TCP but rejects
		// hellos would be hammered at the base interval with no exponential relief.
		_ = s.runConnection(connCtx, cancel, stream, b.reset)
		cancel()
		if ctx.Err() != nil {
			return nil
		}

		// A drain or a transport failure both end this generation; reconnect after backoff.
		// (A server-directed GracefulReconnect will carry its own retry_after in a later
		// increment; R1 uses the normal backoff.)
		if !sleep(ctx, b.next()) {
			return nil
		}
	}
}

// sleep waits for d or until the context is cancelled; it returns false if the context
// was cancelled (the caller should stop).
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
