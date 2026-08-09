package inventory

import (
	"testing"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-relay/internal/kube"
)

func intent(name, uid, image string) kube.WorkloadIntent {
	return kube.WorkloadIntent{
		Kind: "deployment", Namespace: "shop", Name: name, UID: uid,
		Generation: 3, Replicas: "2",
		Containers:   []kube.ContainerIntent{{Name: "app", Image: image, LimitsMemory: "256Mi"}},
		TemplateHash: "hash-" + image,
	}
}

func snapshotOf(intents ...kube.WorkloadIntent) Snapshot {
	snapshot := Snapshot{}
	for _, entry := range intents {
		key, state := workloadState(entry)
		snapshot[key] = state
	}
	return snapshot
}

func TestDiff_AnImageChangeNamesTheFieldThatMovedWithBothValues(t *testing.T) {
	before := snapshotOf(intent("api", "uid-1", "registry/app:v1"))
	after := snapshotOf(intent("api", "uid-1", "registry/app:v2"))

	changes := Diff(before, after)
	if len(changes) != 1 {
		t.Fatalf("one object changed, got %d changes", len(changes))
	}
	change := changes[0]
	if change.GetChange() != relayv1.InventoryChangeKind_INVENTORY_CHANGE_KIND_MODIFIED {
		t.Fatalf("an image change is a modification, got %v", change.GetChange())
	}
	var moved *relayv1.InventoryFieldChange
	for _, field := range change.GetFields() {
		if field.GetField() == "spec.template.spec.containers[app].image" {
			moved = field
		}
	}
	if moved == nil {
		t.Fatal("the changed field must be named in the workload's own terms")
	}
	if moved.GetBefore() != "registry/app:v1" || moved.GetAfter() != "registry/app:v2" {
		t.Fatalf("both values must travel, got %q -> %q", moved.GetBefore(), moved.GetAfter())
	}
}

func TestDiff_AResourceLimitChangeCarriesBeforeAndAfter(t *testing.T) {
	halved := intent("api", "uid-1", "registry/app:v1")
	halved.Containers[0].LimitsMemory = "128Mi"

	changes := Diff(
		snapshotOf(intent("api", "uid-1", "registry/app:v1")),
		snapshotOf(halved))
	if len(changes) != 1 {
		t.Fatalf("one object changed, got %d", len(changes))
	}
	for _, field := range changes[0].GetFields() {
		if field.GetField() == "spec.template.spec.containers[app].resources.limits.memory" {
			if field.GetBefore() != "256Mi" || field.GetAfter() != "128Mi" {
				t.Fatalf("the halved limit must be readable as such, got %q -> %q",
					field.GetBefore(), field.GetAfter())
			}
			return
		}
	}
	t.Fatal("the memory limit change was not itemized")
}

func TestDiff_IdenticalSnapshotsProduceNoChanges(t *testing.T) {
	a := snapshotOf(intent("api", "uid-1", "registry/app:v1"))
	b := snapshotOf(intent("api", "uid-1", "registry/app:v1"))
	if changes := Diff(a, b); len(changes) != 0 {
		t.Fatalf("nothing moved, got %d changes", len(changes))
	}
}

func TestDiff_ADeletedAndRecreatedWorkloadIsAReplacementNotAMutation(t *testing.T) {
	before := snapshotOf(intent("api", "uid-old", "registry/app:v1"))
	after := snapshotOf(intent("api", "uid-new", "registry/app:v1"))

	changes := Diff(before, after)
	if len(changes) != 2 {
		t.Fatalf("a replacement is a creation and a deletion, got %d changes", len(changes))
	}
	kinds := map[relayv1.InventoryChangeKind]bool{}
	for _, change := range changes {
		kinds[change.GetChange()] = true
	}
	if !kinds[relayv1.InventoryChangeKind_INVENTORY_CHANGE_KIND_CREATED] ||
		!kinds[relayv1.InventoryChangeKind_INVENTORY_CHANGE_KIND_DELETED] {
		t.Fatalf("expected one created and one deleted, got %v", kinds)
	}
}

func TestDiff_ADeletionCarriesAnEmptyRevision(t *testing.T) {
	changes := Diff(snapshotOf(intent("api", "uid-1", "registry/app:v1")), Snapshot{})
	if len(changes) != 1 || changes[0].GetChange() != relayv1.InventoryChangeKind_INVENTORY_CHANGE_KIND_DELETED {
		t.Fatalf("expected exactly the deletion, got %+v", changes)
	}
	if changes[0].GetObservedRevision() != "" {
		t.Fatal("a deletion has no revision; its emptiness is the dedup component")
	}
}

func TestDiff_TheTemplateHashSpeaksOnlyWhenNoItemizedTemplateFieldMoved(t *testing.T) {
	// An image change moves the hash too; repeating the hash would tell a reader nothing.
	imageMoved := Diff(
		snapshotOf(intent("api", "uid-1", "registry/app:v1")),
		snapshotOf(intent("api", "uid-1", "registry/app:v2")))
	for _, field := range imageMoved[0].GetFields() {
		if field.GetField() == "spec.template" {
			t.Fatal("the template hash must stay silent when an itemized template field moved")
		}
	}

	// A change nowhere in the itemized set — only the hash moved — is exactly worth a line.
	quiet := intent("api", "uid-1", "registry/app:v1")
	quiet.TemplateHash = "hash-other"
	hashOnly := Diff(
		snapshotOf(intent("api", "uid-1", "registry/app:v1")),
		snapshotOf(quiet))
	if len(hashOnly) != 1 {
		t.Fatalf("one object changed, got %d", len(hashOnly))
	}
	fields := hashOnly[0].GetFields()
	if len(fields) != 1 || fields[0].GetField() != "spec.template" {
		t.Fatalf("expected only the template hash line, got %+v", fields)
	}
}

func TestBaseline_CarriesEveryWatchedFieldsCurrentValue(t *testing.T) {
	changes := Baseline(snapshotOf(intent("api", "uid-1", "registry/app:v1")))
	if len(changes) != 1 {
		t.Fatalf("one object, got %d baseline entries", len(changes))
	}
	values := map[string]string{}
	for _, field := range changes[0].GetFields() {
		if field.GetBefore() != "" {
			t.Fatalf("a first observation has no before value, got %q", field.GetBefore())
		}
		values[field.GetField()] = field.GetAfter()
	}
	if values["spec.replicas"] != "2" ||
		values["spec.template.spec.containers[app].image"] != "registry/app:v1" {
		t.Fatalf("the starting point must be visible, got %v", values)
	}
}
