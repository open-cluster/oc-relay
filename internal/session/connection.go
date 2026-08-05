package session

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// bidiStream is the Connect stream. grpc's generated BidiStreamingClient satisfies it;
// tests substitute a programmable fake.
type bidiStream interface {
	Send(*relayv1.RelayToControl) error
	Recv() (*relayv1.ControlToRelay, error)
}

// runConnection drives one connection generation: send hello, receive SessionAccepted,
// then run the sender, receiver, heartbeat, and resend loops under one errgroup so any
// failure tears the whole connection down together. Returns nil on a clean context
// cancel (graceful drain); a non-nil error signals the caller to reconnect. Durable job
// truth is never touched here — a torn-down connection leaves in-flight jobs leased in
// the control plane, recovered by lease expiry.
// ctx is the connection's own context, created and cancelled by the caller (Run), from
// which the stream was opened — so cancelling it (endConnection) unblocks the stream's
// Recv. runConnection bridges group teardown to that cancel so any internal failure or a
// completed drain tears the whole connection — including the blocked receiver — down
// together.
func (s *Session) runConnection(
	ctx context.Context, endConnection context.CancelFunc, stream bidiStream, onEstablished func(),
) error {
	sender := newSender(stream, s.config.OutboundBuffer)

	if err := stream.Send(s.helloMessage()); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}
	accepted, err := s.awaitAccepted(ctx, stream)
	if err != nil {
		return err
	}
	_ = accepted // session id is available for diagnostics; the fence uses it server-side.

	// The session is genuinely established (hello accepted), not merely transport-connected;
	// only now is it safe to reset reconnect backoff, so a control plane that accepts TCP
	// but rejects sessions still sees exponential relief.
	if onEstablished != nil {
		onEstablished()
	}

	// Job contexts are children of the connection context and are tracked so a
	// Cancellation can cancel exactly one job.
	jobs := newJobRegistry()
	var draining atomic.Bool
	drainDeadline := make(chan time.Duration, 1)

	group, groupCtx := errgroup.WithContext(ctx)
	// When any member errors, the errgroup cancels groupCtx; bridge that to endConnection
	// so the stream's Recv (tied to the connection context) unblocks and the receive loop
	// returns instead of hanging group.Wait.
	stopBridge := context.AfterFunc(groupCtx, func() { endConnection() })
	defer stopBridge()

	group.Go(func() error { return sender.run(groupCtx) })
	group.Go(func() error { return s.heartbeatLoop(groupCtx, sender) })
	group.Go(func() error { return s.resendLoop(groupCtx, sender) })
	group.Go(func() error { return s.drainWatcher(groupCtx, endConnection, sender, &draining, drainDeadline) })
	group.Go(func() error {
		return s.receiveLoop(groupCtx, stream, sender, jobs, &draining, drainDeadline)
	})

	waitErr := group.Wait()
	jobs.cancelAll()
	if ctx.Err() != nil || draining.Load() {
		return nil // graceful: process shutdown, or a completed drain.
	}
	return waitErr
}

func (s *Session) helloMessage() *relayv1.RelayToControl {
	return &relayv1.RelayToControl{
		Message: &relayv1.RelayToControl_Hello{
			Hello: &relayv1.Hello{
				ProtocolVersion:    1,
				RelayVersion:       s.config.RelayVersion,
				Capabilities:       s.config.Capabilities,
				ClusterFingerprint: s.config.ClusterFingerprint,
				LocalPolicyHash:    s.config.LocalPolicyHash,
				MaxConcurrentJobs:  s.config.MaxConcurrentJobs,
				InFlight:           s.inFlightRoster(),
			},
		},
	}
}

func (s *Session) awaitAccepted(
	ctx context.Context, stream bidiStream,
) (*relayv1.SessionAccepted, error) {
	message, err := stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("recv session_accepted: %w", err)
	}
	accepted := message.GetSessionAccepted()
	if accepted == nil {
		return nil, fmt.Errorf("expected session_accepted, got %T", message.GetMessage())
	}
	_ = ctx
	return accepted, nil
}

