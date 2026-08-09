package redaction_test

import (
	"strings"
	"testing"

	"github.com/open-cluster/oc-relay/internal/redaction"
)

func TestADryRunReportsWhatWouldBeMaskedWithoutPrintingIt(t *testing.T) {
	sample := strings.Join([]string{
		"2026-08-01T10:00:00Z starting checkout 1.4.2",
		"2026-08-01T10:00:01Z connecting to postgres://svc:hunter2@db.internal:5432/orders",
		"2026-08-01T10:00:02Z ready, listening on :8080",
	}, "\n")

	preview := redaction.Default().DryRun(sample)

	if preview.Occurrences != 1 {
		t.Errorf("occurrences = %d, want 1", preview.Occurrences)
	}
	if len(preview.Lines) != 1 || preview.Lines[0].Number != 2 {
		t.Fatalf("lines = %v, want the second line only", preview.Lines)
	}
	if preview.ByRule["builtin.credentialed_connection_string"] != 1 {
		t.Errorf("by rule = %v", preview.ByRule)
	}

	rendered := preview.Render(redaction.Default())
	if strings.Contains(rendered, "hunter2") {
		t.Fatal("the dry run printed the secret back at the operator")
	}
	for _, expected := range []string{
		"builtin.credentialed_connection_string", "listening on :8080", "checkout 1.4.2",
	} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("the report does not carry %q", expected)
		}
	}
}

// The reason this exists: a rule that is too broad has to be visible as a number before it
// costs an investigation.
func TestADryRunMakesAnOverBroadCustomerRuleVisible(t *testing.T) {
	policy, fault := redaction.Parse([]byte(`
redaction:
  version: 1
  patterns:
    - id: acme.too_broad
      pattern: '[0-9]+'
`), "policy.yaml")
	if fault != nil {
		t.Fatalf("parsing: %v", fault)
	}

	preview := policy.DryRun("orders=412 latency=93ms retries=2")
	if preview.ByRule["acme.too_broad"] != 1 {
		t.Errorf("by rule = %v", preview.ByRule)
	}
	if preview.Occurrences != 3 {
		t.Errorf("occurrences = %d; a rule claiming every number in a line is exactly what "+
			"this is for", preview.Occurrences)
	}
	if strings.Contains(preview.Masked, "412") {
		t.Error("the preview does not reflect what the rule would do")
	}
}

func TestADryRunOverCleanTextReportsNothing(t *testing.T) {
	preview := redaction.Default().DryRun("ready\nlistening on :8080\nhealthy")
	if preview.Occurrences != 0 || len(preview.Lines) != 0 {
		t.Fatalf("clean text reported %d occurrences", preview.Occurrences)
	}
	if preview.Masked != "ready\nlistening on :8080\nhealthy" {
		t.Errorf("clean text was altered: %q", preview.Masked)
	}
}
