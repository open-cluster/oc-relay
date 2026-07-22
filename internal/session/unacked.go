package session

import (
	"sync"

	relayv1 "github.com/OCluster/opencluster-relay/gen/go/opencluster/relay/v1"
)

// unacked retains every produced result until a definitive ResultAck arrives. It is the
// Relay-side guarantee that a dropped ack or a crash-and-reconnect never loses a result:
// entries are resent from here until acknowledged, and control-plane recording is
// idempotent so resends are safe. The total is bounded by the session's outstanding-work
// semaphore, which admits a job only when a slot is free — so this buffer never needs its
// own capacity check.
type unacked struct {
	mu      sync.Mutex
	results map[string]*relayv1.JobResult
}

func newUnacked() *unacked {
	return &unacked{results: make(map[string]*relayv1.JobResult)}
}

// add retains a result, keyed by job id; a resend of the same job id replaces its entry
// rather than duplicating it.
func (u *unacked) add(result *relayv1.JobResult) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.results[result.GetJobId()] = result
}

// ack releases a result once its fate is settled and reports whether an entry was
// actually removed; unknown or already-acked ids are no-ops that report false.
func (u *unacked) ack(jobID string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, ok := u.results[jobID]; !ok {
		return false
	}
	delete(u.results, jobID)
	return true
}

// pending snapshots the retained results for resend.
func (u *unacked) pending() []*relayv1.JobResult {
	u.mu.Lock()
	defer u.mu.Unlock()
	pending := make([]*relayv1.JobResult, 0, len(u.results))
	for _, result := range u.results {
		pending = append(pending, result)
	}
	return pending
}

func (u *unacked) len() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.results)
}

// has reports whether a result for the job is already retained — used to answer a
// redelivered assignment without re-executing it.
func (u *unacked) has(jobID string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	_, ok := u.results[jobID]
	return ok
}
