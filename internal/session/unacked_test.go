package session

import (
	"testing"

	relayv1 "github.com/OCluster/opencluster-relay/gen/go/opencluster/relay/v1"
)

func jobResult(jobID string, epoch uint64) *relayv1.JobResult {
	return &relayv1.JobResult{JobId: jobID, LeaseEpoch: epoch}
}

// The unacked buffer holds every produced result until a definitive ResultAck. A relay
// crash-and-reconnect or a dropped ack must never lose a result: it is resent from here
// until the control plane acknowledges it, and recording is idempotent so resends are
// safe.
func TestUnacked_RetainsUntilAckedThenReleases(t *testing.T) {
	u := newUnacked()

	u.add(jobResult("job-1", 1))
	u.add(jobResult("job-2", 1))
	if u.len() != 2 {
		t.Fatalf("expected 2 unacked, got %d", u.len())
	}

	pending := u.pending()
	if len(pending) != 2 {
		t.Fatalf("pending must snapshot all unacked, got %d", len(pending))
	}

	u.ack("job-1")
	if u.len() != 1 {
		t.Fatalf("ack must release the result, got %d", u.len())
	}
	// Acking an unknown or already-acked job is a harmless no-op.
	u.ack("job-1")
	u.ack("job-unknown")
	if u.len() != 1 {
		t.Fatalf("redundant ack must not change the buffer, got %d", u.len())
	}
}

func TestUnacked_ReaddingTheSameJobDoesNotDuplicate(t *testing.T) {
	u := newUnacked()
	u.add(jobResult("job-1", 1))
	// A resend re-adds the same job id; the buffer keeps one entry keyed by job id.
	u.add(jobResult("job-1", 1))
	if u.len() != 1 {
		t.Fatalf("re-adding the same job must not duplicate, got %d", u.len())
	}
}

func TestUnacked_AckReportsWhetherAnEntryWasRemoved(t *testing.T) {
	// The ack's return value drives the outstanding-work slot release: only a real removal
	// frees a slot, so an unknown or redundant ack must report false.
	u := newUnacked()
	u.add(jobResult("job-1", 1))

	if !u.ack("job-1") {
		t.Fatal("acking a retained result must report removed=true")
	}
	if u.ack("job-1") {
		t.Fatal("acking an already-removed result must report removed=false")
	}
	if u.ack("job-unknown") {
		t.Fatal("acking an unknown job must report removed=false")
	}
}
