package inventory

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-relay/internal/kube"
)

// fakeReader serves scripted pages keyed by the namespace argument, so a test can
// assert exactly which namespaces a tick reached.
type fakeReader struct {
	workloads  map[string][]kube.WorkloadIntent
	configMaps map[string][]kube.ConfigVersion
	secrets    map[string][]kube.ConfigVersion
	err        error
	listedFrom []string
}

func (f *fakeReader) ListWorkloadIntents(
	_ context.Context, namespace string, _ int64,
) (*kube.InventoryPage, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.listedFrom = append(f.listedFrom, namespace)
	return &kube.InventoryPage{Workloads: f.workloads[namespace]}, nil
}

func (f *fakeReader) ListConfigMapVersions(
	_ context.Context, namespace string, _ int64,
) (*kube.ConfigVersionPage, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &kube.ConfigVersionPage{Versions: f.configMaps[namespace]}, nil
}

func (f *fakeReader) ListSecretVersions(
	_ context.Context, namespace string, _ int64,
) (*kube.ConfigVersionPage, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &kube.ConfigVersionPage{Versions: f.secrets[namespace]}, nil
}

func policyFor(connection string, interval time.Duration, namespaces ...string) *relayv1.InventorySynchronizationPolicy {
	return &relayv1.InventorySynchronizationPolicy{
		ConnectionId:      connection,
		Revision:          1,
		Namespaces:        namespaces,
		RequestedInterval: durationpb.New(interval),
	}
}

func testClock(start time.Time) (func() time.Time, func(time.Duration)) {
	current := start
	return func() time.Time { return current }, func(d time.Duration) { current = current.Add(d) }
}

func newTestSynchronizer(reader Reader, local Local) (*Synchronizer, func(time.Duration)) {
	now, advance := testClock(time.Date(2026, 8, 5, 3, 40, 0, 0, time.UTC))
	return NewSynchronizer(reader, local, nil, now), advance
}

func TestSynchronizer_TheFirstTickIsABaselineMarkedAsSuch(t *testing.T) {
	reader := &fakeReader{workloads: map[string][]kube.WorkloadIntent{
		"": {intent("api", "uid-1", "registry/app:v1")},
	}}
	s, _ := newTestSynchronizer(reader, DefaultLocal())
	s.ApplyPolicy(policyFor("conn-1", time.Minute))

	s.RunDue(context.Background())

	pending := s.Pending()
	if len(pending) != 1 {
		t.Fatalf("the first tick files one delta, got %d", len(pending))
	}
	if !pending[0].GetBaseline() {
		t.Fatal("installing a Relay must arrive as a baseline, not as everything changing at once")
	}
	if len(pending[0].GetChanges()) != 1 {
		t.Fatalf("the baseline carries the observed objects, got %d", len(pending[0].GetChanges()))
	}
}

func TestSynchronizer_ATickWithNoChangeProducesNoMessage(t *testing.T) {
	reader := &fakeReader{workloads: map[string][]kube.WorkloadIntent{
		"": {intent("api", "uid-1", "registry/app:v1")},
	}}
	s, advance := newTestSynchronizer(reader, DefaultLocal())
	s.ApplyPolicy(policyFor("conn-1", time.Minute))

	s.RunDue(context.Background())
	for _, delta := range s.Pending() {
		s.Ack(delta.GetDeltaId())
	}

	advance(2 * time.Minute)
	s.RunDue(context.Background())

	if pending := s.Pending(); len(pending) != 0 {
		t.Fatalf("nothing changed, so nothing may travel; got %d deltas", len(pending))
	}
}

func TestSynchronizer_AChangeProducesADeltaNamingOnlyWhatMoved(t *testing.T) {
	reader := &fakeReader{workloads: map[string][]kube.WorkloadIntent{
		"": {intent("api", "uid-1", "registry/app:v1"), intent("web", "uid-2", "registry/web:v1")},
	}}
	s, advance := newTestSynchronizer(reader, DefaultLocal())
	s.ApplyPolicy(policyFor("conn-1", time.Minute))
	s.RunDue(context.Background())
	for _, delta := range s.Pending() {
		s.Ack(delta.GetDeltaId())
	}

	reader.workloads[""] = []kube.WorkloadIntent{
		intent("api", "uid-1", "registry/app:v2"), intent("web", "uid-2", "registry/web:v1"),
	}
	advance(2 * time.Minute)
	s.RunDue(context.Background())

	pending := s.Pending()
	if len(pending) != 1 {
		t.Fatalf("one delta expected, got %d", len(pending))
	}
	if pending[0].GetBaseline() {
		t.Fatal("a change after the baseline is a delta, not a second baseline")
	}
	changes := pending[0].GetChanges()
	if len(changes) != 1 || changes[0].GetName() != "api" {
		t.Fatalf("only the workload that moved may appear, got %+v", changes)
	}
}