// receiveLoop is the single reader. It dispatches control-plane messages: assignments to
// the bounded worker pool, result acks to the unacked buffer, cancellations to the owning
// job, and drain to graceful shutdown. It never writes the stream — every response is
// enqueued through the single sender.
func (s *Session) receiveLoop(
	ctx context.Context, stream bidiStream, sender *sender, jobs *jobRegistry,
	draining *atomic.Bool, drainDeadline chan time.Duration,
) error {
	for {
		message, err := stream.Recv()
		if err != nil {
			return err
		}

		switch m := message.GetMessage().(type) {
		case *relayv1.ControlToRelay_JobAssignment:
			if draining.Load() {
				// Draining: accept no new work; the job stays leased and recovers.
				_ = sender.enqueue(ctx, intakeShed())
				continue
			}
			s.dispatchAssignment(ctx, m.JobAssignment, sender, jobs)
		case *relayv1.ControlToRelay_ResultAck:
			// The result's fate is settled; free its outstanding-work slot.
			if s.unacked.ack(m.ResultAck.GetJobId()) {
				s.releaseSlot()
			}
		case *relayv1.ControlToRelay_Cancellation:
			s.handleCancellation(ctx, m.Cancellation.GetJobId(), m.Cancellation.GetLeaseEpoch(), sender, jobs)
		case *relayv1.ControlToRelay_DrainInstruction:
			// Graceful drain: flip to draining IMMEDIATELY so any assignment already queued
			// behind this message is shed, then hand the deadline to the watcher, which
			// lets in-flight work flush and ends the connection when drained or on the
			// deadline. Not a hard stop here — the receive loop keeps processing acks.
			draining.Store(true)
			select {
			case drainDeadline <- m.DrainInstruction.GetDeadline().AsDuration():
			default:
			}
		case *relayv1.ControlToRelay_InventorySynchronizationPolicy:
			// A policy with no inventory attached is dropped, not errored: the envelope
			// evolves additively and a build that runs no detection owes nothing here.
			if s.inventory != nil {
				s.inventory.ApplyPolicy(m.InventorySynchronizationPolicy)
			}
		case *relayv1.ControlToRelay_InventoryDeltaAck:
			if s.inventory != nil {
				s.inventory.Ack(m.InventoryDeltaAck.GetDeltaId())
			}
		}
	}
}

// handleCancellation answers with a CancelAck and then cancels the job: a result already
// produced wins (ALREADY_COMPLETED); otherwise the job is aborted and its terminal
// KIND_CANCELLED result follows. The ack is enqueued BEFORE jobs.cancel because cancelling
// unblocks the worker to enqueue its result on the same single sender — enqueuing the ack
// first makes it precede the result on the FIFO channel deterministically.
func (s *Session) handleCancellation(
	ctx context.Context, jobID string, epoch uint64, sender *sender, jobs *jobRegistry,
) {
	disposition := relayv1.CancelAck_DISPOSITION_ABORTED
	if s.unacked.has(jobID) {
		disposition = relayv1.CancelAck_DISPOSITION_ALREADY_COMPLETED
	}
	_ = sender.enqueue(ctx, &relayv1.RelayToControl{
		Message: &relayv1.RelayToControl_CancelAck{
			CancelAck: &relayv1.CancelAck{JobId: jobID, LeaseEpoch: epoch, Disposition: disposition},
		},
	})
	jobs.cancel(jobID)
}

