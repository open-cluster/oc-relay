package redaction

import (
	"google.golang.org/protobuf/reflect/protoreflect"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// What every string field in a capability result IS, declared rather than guessed.
//
// The enforcement point does not infer free text from a type. It reads this table, and a
// string field that is not in it is a Fault here and a build failure in internal/gates. That
// is the difference between a rule and a convention: a capability added next year cannot emit
// unredacted text by forgetting something, because the code that would have emitted it does
// not compile.

// Classification is what one declared string field is.
type Classification int

const (
	// Structural is a status, reason, phase, quantity, name, namespace or identifier. It is the
	// substance of an investigation and carries no secret, so it is never swept.
	//
	// This is a design constraint on what may become a rule, not merely a default. A policy that
	// masked a reason code would destroy the answer while reporting that it protected something.
	Structural Classification = iota + 1
	// FreeText is text some other software wrote and this one only carried. It is where secrets
	// arrive, and it is the only thing redaction reads.
	FreeText
)

// declared maps a contract message to what each of its string fields is. Every string field
// reachable from CapabilityResult appears exactly once; the gate in internal/gates fails the
// build if that stops being true.
var declared = map[protoreflect.FullName]map[protoreflect.Name]Classification{
	"opencluster.relay.v1.RedactedField": {
		"field_name": Structural,
		"rule_ids":   Structural,
	},
	"opencluster.relay.v1.KubernetesWorkloadSummary": {
		"kind":             Structural,
		"name":             Structural,
		"namespace":        Structural,
		"uid":              Structural,
		"container_images": Structural,
		// A rendering of matchLabels and matchExpressions this Relay produced from the cluster's
		// own structured selector. It is not text an application wrote.
		"selector_summary": Structural,
	},
	"opencluster.relay.v1.KubernetesContainerResources": {
		"container":      Structural,
		"cpu_request":    Structural,
		"cpu_limit":      Structural,
		"memory_request": Structural,
		"memory_limit":   Structural,
	},
	"opencluster.relay.v1.KubernetesPodRuntime": {
		"name":               Structural,
		"phase":              Structural,
		"node_name":          Structural,
		"unscheduled_reason": Structural,
	},
	"opencluster.relay.v1.KubernetesContainerRuntime": {
		"name": Structural,
		// An image reference is a registry coordinate. A credentialed registry URL would be a
		// secret, but this field carries the resolved reference the cluster reports, never a pull
		// credential, and masking it would remove the answer to "which build is running".
		"image": Structural,
	},
	"opencluster.relay.v1.KubernetesContainerState": {
		"waiting_reason": Structural,
	},
	"opencluster.relay.v1.KubernetesContainerTermination": {
		"reason": Structural,
	},
	"opencluster.relay.v1.KubernetesInvolvedObject": {
		"kind": Structural,
		"name": Structural,
		"uid":  Structural,
	},
	"opencluster.relay.v1.KubernetesEvent": {
		"type":             Structural,
		"reason":           Structural,
		"source_component": Structural,
		// The cluster's own account of what happened, written by controllers and admission
		// webhooks that routinely quote a workload's own configuration back into it.
		"message": FreeText,
	},
	"opencluster.relay.v1.KubernetesLogLine": {
		// The largest untrusted surface in the product: written by software an attacker may
		// control, and where applications print the secrets they never meant to print.
		"content": FreeText,
	},
}

// classify reports what a string field is, and whether it was declared at all.
func classify(
	message protoreflect.MessageDescriptor, field protoreflect.FieldDescriptor,
) (Classification, bool) {
	fields, known := declared[message.FullName()]
	if !known {
		return 0, false
	}
	classification, present := fields[field.Name()]
	return classification, present
}

// Declared exposes the table to the build gate. It is a copy, so a gate cannot alter what the
// enforcement point reads — a check that can change the thing it checks is not a check.
func Declared() map[protoreflect.FullName]map[protoreflect.Name]Classification {
	copied := make(map[protoreflect.FullName]map[protoreflect.Name]Classification, len(declared))
	for message, fields := range declared {
		byField := make(map[protoreflect.Name]Classification, len(fields))
		for name, classification := range fields {
			byField[name] = classification
		}
		copied[message] = byField
	}
	return copied
}

// capabilityResults names, for every capability this build can execute, the CapabilityResult
// field its result arrives in. The enforcement point needs it before execution: a policy Fault
// must refuse a read that would have produced free text BEFORE it touches the cluster, rather
// than reading first and declining to send.
//
// It is declared rather than derived from the capability identifier by string surgery, and the
// gate fails the build when a CapabilityResult variant is missing from it. A rule that
// rewrites "kubernetes.container.logs" into a field name would keep working right up to the
// first capability that did not follow the pattern, and would then fail open.
var capabilityResults = []struct {
	id      string
	version uint32
	field   protoreflect.Name
}{
	{"kubernetes.workload.runtime", 1, "kubernetes_workload_runtime_v1"},
	{"kubernetes.namespace.events", 1, "kubernetes_namespace_events_v1"},
	{"kubernetes.container.logs", 1, "kubernetes_container_logs_v1"},
}

// CapabilityResultFields is the same table, for the gate.
func CapabilityResultFields() map[protoreflect.Name]bool {
	fields := make(map[protoreflect.Name]bool, len(capabilityResults))
	for _, capability := range capabilityResults {
		fields[capability.field] = true
	}
	return fields
}

// carriesFreeText reports whether a capability's result can contain a declared free-text
// field, and therefore whether a policy Fault must stop it running.
//
// A capability this build does not know is treated as carrying free text. The registry refuses
// it moments later anyway, and the safe answer to "might this emit text nobody classified" is
// always yes.
func carriesFreeText(id string, version uint32) bool {
	result := (&relayv1.CapabilityResult{}).ProtoReflect().Descriptor()
	for _, capability := range capabilityResults {
		if capability.id != id || capability.version != version {
			continue
		}
		field := result.Fields().ByName(capability.field)
		if field == nil || field.Message() == nil {
			return true
		}
		return messageCarriesFreeText(field.Message(), make(map[protoreflect.FullName]bool))
	}
	return true
}

func messageCarriesFreeText(
	message protoreflect.MessageDescriptor, visited map[protoreflect.FullName]bool,
) bool {
	if visited[message.FullName()] {
		return false
	}
	visited[message.FullName()] = true

	fields := message.Fields()
	for i := range fields.Len() {
		field := fields.Get(i)
		switch field.Kind() {
		case protoreflect.StringKind:
			// An undeclared string field is treated as free text for the same reason an unknown
			// capability is: the only safe answer is the one that refuses.
			classification, declaredHere := classify(message, field)
			if !declaredHere || classification == FreeText {
				return true
			}
		case protoreflect.MessageKind, protoreflect.GroupKind:
			if messageCarriesFreeText(field.Message(), visited) {
				return true
			}
		}
	}
	return false
}