func TestSynchronizer_ARequestedIntervalBelowTheLocalFloorIsFloored(t *testing.T) {
	local := DefaultLocal()
	local.MinimumInterval = time.Minute
	reader := &fakeReader{}
	s, advance := newTestSynchronizer(reader, local)
	s.ApplyPolicy(policyFor("conn-1", time.Second))

	s.RunDue(context.Background()) // the first tick runs immediately
	first := len(reader.listedFrom)

	advance(30 * time.Second) // above the request, below the floor
	s.RunDue(context.Background())
	if len(reader.listedFrom) != first {
		t.Fatal("a tick before the local floor elapsed means the server set the schedule")
	}

	advance(31 * time.Second)
	s.RunDue(context.Background())
	if len(reader.listedFrom) <= first {
		t.Fatal("once the floor elapsed the scope must tick again")
	}
}

func TestSynchronizer_RequestedNamespacesAreIntersectedWithLocalConfiguration(t *testing.T) {
	local := DefaultLocal()
	local.Namespaces = map[string]bool{"shop": true}
	reader := &fakeReader{}
	s, _ := newTestSynchronizer(reader, local)
	s.ApplyPolicy(policyFor("conn-1", time.Minute, "shop", "payments"))

	s.RunDue(context.Background())

	if len(reader.listedFrom) != 1 || reader.listedFrom[0] != "shop" {
		t.Fatalf("the server asked for two namespaces and local configuration allows one; "+
			"listed %v", reader.listedFrom)
	}
}

func TestSynchronizer_AFaultedTickKeepsThePreviousSnapshotSoTheChangeIsNotLost(t *testing.T) {
	reader := &fakeReader{workloads: map[string][]kube.WorkloadIntent{
		"": {intent("api", "uid-1", "registry/app:v1")},
	}}
	s, advance := newTestSynchronizer(reader, DefaultLocal())
	s.ApplyPolicy(policyFor("conn-1", time.Minute))
	s.RunDue(context.Background())
	for _, delta := range s.Pending() {
		s.Ack(delta.GetDeltaId())
	}

	reader.err = errors.New("apiserver unreachable")
	advance(2 * time.Minute)
	s.RunDue(context.Background())

	statuses := s.Statuses()
	if len(statuses) != 1 || !statuses[0].GetFaulted() {
		t.Fatalf("a failed read must surface as a faulted scope, got %+v", statuses)
	}

	// The change that happened during the fault is detected once reads recover,
	// because the diff still runs against the last good snapshot.
	reader.err = nil
	reader.workloads[""] = []kube.WorkloadIntent{intent("api", "uid-1", "registry/app:v2")}
	advance(2 * time.Minute)
	s.RunDue(context.Background())

	pending := s.Pending()
	if len(pending) != 1 || pending[0].GetBaseline() {
		t.Fatalf("recovery must diff against the pre-fault snapshot, got %+v", pending)
	}
	if statuses := s.Statuses(); statuses[0].GetFaulted() {
		t.Fatal("a recovered scope must stop reporting faulted")
	}
}

func TestSynchronizer_AnUnackedDeltaStaysPendingAndAnAckDropsIt(t *testing.T) {
	reader := &fakeReader{workloads: map[string][]kube.WorkloadIntent{
		"": {intent("api", "uid-1", "registry/app:v1")},
	}}
	s, _ := newTestSynchronizer(reader, DefaultLocal())
	s.ApplyPolicy(policyFor("conn-1", time.Minute))
	s.RunDue(context.Background())

	pending := s.Pending()
	if len(pending) != 1 {
		t.Fatalf("expected the baseline pending, got %d", len(pending))
	}
	if again := s.Pending(); len(again) != 1 {
		t.Fatal("an unacked delta stays pending for the resend loop")
	}
	s.Ack(pending[0].GetDeltaId())
	if after := s.Pending(); len(after) != 0 {
		t.Fatalf("an acked delta must stop resending, got %d", len(after))
	}
}

func TestSynchronizer_PendingOverflowClearsTheQueueAndSchedulesAFreshBaseline(t *testing.T) {
	reader := &fakeReader{workloads: map[string][]kube.WorkloadIntent{
		"": {intent("api", "uid-1", "registry/app:v0")},
	}}
	s, advance := newTestSynchronizer(reader, DefaultLocal())
	s.ApplyPolicy(policyFor("conn-1", time.Minute))
	s.RunDue(context.Background())

	// Nothing is ever acked, and the workload changes on every tick.
	for i := 0; len(s.Pending()) > 0 && i < maxPendingDeltas+8; i++ {
		reader.workloads[""] = []kube.WorkloadIntent{
			intent("api", "uid-1", "registry/app:v"+string(rune('1'+i%8))+"x"+time.Duration(i).String()),
		}
		advance(2 * time.Minute)
		s.RunDue(context.Background())
	}

	if pending := s.Pending(); len(pending) != 0 {
		t.Fatalf("overflow must clear the queue rather than grow without bound, got %d", len(pending))
	}

	advance(2 * time.Minute)
	s.RunDue(context.Background())
	pending := s.Pending()
	if len(pending) != 1 || !pending[0].GetBaseline() {
		t.Fatal("after overflow the next tick must re-observe as a fresh baseline")
	}
}