// drainWatcher implements the drain contract: on a DrainInstruction it flips the relay to
// draining (new intake sheds), reports DrainState, and ends the connection when in-flight
// work and unacked results have drained OR the deadline elapses — whichever comes first.
func (s *Session) drainWatcher(
	ctx context.Context, endConnection context.CancelFunc, sender *sender,
	draining *atomic.Bool, drainDeadline chan time.Duration,
) error {
	var deadline time.Duration
	select {
	case deadline = <-drainDeadline:
	case <-ctx.Done():
		return ctx.Err()
	}

	draining.Store(true) // already set by the receive loop; idempotent.
	timeout := time.NewTimer(deadline)
	defer timeout.Stop()
	report := time.NewTicker(drainReportInterval(deadline))
	defer report.Stop()

	for {
		_ = sender.enqueue(ctx, drainStateMessage(len(s.outstanding)))
		if len(s.outstanding) == 0 {
			endConnection() // fully drained: end gracefully
			return nil
		}
		select {
		case <-report.C:
		case <-timeout.C:
			endConnection() // deadline: abandon the rest (stays leased, recovers)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func drainReportInterval(deadline time.Duration) time.Duration {
	interval := deadline / 4
	if interval <= 0 || interval > time.Second {
		interval = time.Second
	}
	return interval
}

func drainStateMessage(outstanding int) *relayv1.RelayToControl {
	return &relayv1.RelayToControl{
		Message: &relayv1.RelayToControl_DrainState{
			DrainState: &relayv1.DrainState{Draining: true, InFlightCount: uint32(outstanding)},
		},
	}
}

// dispatchAssignment admits or sheds one assignment WITHOUT blocking the receive loop, so
// cancellations, acks, and drain stay responsive even at capacity. It dedups redeliveries,
// verifies the assignment's identity against the bootstrap-bound identity, and admits only
// when there is both a free worker slot and room in the unacked buffer to durably retain
// the eventual result. The worker owns ack → started → execute → result so no stream write
// happens on the receive goroutine.
func (s *Session) dispatchAssignment(
	ctx context.Context, job *relayv1.JobAssignment, sender *sender, jobs *jobRegistry,
) {
	jobID := job.GetJobId()
	epoch := job.GetLeaseEpoch()

	// Redelivery (sweep / on-connect catch-up): a job already in flight, or one whose
	// result is still retained, is re-acked but never re-executed. The retained result
	// keeps resending on its own.
	if jobs.has(jobID) || s.unacked.has(jobID) {
		_ = sender.enqueue(ctx, ack(jobID, epoch))
		return
	}

	// Identity cross-check FIRST — before reserving any capacity — so a compromised control
	// plane flooding another org's jobs cannot even consume outstanding-work slots. The
	// relay verifies org id AND registration id against its bootstrap-bound identity; a
	// mismatch is refused with a typed failure and never executed. The refusal is a
	// best-effort report (not retained), so it needs no slot.
	if job.GetOrgId() != s.config.OrgID || job.GetRegistrationId() != s.config.RegistrationID {
		_ = sender.enqueue(ctx, resultMessage(identityMismatch(jobID, epoch)))
		return
	}

	// The assignment must name something this relay compiled in, at the version it compiled.
	// Executing whatever is to hand and reporting the result under the assignment's version
	// would be the worst available answer: schema versions are frozen, so a version this relay
	// does not have means semantics it does not implement, and an answer produced under the
	// wrong contract is indistinguishable downstream from one produced under the right one.
	//
	// Like an identity mismatch this is refused before any capacity is reserved and reported
	// best-effort rather than retained. Nothing was executed, so there is no result to keep.
	//
	// That is a deliberate divergence from what session.proto says about JobResult — that the
	// relay retains a result and resends it until a definitive ResultAck. Retaining this one
	// would mean holding a slot for it, and a control plane dispatching work no relay in the
	// fleet supports would then consume the whole fleet's capacity with refusals it is
	// already ignoring. The cost of not retaining is that a lost refusal waits out the lease
	// instead of ending the job promptly, which is slow rather than wrong.
	if !s.supportsAssignment(job) {
		_ = sender.enqueue(ctx, resultMessage(unsupportedCapability(jobID, epoch)))
		return
	}

	// Admission: reserve one unit of outstanding-work capacity. At capacity the relay
	// sheds (leaving the job leased for the control plane to recover); the non-blocking
	// acquire keeps the receive loop responsive to acks, cancellations, and drain.
	if !s.acquireSlot() {
		_ = sender.enqueue(ctx, intakeShed())
		return
	}

	jobCtx := jobs.add(ctx, jobID)
	s.markInFlight(jobID, epoch)

	go func() {
		defer jobs.remove(jobID)

		_ = sender.enqueue(ctx, ack(jobID, epoch))
		_ = sender.enqueue(ctx, started(jobID, epoch))

		result := s.executor.Execute(jobCtx, job)
		s.clearInFlight(jobID)

		// A job cancelled by connection TEARDOWN (not by an explicit control-plane
		// Cancellation) is abandoned: it stays leased and the control plane recovers it on
		// lease expiry. Manufacturing a KIND_CANCELLED result here would race that recovery
		// and could record a still-running job as failed. The slot is freed since nothing
		// is retained.
		if isCancelled(result) && !jobs.wasExplicit(jobID) {
			s.releaseSlot()
			return
		}
		// The result is retained and resent until acked; its slot is freed on that ack.
		s.deliver(ctx, sender, result)
	}()
}

// deliver retains a result for resend-until-ack and enqueues it for the stream.
func (s *Session) deliver(ctx context.Context, sender *sender, result *relayv1.JobResult) {
	s.unacked.add(result)
	_ = sender.enqueue(ctx, resultMessage(result))
}

// heartbeatLoop proves session liveness on a fixed interval. A heartbeat says nothing
// about job progress — job truth advances only through results. It does carry the
// inventory freshness stamps, because a tick that found nothing changed sends no delta
// and the heartbeat is the only cadence left for confirming the ledger is current.
func (s *Session) heartbeatLoop(ctx context.Context, sender *sender) error {
	ticker := time.NewTicker(s.config.HeartbeatInterval)
	defer ticker.Stop()
	var sequence uint64
	for {
		select {
		case <-ticker.C:
			sequence++
			if err := sender.enqueue(ctx, heartbeat(sequence, s.inventoryStatuses())); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *Session) inventoryStatuses() []*relayv1.InventoryScopeStatus {
	if s.inventory == nil {
		return nil
	}
	return s.inventory.Statuses()
}

// resendLoop re-emits every unacked result on a fixed interval until the control plane
// acknowledges it. Recording is idempotent, so a resend of an already-recorded result is
// answered with ALREADY_RECORDED and dropped. Pending inventory deltas ride the same
// cadence and the same until-acked contract; their recording is deduplicated
// server-side, so a resend collapses rather than duplicating history.
func (s *Session) resendLoop(ctx context.Context, sender *sender) error {
	ticker := time.NewTicker(s.config.ResendInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			for _, result := range s.unacked.pending() {
				if err := sender.enqueue(ctx, &relayv1.RelayToControl{
					Message: &relayv1.RelayToControl_JobResult{JobResult: result},
				}); err != nil {
					return err
				}
			}
			if s.inventory != nil {
				for _, delta := range s.inventory.Pending() {
					if err := sender.enqueue(ctx, &relayv1.RelayToControl{
						Message: &relayv1.RelayToControl_InventoryDelta{InventoryDelta: delta},
					}); err != nil {
						return err
					}
				}
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func ack(jobID string, epoch uint64) *relayv1.RelayToControl {
	return &relayv1.RelayToControl{
		Message: &relayv1.RelayToControl_JobAck{JobAck: &relayv1.JobAck{JobId: jobID, LeaseEpoch: epoch}},
	}
}

func started(jobID string, epoch uint64) *relayv1.RelayToControl {
	return &relayv1.RelayToControl{
		Message: &relayv1.RelayToControl_JobStarted{
			JobStarted: &relayv1.JobStarted{JobId: jobID, LeaseEpoch: epoch},
		},
	}
}

func heartbeat(sequence uint64, scopes []*relayv1.InventoryScopeStatus) *relayv1.RelayToControl {
	return &relayv1.RelayToControl{
		Message: &relayv1.RelayToControl_Heartbeat{
			Heartbeat: &relayv1.Heartbeat{Sequence: sequence, InventoryScopes: scopes},
		},
	}
}

func resultMessage(result *relayv1.JobResult) *relayv1.RelayToControl {
	return &relayv1.RelayToControl{
		Message: &relayv1.RelayToControl_JobResult{JobResult: result},
	}
}

// intakeShed signals the control plane to back off; the shed assignment stays leased and
// is recovered on lease expiry. It carries no distinguishing detail.
func intakeShed() *relayv1.RelayToControl {
	return &relayv1.RelayToControl{
		Message: &relayv1.RelayToControl_ProtocolError{
			ProtocolError: &relayv1.ProtocolError{Code: relayv1.ProtocolError_CODE_INTAKE_SHED},
		},
	}
}

// identityMismatch is the typed refusal of an assignment whose org/registration does not
// match the bootstrap-bound identity — audited centrally as a possible compromise.
func identityMismatch(jobID string, epoch uint64) *relayv1.JobResult {
	return &relayv1.JobResult{
		JobId: jobID, LeaseEpoch: epoch,
		Outcome: &relayv1.JobResult_Failure{
			Failure: &relayv1.JobFailure{Kind: relayv1.JobFailure_KIND_IDENTITY_MISMATCH},
		},
	}
}

// supportsAssignment reports whether the assignment names a capability this relay advertised,
// at the version it advertised. The declared set is the same one the hello carries, so what
// the relay refuses is exactly what it never claimed — there is no second list to drift.
func (s *Session) supportsAssignment(job *relayv1.JobAssignment) bool {
	for _, declared := range s.config.Capabilities {
		if declared.GetCapabilityId() == job.GetCapabilityId() &&
			declared.GetCapabilityVersion() == job.GetCapabilityVersion() {
			return true
		}
	}
	return false
}

// unsupportedCapability is the typed refusal of an assignment this relay cannot run. One kind
// covers an unknown capability and a known one at an unknown version, because the operator
// action is the same for both: this relay is not the build that can do this.
func unsupportedCapability(jobID string, epoch uint64) *relayv1.JobResult {
	return &relayv1.JobResult{
		JobId: jobID, LeaseEpoch: epoch,
		Outcome: &relayv1.JobResult_Failure{
			Failure: &relayv1.JobFailure{
				Kind: relayv1.JobFailure_KIND_UNSUPPORTED_CAPABILITY_VERSION,
			},
		},
	}
}

func isCancelled(result *relayv1.JobResult) bool {
	return result.GetFailure().GetKind() == relayv1.JobFailure_KIND_CANCELLED
}

// jobRegistry tracks per-job cancellation contexts for the current connection and which
// jobs were cancelled by an EXPLICIT control-plane Cancellation (as opposed to connection
// teardown), so the worker can decide whether a KIND_CANCELLED result should be delivered
// or the job abandoned for lease-expiry recovery.
type jobRegistry struct {
	mu       sync.Mutex
	cancels  map[string]context.CancelFunc
	explicit map[string]bool
}

func newJobRegistry() *jobRegistry {
	return &jobRegistry{
		cancels:  make(map[string]context.CancelFunc),
		explicit: make(map[string]bool),
	}
}

func (r *jobRegistry) add(parent context.Context, jobID string) context.Context {
	ctx, cancel := context.WithCancel(parent)
	r.mu.Lock()
	r.cancels[jobID] = cancel
	r.mu.Unlock()
	return ctx
}

func (r *jobRegistry) has(jobID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.cancels[jobID]
	return ok
}

func (r *jobRegistry) remove(jobID string) {
	r.mu.Lock()
	if cancel, ok := r.cancels[jobID]; ok {
		cancel()
		delete(r.cancels, jobID)
	}
	delete(r.explicit, jobID)
	r.mu.Unlock()
}

// cancel records the cancellation as EXPLICIT (control-plane directed) and cancels the
// job context, so the resulting KIND_CANCELLED is delivered rather than abandoned.
func (r *jobRegistry) cancel(jobID string) {
	r.mu.Lock()
	if cancel, ok := r.cancels[jobID]; ok {
		r.explicit[jobID] = true
		cancel()
	}
	r.mu.Unlock()
}

func (r *jobRegistry) wasExplicit(jobID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.explicit[jobID]
}

func (r *jobRegistry) cancelAll() {
	r.mu.Lock()
	for _, cancel := range r.cancels {
		cancel()
	}
	r.cancels = make(map[string]context.CancelFunc)
	r.mu.Unlock()
}
