package inventory

import (
	"sort"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// Diff reports what moved between two snapshots of one scope, in deterministic order.
// It compares only what the snapshots hold, which is only declared intent — the
// property that a status change cannot produce a delta is decided upstream by what the
// snapshot type can carry, and this function cannot reintroduce it.
func Diff(previous, current Snapshot) []*relayv1.InventoryObjectChange {
	var changes []*relayv1.InventoryObjectChange

	for _, key := range sortedKeys(current) {
		state := current[key]
		before, existed := previous[key]
		if !existed {
			changes = append(changes, objectChange(key, state.Revision,
				relayv1.InventoryChangeKind_INVENTORY_CHANGE_KIND_CREATED,
				fieldValues(state.Fields)))
			continue
		}
		if before.Revision == state.Revision {
			continue
		}
		changes = append(changes, objectChange(key, state.Revision,
			relayv1.InventoryChangeKind_INVENTORY_CHANGE_KIND_MODIFIED,
			fieldDifferences(before.Fields, state.Fields)))
	}

	for _, key := range sortedKeys(previous) {
		if _, still := current[key]; !still {
			// A deletion's observed revision is empty, and that emptiness is itself the
			// dedup component: a UID is deleted at most once, so no second row can want
			// the same key.
			changes = append(changes, objectChange(key, "",
				relayv1.InventoryChangeKind_INVENTORY_CHANGE_KIND_DELETED, nil))
		}
	}
	return changes
}

// Baseline renders a whole snapshot as first-observation changes, carrying every
// watched field's current value so later diffs have a visible starting point.
func Baseline(snapshot Snapshot) []*relayv1.InventoryObjectChange {
	var changes []*relayv1.InventoryObjectChange
	for _, key := range sortedKeys(snapshot) {
		state := snapshot[key]
		changes = append(changes, objectChange(key, state.Revision,
			relayv1.InventoryChangeKind_INVENTORY_CHANGE_KIND_CREATED,
			fieldValues(state.Fields)))
	}
	return changes
}

func objectChange(
	key ObjectKey, revision string, kind relayv1.InventoryChangeKind,
	fields []*relayv1.InventoryFieldChange,
) *relayv1.InventoryObjectChange {
	return &relayv1.InventoryObjectChange{
		Namespace:        key.Namespace,
		Kind:             key.Kind,
		Name:             key.Name,
		Uid:              key.UID,
		ObservedRevision: revision,
		Change:           kind,
		Fields:           fields,
	}
}

// maxFieldValueBytes is the contract's bound on one field value, applied where the wire
// message is built. The SNAPSHOT keeps the full value — the revision digest must cover
// it, or two states differing only past the cut would read as unchanged — and the clip
// happens on the way out, so no change can be refused at the far end for its size and
// silently lost, because the snapshot advances whether or not a delta lands.
const maxFieldValueBytes = 512

// clip bounds a field value, marking the cut so a truncated reference list cannot be
// mistaken for the whole one.
func clip(value string) string {
	if len(value) <= maxFieldValueBytes {
		return value
	}
	const marker = "...clipped"
	return value[:maxFieldValueBytes-len(marker)] + marker
}

// fieldValues renders a state's fields as changes from nothing, sorted by name.
func fieldValues(fields map[string]string) []*relayv1.InventoryFieldChange {
	names := sortedFieldNames(fields, nil)
	rendered := make([]*relayv1.InventoryFieldChange, 0, len(names))
	for _, name := range names {
		rendered = append(rendered, &relayv1.InventoryFieldChange{Field: name, After: clip(fields[name])})
	}
	return rendered
}

// fieldDifferences itemizes what moved between two field maps. The template-hash field
// is reported only when it is the SOLE template signal: every itemized template field is
// inside the template, so the hash moves with each of them and repeating it would tell a
// reader nothing — but when the hash alone moved, the template changed in a field this
// ledger does not itemize, and that is exactly worth a line.
func fieldDifferences(before, after map[string]string) []*relayv1.InventoryFieldChange {
	var changes []*relayv1.InventoryFieldChange
	templateItemized := false

	for _, name := range sortedFieldNames(before, after) {
		if before[name] == after[name] {
			continue
		}
		if name == "spec.template" {
			continue // decided after the walk, once templateItemized is known
		}
		if isTemplateField(name) {
			templateItemized = true
		}
		changes = append(changes, &relayv1.InventoryFieldChange{
			Field:  name,
			Before: clip(before[name]),
			After:  clip(after[name]),
		})
	}

	if before["spec.template"] != after["spec.template"] && !templateItemized {
		changes = append(changes, &relayv1.InventoryFieldChange{
			Field:  "spec.template",
			Before: before["spec.template"],
			After:  after["spec.template"],
		})
	}
	return changes
}

func isTemplateField(name string) bool {
	return len(name) > len("spec.template.") && name[:len("spec.template.")] == "spec.template."
}

func sortedFieldNames(a, b map[string]string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	names := make([]string, 0, len(a)+len(b))
	for name := range a {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for name := range b {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
