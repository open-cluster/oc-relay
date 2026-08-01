package redaction

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// maskedFieldRule is the identifier reported when a whole field was excluded categorically
// rather than matched by a pattern. It reads as a rule because that is what a reader needs it
// to be: something with a name they can ask about.
const maskedFieldRule = "policy.masked_field"

// policyVersion is the only format version this build understands. A policy is a security
// control, so a version it does not recognise is refused rather than interpreted optimistically
// — the semantics of an unknown version are exactly what nobody here can know.
const policyVersion = 1

// document is the file's shape, nested under a `redaction:` root. Decoding is STRICT: a key
// this build does not know fails the parse, which is what makes "a policy may only add"
// enforceable rather than aspirational. An operator who writes `disabled_rules:` expecting it to
// weaken the defaults learns at startup that there is no such thing, instead of believing a
// built-in rule is off when it is not.
type document struct {
	Redaction section `yaml:"redaction"`
}

type section struct {
	Version      int             `yaml:"version"`
	Patterns     []patternEntry  `yaml:"patterns"`
	MaskedFields []string        `yaml:"masked_fields"`
	Limits       *limitsDocument `yaml:"limits"`
}

type patternEntry struct {
	ID      string `yaml:"id"`
	Pattern string `yaml:"pattern"`
}

type limitsDocument struct {
	SweepBudget   string `yaml:"sweep_budget"`
	MaxInputBytes int    `yaml:"max_input_bytes"`
}

// Parse reads a customer-authored policy and returns the effective policy: the built-in
// defaults with whatever it added.
//
// Every refusal below is a Fault rather than a fallback. A customer who wrote a policy has
// signalled that the defaults are insufficient for them, so quietly running the defaults after
// a typo would send out exactly what they wrote the file to keep in.
func Parse(raw []byte, source string) (*Policy, *Fault) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)

	var parsed document
	if err := decoder.Decode(&parsed); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, faultf("%s is empty; a policy file that exists must say something, "+
				"because an empty one is indistinguishable from a truncated write", source)
		}
		// The parser's own message names the line and column and never quotes a pattern's value,
		// which is what keeps the raw policy out of the Relay's logs.
		return nil, faultf("%s does not parse: %s", source, firstLine(err.Error()))
	}
	// A second document in the file would be silently ignored, and the one that was ignored is
	// exactly the one an operator would swear they had configured.
	if err := decoder.Decode(new(document)); !errors.Is(err, io.EOF) {
		return nil, faultf("%s carries more than one document; a policy is one document", source)
	}

	if parsed.Redaction.Version != policyVersion {
		return nil, faultf("%s declares redaction format version %d; this build understands "+
			"version %d", source, parsed.Redaction.Version, policyVersion)
	}

	policy := Default()
	policy.source = source

	if fault := policy.addPatterns(parsed.Redaction.Patterns, source); fault != nil {
		return nil, fault
	}
	if fault := policy.addMaskedFields(parsed.Redaction.MaskedFields, source); fault != nil {
		return nil, fault
	}
	if fault := policy.lowerLimits(parsed.Redaction.Limits, source); fault != nil {
		return nil, fault
	}
	return policy, nil
}

// addPatterns appends the customer's own secret shapes. They are appended rather than merged,
// so the built-in set is always evaluated and a customer rule can only ever mask MORE.
func (p *Policy) addPatterns(entries []patternEntry, source string) *Fault {
	seen := make(map[string]bool, len(p.rules)+len(entries))
	for _, rule := range p.rules {
		seen[rule.ID] = true
	}

	for _, entry := range entries {
		id := strings.TrimSpace(entry.ID)
		switch {
		case id == "":
			return faultf("%s: every pattern needs an id, so a coverage gap can name what "+
				"masked the field", source)
		case strings.HasPrefix(id, builtinPrefix):
			// The only way to "disable" a built-in would be to redefine it. Refusing the prefix
			// outright is what makes the add-only rule structural rather than a matter of ordering.
			return faultf("%s: %q uses the reserved %q prefix; a policy may add rules and can "+
				"never replace or disable a built-in one", source, id, builtinPrefix)
		case seen[id]:
			return faultf("%s: %q is declared twice", source, id)
		}

		rule, err := NewRule(id, entry.Pattern)
		if err != nil {
			// The pattern itself is never echoed: the file that governs disclosure must not reach
			// the Relay's logs through its own error path.
			return faultf("%s: the pattern for %q does not compile", source, id)
		}
		seen[id] = true
		p.rules = append(p.rules, rule)
	}
	return nil
}

// addMaskedFields excludes named fields categorically. An unknown name is refused rather than
// ignored: a field name nobody recognises is a policy that masks nothing while its author
// believes it masks everything.
func (p *Policy) addMaskedFields(fields []string, source string) *Fault {
	known := declaredPaths()
	for _, field := range fields {
		name := strings.TrimSpace(field)
		if !known[name] {
			return faultf("%s: %q is not a field any capability result carries; the fields that "+
				"can be named are %s", source, name, strings.Join(sortedKeys(known), ", "))
		}
		p.alwaysMasked[name] = true
	}
	return nil
}

// lowerLimits applies the customer's bounds. They may only be lowered: raising one is the one
// direction that increases what leaves the cluster, which is the direction local configuration
// exists to close.
func (p *Policy) lowerLimits(limits *limitsDocument, source string) *Fault {
	if limits == nil {
		return nil
	}

	if limits.SweepBudget != "" {
		budget, err := time.ParseDuration(limits.SweepBudget)
		if err != nil || budget <= 0 {
			return faultf("%s: sweep_budget must be a positive duration", source)
		}
		if budget > DefaultBudget {
			return faultf("%s: sweep_budget %s is above the %s this build enforces; local "+
				"configuration may lower a bound and never raise one", source, budget, DefaultBudget)
		}
		p.budget = budget
	}

	if limits.MaxInputBytes != 0 {
		if limits.MaxInputBytes < 0 {
			return faultf("%s: max_input_bytes must be positive", source)
		}
		if limits.MaxInputBytes > DefaultMaxInputBytes {
			return faultf("%s: max_input_bytes %d is above the %d this build enforces; local "+
				"configuration may lower a bound and never raise one",
				source, limits.MaxInputBytes, DefaultMaxInputBytes)
		}
		p.maxInput = limits.MaxInputBytes
	}
	return nil
}

// firstLine keeps a parser's message to its first line. A YAML error can carry a fragment of the
// document, and the document is the policy.
func firstLine(message string) string {
	if index := strings.IndexByte(message, '\n'); index >= 0 {
		return strings.TrimSpace(message[:index])
	}
	return strings.TrimSpace(message)
}
