package session

import (
	"context"
	"sync"
	"testing"
	"time"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// fakeInventory records what the session routed to it and serves scripted pending
// deltas and statuses.
type fakeInventory struct {
	mu       sync.Mutex
	policies []*relayv1.InventorySynchronizationPolicy
	acked    []string
	pending  []*relayv1.InventoryDelta
	statuses []*relayv1.InventoryScopeStatus
}

func (f *fakeInventory) ApplyPolicy(policy *relayv1.InventorySynchronizationPolicy) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.policies = append(f.policies, policy)
}

func (f *fakeInventory) Ack(deltaID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acked = append(f.acked, deltaID)
	remaining := f.pending[:0]
	for _, delta := range f.pending {
		if delta.GetDeltaId() != deltaID {
			remaining = append(remaining, delta)
		}
	}
	f.pending = remaining
}

func (f *fakeInventory) Pending() []*relayv1.InventoryDelta {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*relayv1.InventoryDelta(nil), f.pending...)
}

func (f *fakeInventory) Statuses() []*relayv1.InventoryScopeStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*relayv1.InventoryScopeStatus(nil), f.statuses...)
}

func (f *fakeInventory) receivedPolicies() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.policies)
}

func TestSession_APolicyFromTheStreamReachesTheAttachedInventory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	s := testSession(echoExecutor{})
	inventory := &fakeInventory{}
	s.AttachInventory(inventory)
	go func() { _ = s.runConnection(ctx, cancel, stream, nil) }()

	_ = stream.nextFrom(t) // hello
	stream.accept("session-1")
	stream.incoming <- &relayv1.ControlToRelay{
		Message: &relayv1.ControlToRelay_InventorySynchronizationPolicy{
			InventorySynchronizationPolicy: &relayv1.InventorySynchronizationPolicy{
				ConnectionId: "conn-1", Revision: 1,
			},
		},
	}

	deadline := time.After(2 * time.Second)
	for inventory.receivedPolicies() == 0 {
		select {
		case <-deadline:
			t.Fatal("the policy must reach the attached inventory")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestSession_PendingDeltasAreResentUntilAcked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	s := testSession(echoExecutor{})
	inventory := &fakeInventory{pending: []*relayv1.InventoryDelta{
		{DeltaId: "delta-1", ConnectionId: "conn-1"},
	}}
	s.AttachInventory(inventory)
	go func() { _ = s.runConnection(ctx, cancel, stream, nil) }()

	_ = stream.nextFrom(t) // hello
	stream.accept("session-1")

	first := stream.nextFrom(t).GetInventoryDelta()
	if first == nil || first.GetDeltaId() != "delta-1" {
		t.Fatalf("the pending delta must ride the resend loop, got %+v", first)
	}
	resent := stream.nextFrom(t).GetInventoryDelta()
	if resent == nil || resent.GetDeltaId() != "delta-1" {
		t.Fatal("an unacked delta must be re-emitted")
	}

	stream.incoming <- &relayv1.ControlToRelay{
		Message: &relayv1.ControlToRelay_InventoryDeltaAck{
			InventoryDeltaAck: &relayv1.InventoryDeltaAck{DeltaId: "delta-1"},
		},
	}
	deadline := time.After(2 * time.Second)
	for {
		inventory.mu.Lock()
		acked := len(inventory.acked)
		inventory.mu.Unlock()
		if acked > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the ack must reach the inventory so resending stops")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestSession_HeartbeatsCarryInventoryScopeStatuses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	s := testSession(echoExecutor{})
	inventory := &fakeInventory{statuses: []*relayv1.InventoryScopeStatus{
		{ConnectionId: "conn-1", PolicyRevision: 1},
	}}
	s.AttachInventory(inventory)
	go func() { _ = s.runConnection(ctx, cancel, stream, nil) }()

	_ = stream.nextFrom(t) // hello
	stream.accept("session-1")

	deadline := time.After(2 * time.Second)
	for {
		select {
		case m := <-stream.outgoing:
			heartbeat := m.GetHeartbeat()
			if heartbeat == nil {
				continue
			}
			scopes := heartbeat.GetInventoryScopes()
			if len(scopes) != 1 || scopes[0].GetConnectionId() != "conn-1" {
				t.Fatalf("a heartbeat must carry the scope stamps, got %+v", scopes)
			}
			return
		case <-deadline:
			t.Fatal("expected a heartbeat carrying inventory scope statuses")
		}
	}
}
