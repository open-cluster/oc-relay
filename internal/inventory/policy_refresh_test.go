package inventory

import (
	"context"
	"testing"
	"time"

	"github.com/open-cluster/oc-relay/internal/kube"
)

// The control plane re-sends an identical policy to every live session on a cadence, so a
// Connection created mid-session starts being watched without a reconnect. The refresh must
// not become the schedule: reapplying an unchanged policy resets nothing.
func TestSynchronizer_AnUnchangedPolicyDoesNotResetTheSchedule(t *testing.T) {
	reader := &fakeReader{workloads: map[string][]kube.WorkloadIntent{
		"": {intent("api", "uid-1", "registry/app:v1")},
	}}
	s, advance := newTestSynchronizer(reader, DefaultLocal())
	s.ApplyPolicy(policyFor("conn-1", time.Minute))
	s.RunDue(context.Background())
	ticked := len(reader.listedFrom)

	// The refresh cadence is much faster than the interval. If each reapplication reset
	// nextDue, every RunDue below would tick.
	for range 5 {
		advance(2 * time.Second)
		s.ApplyPolicy(policyFor("conn-1", time.Minute))
		s.RunDue(context.Background())
	}
	if len(reader.listedFrom) != ticked {
		t.Fatalf("reapplying an unchanged policy must not tick the scope; %d extra ticks",
			len(reader.listedFrom)-ticked)
	}

	advance(time.Minute)
	s.RunDue(context.Background())
	if len(reader.listedFrom) <= ticked {
		t.Fatal("the configured interval must still tick once it elapses")
	}
}
