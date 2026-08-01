package redaction

import (
	"fmt"
	"strings"
)

// A policy whose breadth is only discoverable by losing an investigation is a policy nobody
// will tune. This is how an operator finds out that a rule is too broad before it costs them
// the decisive log line rather than after.

// Preview is what a policy would do to a piece of sample text. It runs entirely locally: no
// control plane, no cluster, no session.
type Preview struct {
	// Masked is the sample as it would leave the cluster.
	Masked string
	// Occurrences is how many spans were masked.
	Occurrences int
	// ByRule counts the spans each rule claimed, so a rule that is claiming far more than the
	// operator expected is visible as a number rather than as a feeling.
	ByRule map[string]int
	// Lines is the per-line account, kept because a rule that fires on one line in a thousand
	// is a different problem from one that fires on all of them.
	Lines []PreviewLine
}

// PreviewLine is one line of the sample and what happened to it.
type PreviewLine struct {
	Number      int
	Masked      string
	Occurrences int
	Rules       []string
}

// DryRun evaluates a policy against operator-supplied sample text.
//
// It reports per line because that is the unit an operator pasted in and the unit a log is read
// in. Categorically masked fields are NOT applied here: this answers "what do my patterns do to
// this text", and a field exclusion is a decision about a field rather than about text.
func (p *Policy) DryRun(sample string) Preview {
	preview := Preview{ByRule: map[string]int{}}

	lines := strings.Split(sample, "\n")
	rendered := make([]string, 0, len(lines))
	for index, line := range lines {
		masked, occurrences, matched := mask(p.rules, line)
		rendered = append(rendered, masked)
		if occurrences == 0 {
			continue
		}
		preview.Occurrences += occurrences
		for _, id := range matched {
			preview.ByRule[id]++
		}
		preview.Lines = append(preview.Lines, PreviewLine{
			Number: index + 1, Masked: masked, Occurrences: occurrences, Rules: matched,
		})
	}
	preview.Masked = strings.Join(rendered, "\n")
	return preview
}

// Render writes the preview for a person reading a terminal.
//
// It prints the MASKED text and never the original: an operator running this against a real log
// line would otherwise have the secret printed back at them by the tool whose whole purpose is
// to stop it being printed.
func (p Preview) Render(policy *Policy) string {
	out := &strings.Builder{}
	fmt.Fprintf(out, "policy: %s\n", policy.Source())
	fmt.Fprintf(out, "rules in force: %d\n", len(policy.Rules()))
	for _, id := range policy.Rules() {
		fmt.Fprintf(out, "  %s\n", id)
	}
	if fields := policy.AlwaysMasked(); len(fields) > 0 {
		out.WriteString("fields excluded categorically:\n")
		for _, field := range fields {
			fmt.Fprintf(out, "  %s\n", field)
		}
	}

	fmt.Fprintf(out, "\nmasked %d occurrence(s) across %d line(s)\n",
		p.Occurrences, len(p.Lines))
	if p.Occurrences > 0 {
		out.WriteString("by rule:\n")
		for _, id := range sortedKeys(p.ByRule) {
			fmt.Fprintf(out, "  %s: %d\n", id, p.ByRule[id])
		}
	}

	out.WriteString("\nwhat would leave the cluster:\n")
	for _, line := range strings.Split(p.Masked, "\n") {
		fmt.Fprintf(out, "  %s\n", line)
	}
	return out.String()
}
