// Package session runs the Relay's side of the control-plane session stream: the
// outbound single-sender discipline, a bounded worker pool executing assignments through
// a pluggable capability Executor, an unacked-result buffer that resends until the
// control plane acknowledges, heartbeats, an in-flight roster reported on every hello,
// graceful drain, and reconnect with capped jittered backoff. Durable job truth lives in
// the control plane; this package never treats a lost stream as a lost or completed job.
package session

import (
	"context"
	"sync"
	"time"

	relayv1 "github.com/OCluster/opencluster-relay/gen/go/opencluster/relay/v1"
)

// Executor runs one capability job to a typed result. Every source-side fault is a typed
// JobResult (never a panic or a free-text error); the real client-go implementation is a
// separate increment. The context is a child of the connection context and is cancelled
// on a control-plane Cancellation for that job.
type Executor interface {
	Execute(ctx context.Context, job *relayv1.JobAssignment) *relayv1.JobResult
}

// Config is the non-secret session configuration. The credential itself is presented in
// call metadata by the connector, never stored on the session.
type Config struct {
	OrgID              string
	RegistrationID     string
	Capabilities       []*relayv1.CapabilityDescriptor
	MaxConcurrentJobs  uint32
	RelayVersion       string
	ClusterFingerprint string
	LocalPolicyHash    string
	HeartbeatInterval  time.Duration
	ResendInterval     time.Duration
	OutboundBuffer     int
	ReconnectBase      time.Duration
	ReconnectMax       time.Duration
}

// Session owns the durable-identity-scoped runtime across reconnects. The unacked buffer,
// in-flight roster, and the outstanding-work semaphore survive a dropped connection so a
// reconnect resumes cleanly.
type Session struct {
	config   Config
	executor Executor
	unacked  *unacked

	// outstanding bounds total in-flight work — jobs executing PLUS results awaiting a
	// ResultAck — to the advertised concurrency. A slot is acquired at admission and
	// released only when the result is acknowledged (or the job is abandoned by teardown),
	// so a control plane that floods assignments or never acknowledges cannot grow the
	// relay's memory without bound.
	outstanding chan struct{}

	mu       sync.Mutex
	inFlight map[string]uint64 // job id → lease epoch, reported on every hello
}

// New builds a session. MaxConcurrentJobs is clamped to a minimum of 1: a relay that
// advertised zero concurrency could accept no work and would deadlock its first
// assignment on an unbuffered slot channel.
func New(config Config, executor Executor) *Session {
	capacity := int(config.MaxConcurrentJobs)
	if capacity < 1 {
		capacity = 1
		config.MaxConcurrentJobs = 1
	}
	return &Session{
		config:      config,
		executor:    executor,
		unacked:     newUnacked(),
		outstanding: make(chan struct{}, capacity),
		inFlight:    make(map[string]uint64),
	}
}

// acquireSlot reserves one unit of outstanding-work capacity without blocking; it reports
// false when the relay is already at capacity (the caller sheds).
func (s *Session) acquireSlot() bool {
	select {
	case s.outstanding <- struct{}{}:
		return true
	default:
		return false
	}
}

// releaseSlot frees one unit of outstanding-work capacity; it is safe to call at most once
// per acquired slot.
func (s *Session) releaseSlot() {
	select {
	case <-s.outstanding:
	default:
	}
}

// heldSlots reports how many outstanding-work slots are currently reserved — the count of
// jobs executing plus results awaiting a ResultAck. Test-facing so abandonment can be
// observed deterministically.
func (s *Session) heldSlots() int {
	return len(s.outstanding)
}

// markInFlight records a job as executing under a lease epoch so the next hello reports
// it — attested input the control plane may use to renew the lease under a valid epoch.
func (s *Session) markInFlight(jobID string, epoch uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight[jobID] = epoch
}

func (s *Session) clearInFlight(jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inFlight, jobID)
}

func (s *Session) inFlightRoster() []*relayv1.InFlightJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	roster := make([]*relayv1.InFlightJob, 0, len(s.inFlight))
	for jobID, epoch := range s.inFlight {
		roster = append(roster, &relayv1.InFlightJob{JobId: jobID, LeaseEpoch: epoch})
	}
	return roster
}