func TestSynchronizer_OnlyReferencedConfigMapsAndSecretsAreWatched(t *testing.T) {
	referenced := intent("api", "uid-1", "registry/app:v1")
	referenced.ConfigMapRefs = []string{"app-config"}
	referenced.SecretRefs = []string{"db-credentials"}
	reader := &fakeReader{
		workloads: map[string][]kube.WorkloadIntent{"": {referenced}},
		configMaps: map[string][]kube.ConfigVersion{"": {
			{Namespace: "shop", Name: "app-config", UID: "cm-1", ResourceVersion: "100"},
			{Namespace: "shop", Name: "unrelated", UID: "cm-2", ResourceVersion: "200"},
		}},
		secrets: map[string][]kube.ConfigVersion{"": {
			{Namespace: "shop", Name: "db-credentials", UID: "sec-1", ResourceVersion: "300"},
		}},
	}
	s, _ := newTestSynchronizer(reader, DefaultLocal())
	s.ApplyPolicy(policyFor("conn-1", time.Minute))
	s.RunDue(context.Background())

	pending := s.Pending()
	if len(pending) != 1 {
		t.Fatalf("expected one baseline delta, got %d", len(pending))
	}
	names := map[string]bool{}
	for _, change := range pending[0].GetChanges() {
		names[change.GetName()] = true
	}
	if !names["app-config"] || !names["db-credentials"] {
		t.Fatalf("referenced configuration must be watched, got %v", names)
	}
	if names["unrelated"] {
		t.Fatal("an unreferenced ConfigMap is not context for any watched workload")
	}
}

func TestSynchronizer_ASecretChangeIsRecordedByVersionAndItsContentAppearsNowhere(t *testing.T) {
	referenced := intent("api", "uid-1", "registry/app:v1")
	referenced.SecretRefs = []string{"db-credentials"}
	reader := &fakeReader{
		workloads: map[string][]kube.WorkloadIntent{"": {referenced}},
		secrets: map[string][]kube.ConfigVersion{"": {
			{Namespace: "shop", Name: "db-credentials", UID: "sec-1", ResourceVersion: "300"},
		}},
	}
	s, advance := newTestSynchronizer(reader, DefaultLocal())
	s.ApplyPolicy(policyFor("conn-1", time.Minute))
	s.RunDue(context.Background())
	for _, delta := range s.Pending() {
		s.Ack(delta.GetDeltaId())
	}

	reader.secrets[""] = []kube.ConfigVersion{
		{Namespace: "shop", Name: "db-credentials", UID: "sec-1", ResourceVersion: "301"},
	}
	advance(2 * time.Minute)
	s.RunDue(context.Background())

	pending := s.Pending()
	if len(pending) != 1 || len(pending[0].GetChanges()) != 1 {
		t.Fatalf("the rotation must surface as one change, got %+v", pending)
	}
	change := pending[0].GetChanges()[0]
	if change.GetKind() != relayv1.InventoryObjectKind_INVENTORY_OBJECT_KIND_SECRET ||
		change.GetName() != "db-credentials" || change.GetObservedRevision() != "301" {
		t.Fatalf("a rotation is identity and version, got %+v", change)
	}
	for _, field := range change.GetFields() {
		if field.GetField() != "metadata.resourceVersion" {
			t.Fatalf("no field beyond the version may travel for a Secret, got %q", field.GetField())
		}
	}
}

func TestSynchronizer_AChangedNamespaceSetRebaselines(t *testing.T) {
	reader := &fakeReader{workloads: map[string][]kube.WorkloadIntent{
		"shop":     {intent("api", "uid-1", "registry/app:v1")},
		"payments": {intent("ledger", "uid-2", "registry/ledger:v1")},
	}}
	s, advance := newTestSynchronizer(reader, DefaultLocal())
	s.ApplyPolicy(policyFor("conn-1", time.Minute, "shop"))
	s.RunDue(context.Background())
	for _, delta := range s.Pending() {
		s.Ack(delta.GetDeltaId())
	}

	s.ApplyPolicy(policyFor("conn-1", time.Minute, "shop", "payments"))
	advance(2 * time.Minute)
	s.RunDue(context.Background())

	pending := s.Pending()
	if len(pending) != 1 || !pending[0].GetBaseline() {
		t.Fatal("a moved scope boundary must re-baseline; a diff across two boundaries " +
			"would report the boundary as cluster changes")
	}
}
