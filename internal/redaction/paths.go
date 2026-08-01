package redaction

import (
	"sort"

	"google.golang.org/protobuf/reflect/protoreflect"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// The dotted names a customer policy and a coverage gap both use.
//
// One derivation serves the sweep, the policy parser, the dry run and the build gate. A second
// way of spelling a field is a second thing to drift: a policy naming a field the sweep spells
// differently would silently mask nothing while reporting that it was configured.

// FieldPath is one string field reachable from a CapabilityResult, named as the sweep names it.
type FieldPath struct {
	Path           string
	Message        protoreflect.FullName
	Field          protoreflect.Name
	Classification Classification
	// Declared is false for a string field nobody classified. The gate fails on these; the
	// sweep faults on them.
	Declared bool
}

// FieldPaths enumerates every string field a capability result can carry, in a stable order.
func FieldPaths() []FieldPath {
	var found []FieldPath
	walkStrings(
		(&relayv1.CapabilityResult{}).ProtoReflect().Descriptor(), "",
		make(map[protoreflect.FullName]bool),
		func(path FieldPath) { found = append(found, path) },
	)
	sort.Slice(found, func(i, j int) bool { return found[i].Path < found[j].Path })
	return found
}

// declaredPaths is what a customer policy may name. It is every string field rather than only
// the free-text ones: excluding a field categorically is the customer's decision to make about
// their own evidence, and a typo in the name has to be louder than a rule that quietly masks
// nothing.
func declaredPaths() map[string]bool {
	paths := make(map[string]bool)
	for _, field := range FieldPaths() {
		if field.Declared {
			paths[field.Path] = true
		}
	}
	return paths
}

// walkStrings visits every string field reachable from a message, dotting the path through the
// messages it passed. Recursion is cycle-guarded by message identity rather than by depth: the
// contract has no recursive message today, and a depth limit would silently stop covering one
// that appeared.
func walkStrings(
	message protoreflect.MessageDescriptor, prefix string,
	visiting map[protoreflect.FullName]bool, found func(FieldPath),
) {
	if visiting[message.FullName()] {
		return
	}
	visiting[message.FullName()] = true
	defer delete(visiting, message.FullName())

	fields := message.Fields()
	for i := range fields.Len() {
		field := fields.Get(i)
		path := string(field.Name())
		if prefix != "" {
			path = prefix + "." + path
		}

		switch field.Kind() {
		case protoreflect.StringKind:
			classification, declaredHere := classify(message, field)
			found(FieldPath{
				Path:           path,
				Message:        message.FullName(),
				Field:          field.Name(),
				Classification: classification,
				Declared:       declaredHere,
			})
		case protoreflect.MessageKind, protoreflect.GroupKind:
			if field.IsMap() {
				continue
			}
			walkStrings(field.Message(), path, visiting, found)
		}
	}
}
