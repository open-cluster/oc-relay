package redaction

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Rule is one named secret shape. The identifier travels with every masked span and every
// reported count, so an operator reading a coverage gap can ask the right person to adjust the
// right rule instead of guessing which pattern was too broad.
type Rule struct {
	// ID is stable and public. Built-in identifiers are prefixed "builtin." and customer
	// identifiers may not be, so a policy cannot impersonate a default it is unable to disable.
	ID string
	// pattern is compiled once. Go's regexp is RE2, so matching is linear in the input and no
	// pattern can be made to backtrack catastrophically; the cost bounds in Policy exist for
	// pathological INPUT sizes rather than for pathological patterns.
	pattern *regexp.Regexp
}

// NewRule compiles one rule, refusing an identifier or pattern that would make the policy
// ambiguous. It is the only constructor: a Rule with a nil pattern cannot be built.
func NewRule(id, pattern string) (Rule, error) {
	if strings.TrimSpace(id) == "" {
		return Rule{}, fmt.Errorf("a rule needs an identifier")
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return Rule{}, fmt.Errorf("rule %s: %w", id, err)
	}
	// A pattern that matches the empty string would mark every position in every field, which
	// is a policy that destroys all evidence while reporting that it is working.
	if compiled.MatchString("") {
		return Rule{}, fmt.Errorf("rule %s: the pattern matches the empty string, which would "+
			"mask every field entirely", id)
	}
	return Rule{ID: id, pattern: compiled}, nil
}

// marker is what replaces a masked span. It is fixed rather than length-preserving, and it
// carries the rule identifier so a reader knows why the span is not there.
func marker(ruleID string) string { return "[redacted:" + ruleID + "]" }

// span is one region of a string a rule claimed, half-open as [start, end).
type span struct {
	start, end int
	// rules are every rule that claimed any part of this span after merging, ordered as the
	// policy orders them. The first names the marker; all of them are reported.
	rules []string
}

// mask replaces every span the rules claim, and reports how many spans were masked and which
// rules claimed them.
//
// Matching runs over the ORIGINAL text and the replacements happen afterwards in one pass.
// Applying rules one after another over already-masked text would let one rule match inside
// another's marker, which makes the output depend on rule order in a way nobody declared and
// breaks the determinism a recorded transcript rests on.
func mask(rules []Rule, value string) (masked string, occurrences int, matched []string) {
	if value == "" {
		return value, 0, nil
	}

	var claimed []span
	for _, rule := range rules {
		for _, at := range rule.pattern.FindAllStringIndex(value, -1) {
			claimed = append(claimed, span{start: at[0], end: at[1], rules: []string{rule.ID}})
		}
	}
	if len(claimed) == 0 {
		return value, 0, nil
	}

	merged := merge(claimed)
	rebuilt := &strings.Builder{}
	cursor := 0
	seen := make(map[string]bool, len(rules))
	for _, region := range merged {
		rebuilt.WriteString(value[cursor:region.start])
		rebuilt.WriteString(marker(region.rules[0]))
		cursor = region.end
		for _, id := range region.rules {
			seen[id] = true
		}
	}
	rebuilt.WriteString(value[cursor:])

	// Reported in the policy's own order rather than the order they happened to match, so two
	// results carrying the same secrets report the same identifiers in the same order.
	for _, rule := range rules {
		if seen[rule.ID] {
			matched = append(matched, rule.ID)
		}
	}
	return rebuilt.String(), len(merged), matched
}

// merge collapses overlapping and adjacent claims into single spans. Two rules that both
// matched a credential produce one masked region, not one nested inside another, and the
// occurrence count is a count of secrets rather than of rules that noticed them.
func merge(claimed []span) []span {
	sort.SliceStable(claimed, func(i, j int) bool {
		if claimed[i].start == claimed[j].start {
			return claimed[i].end > claimed[j].end
		}
		return claimed[i].start < claimed[j].start
	})

	merged := []span{claimed[0]}
	for _, next := range claimed[1:] {
		last := &merged[len(merged)-1]
		if next.start > last.end {
			merged = append(merged, next)
			continue
		}
		if next.end > last.end {
			last.end = next.end
		}
		last.rules = append(last.rules, next.rules...)
	}
	return merged
}
