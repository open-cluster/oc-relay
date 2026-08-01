package redaction

import (
	"sort"
	"time"

	"google.golang.org/protobuf/reflect/protoreflect"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// Sweep masks every declared free-text field in a result and reports what it removed.
//
// It mutates the result in place and returns the report it also attaches, because there must be
// exactly one copy of the text: a function that returned a masked value beside the original
// would leave the caller holding the thing this package exists to stop, and a test asserting on
// the return value would pass against an implementation that sent both.
//
// A nil report means nothing was masked. It never means the sweep did not happen — a result
// that faulted is never serialized at all.
func (p *Policy) Sweep(result *relayv1.CapabilityResult) (*relayv1.RedactionReport, *Fault) {
	if result == nil {
		return nil, nil
	}

	run := &sweep{policy: p, started: time.Now(), counts: map[string]*tally{}}
	if fault := run.message(result.ProtoReflect(), ""); fault != nil {
		return nil, fault
	}

	report := run.report()
	result.Redaction = report
	return report, nil
}

// tally is one declared field's accumulated result across every element of every repeated
// message it sits inside.
type tally struct {
	order       int
	occurrences uint32
	rules       map[string]bool
}

type sweep struct {
	policy  *Policy
	started time.Time
	swept   int
	counts  map[string]*tally
	next    int
}

// message walks one message, masking the string fields declared free text and leaving the rest
// exactly as the source reported them.
//
// Fields are visited in field-number order rather than in whatever order the message happens to
// hold them, so two sweeps of identical input produce identical reports.
func (s *sweep) message(message protoreflect.Message, path string) *Fault {
	descriptor := message.Descriptor()
	fields := descriptor.Fields()

	for i := range fields.Len() {
		field := fields.Get(i)
		if !message.Has(field) {
			continue
		}
		here := string(field.Name())
		if path != "" {
			here = path + "." + here
		}

		switch field.Kind() {
		case protoreflect.StringKind:
			if fault := s.strings(message, descriptor, field, here); fault != nil {
				return fault
			}
		case protoreflect.MessageKind, protoreflect.GroupKind:
			if fault := s.messages(message, field, here); fault != nil {
				return fault
			}
		}
	}
	return nil
}

// messages recurses into a message field, singular or repeated.
func (s *sweep) messages(
	holder protoreflect.Message, field protoreflect.FieldDescriptor, path string,
) *Fault {
	// A map field would be a dynamic payload, which the schema-shape gate already bans; guarding
	// here as well means this walk cannot silently skip one if that gate is ever weakened.
	if field.IsMap() {
		return faultf("%s is a map field, which the contract does not permit", path)
	}
	if field.IsList() {
		list := holder.Get(field).List()
		for i := range list.Len() {
			if fault := s.message(list.Get(i).Message(), path); fault != nil {
				return fault
			}
		}
		return nil
	}
	return s.message(holder.Get(field).Message(), path)
}

// strings masks one string field, singular or repeated.
//
// An undeclared field is a Fault rather than a value passed through. Passing it through is
// silent disclosure, and it is the exact failure the declaration table exists to make
// impossible; the build gate normally catches it first, and this is what happens if it does not.
func (s *sweep) strings(
	holder protoreflect.Message, descriptor protoreflect.MessageDescriptor,
	field protoreflect.FieldDescriptor, path string,
) *Fault {
	classification, known := classify(descriptor, field)
	if !known {
		return faultf("%s.%s is a string field no one classified, so this build cannot tell "+
			"whether it may leave the cluster", descriptor.FullName(), field.Name())
	}
	if classification != FreeText && !s.policy.alwaysMasked[path] {
		return nil
	}

	if field.IsList() {
		list := holder.Mutable(field).List()
		for i := range list.Len() {
			masked, fault := s.value(list.Get(i).String(), path)
			if fault != nil {
				return fault
			}
			list.Set(i, protoreflect.ValueOfString(masked))
		}
		return nil
	}

	masked, fault := s.value(holder.Get(field).String(), path)
	if fault != nil {
		return fault
	}
	holder.Set(field, protoreflect.ValueOfString(masked))
	return nil
}

// value masks one string and records what was removed from it.
func (s *sweep) value(value, path string) (string, *Fault) {
	// Checked before the work rather than after it. A bound enforced once the cost has already
	// been paid is not a bound.
	s.swept += len(value)
	if s.swept > s.policy.maxInput {
		return "", faultf("this result presents more than %d bytes of free text; the sweep is "+
			"refused rather than shortened, because a pattern that is skipped is a secret that "+
			"is sent", s.policy.maxInput)
	}
	if time.Since(s.started) > s.policy.budget {
		return "", faultf("sweeping this result exceeded the %s budget; it is refused rather "+
			"than partially swept", s.policy.budget)
	}

	// A field the customer excluded categorically is replaced whole. There is nothing to count
	// occurrences of: the exclusion is about the field, not about what happened to be in it.
	if s.policy.alwaysMasked[path] {
		if value == "" {
			return value, nil
		}
		s.record(path, 1, []string{maskedFieldRule})
		return marker(maskedFieldRule), nil
	}

	masked, occurrences, matched := mask(s.policy.rules, value)

	// Checked AFTER the work as well as before it. Checking only on the way in cannot catch the
	// case the budget exists for — one enormous value that takes longer than the whole budget on
	// its own — because there is no next value to be refused at. The result is still dropped
	// whole: what has already been swept is not sent, so a slow field costs the read rather than
	// leaking the fields behind it.
	if time.Since(s.started) > s.policy.budget {
		return "", faultf("sweeping this result exceeded the %s budget; it is refused rather "+
			"than partially swept", s.policy.budget)
	}

	if occurrences > 0 {
		s.record(path, uint32(occurrences), matched)
	}
	return masked, nil
}

func (s *sweep) record(path string, occurrences uint32, rules []string) {
	entry, seen := s.counts[path]
	if !seen {
		entry = &tally{order: s.next, rules: map[string]bool{}}
		s.next++
		s.counts[path] = entry
	}
	entry.occurrences += occurrences
	for _, id := range rules {
		entry.rules[id] = true
	}
}

// report renders the tallies in the order the fields were first reached, so the same result
// always produces the same report.
func (s *sweep) report() *relayv1.RedactionReport {
	if len(s.counts) == 0 {
		return nil
	}

	paths := make([]string, 0, len(s.counts))
	for path := range s.counts {
		paths = append(paths, path)
	}
	sort.Slice(paths, func(i, j int) bool {
		return s.counts[paths[i]].order < s.counts[paths[j]].order
	})

	fields := make([]*relayv1.RedactedField, 0, len(paths))
	for _, path := range paths {
		entry := s.counts[path]
		fields = append(fields, &relayv1.RedactedField{
			FieldName:             path,
			MaskedOccurrenceCount: entry.occurrences,
			RuleIds:               sortedKeys(entry.rules),
		})
	}
	return &relayv1.RedactionReport{Fields: fields}
}

// sortedKeys orders a map's keys. Every list this package reports — rule identifiers, masked
// fields, the names a policy may use — goes through it, so two results carrying the same masking
// always describe it in the same order and a recorded transcript stays comparable.
func sortedKeys[T any](keyed map[string]T) []string {
	keys := make([]string, 0, len(keyed))
	for key := range keyed {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
