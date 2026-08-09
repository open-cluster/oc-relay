package gates

import (
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/open-cluster/oc-relay/internal/redaction"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// The structural half of the redaction guarantee.
//
// The enforcement point sweeps DECLARED free-text fields rather than guessing from the type,
// which leaves exactly one way for a capability to emit unredacted text: adding a string field
// nobody classified. These gates make that a build failure, in the same shape as the
// banned-import and schema-shape gates above — ordinary tests over the real descriptors, so a
// //nolint comment or a lint-config edit cannot switch them off.
//
// The sweep also faults at runtime on an undeclared field, so the guarantee does not rest on
// this test alone. It rests on this test for the property that matters to a reviewer: that the
// mistake is caught before the code ships, not the first time a customer's cluster is read.

// TestEveryStringFieldInACapabilityResultIsClassified is the gate the specification names. A
// capability message adding a free-text field without declaring it fails the build here.
func TestEveryStringFieldInACapabilityResultIsClassified(t *testing.T) {
	paths := redaction.FieldPaths()
	// A gate that found nothing to check would pass while covering nothing, which is the exact
	// failure mode these gates exist to prevent.
	if len(paths) == 0 {
		t.Fatal("no string fields found in CapabilityResult; the gate would be vacuous")
	}

	for _, field := range paths {
		if field.Declared {
			continue
		}
		t.Errorf("%s (%s.%s) is a string field nobody classified.\n"+
			"Add it to `declared` in internal/redaction/declared.go as Structural (a status, "+
			"reason, name, quantity or identifier) or FreeText (text some other software wrote "+
			"and this one only carries). Until it is classified the enforcement point cannot "+
			"tell whether it may leave the cluster, and refuses the capability that produces it.",
			field.Path, field.Message, field.Field)
	}
}

// The declaration table must not accumulate entries for fields that no longer exist. A stale
// entry is a classification nobody can act on and, worse, reads as coverage.
func TestTheDeclarationTableNamesOnlyFieldsThatExist(t *testing.T) {
	live := map[protoreflect.FullName]map[protoreflect.Name]bool{}
	for _, field := range redaction.FieldPaths() {
		if live[field.Message] == nil {
			live[field.Message] = map[protoreflect.Name]bool{}
		}
		live[field.Message][field.Field] = true
	}

	for message, fields := range redaction.Declared() {
		for name := range fields {
			if !live[message][name] {
				t.Errorf("internal/redaction declares %s.%s, which no capability result carries; "+
					"remove it rather than leaving a classification nobody can reach",
					message, name)
			}
		}
	}
}

// The enforcement point refuses a capability BEFORE it reads, which needs a mapping from the
// capability to the result field it produces. A capability whose result variant is missing from
// that table would be refused under a faulted policy for the right reason and by accident; one
// added to the contract and forgotten there is the case this catches.
func TestEveryCapabilityResultVariantIsMappedToACapability(t *testing.T) {
	mapped := redaction.CapabilityResultFields()
	descriptor := (&relayv1.CapabilityResult{}).ProtoReflect().Descriptor()

	oneof := descriptor.Oneofs().ByName("result")
	if oneof == nil {
		t.Fatal("CapabilityResult has no result oneof; the gate would be vacuous")
	}

	var missing []string
	for i := range oneof.Fields().Len() {
		field := oneof.Fields().Get(i)
		if !mapped[field.Name()] {
			missing = append(missing, string(field.Name()))
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("CapabilityResult carries %s, which internal/redaction does not map to a "+
			"capability identifier. Add it to `capabilityResults` in declared.go so a faulted "+
			"policy refuses that read before it touches the cluster.",
			strings.Join(missing, ", "))
	}
}

// There is no message by which a control plane could send a redaction policy, and this is what
// makes that structural rather than a check somebody could remove.
//
// The policy is the customer's. A server-side change must never be able to increase what leaves
// their cluster, and the strongest form of that guarantee is not a handler that refuses such a
// message — it is that the contract has no field the message could arrive in. A gate over the
// descriptors is the only thing that keeps it true as the protocol grows.
func TestTheContractCarriesNoWayToSendARedactionPolicy(t *testing.T) {
	// Names a control-plane-authored policy would plausibly arrive under. A field matching one of
	// these on a control-to-relay message is the thing that must never exist.
	suspect := []string{"redaction_policy", "redactionpolicy", "masking_policy", "policy"}

	inspected := 0
	protoregistry.GlobalFiles.RangeFiles(func(file protoreflect.FileDescriptor) bool {
		if !strings.HasPrefix(string(file.Package()), "opencluster.relay") {
			return true
		}
		messages := file.Messages()
		for i := range messages.Len() {
			message := messages.Get(i)
			inspected++
			fields := message.Fields()
			for j := range fields.Len() {
				name := strings.ToLower(string(fields.Get(j).Name()))
				for _, banned := range suspect {
					if name == banned {
						t.Errorf("%s.%s exists. A redaction policy travelling from the control "+
							"plane would let a server-side change widen what leaves a customer's "+
							"cluster; the policy is theirs and is read from their own file.",
							message.FullName(), fields.Get(j).Name())
					}
				}
			}
		}
		return true
	})
	if inspected == 0 {
		t.Fatal("no opencluster.relay messages registered; the gate would be vacuous")
	}
}

// A built-in rule that does not compile would be a Relay whose masking silently did less than
// it claims. BuiltIn panics on one, so this asserts the set is buildable at all.
func TestTheBuiltInRuleSetCompiles(t *testing.T) {
	rules := redaction.Default().Rules()
	if len(rules) == 0 {
		t.Fatal("the built-in rule set is empty; the first install would not be safe by default")
	}
	for _, id := range rules {
		if !strings.HasPrefix(id, "builtin.") {
			t.Errorf("default rule %q does not carry the reserved prefix, so a customer policy "+
				"could redefine it", id)
		}
	}
}
