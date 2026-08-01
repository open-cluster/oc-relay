package redaction_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/open-cluster/oc-relay/internal/redaction"
)

func parse(t *testing.T, body string) (*redaction.Policy, *redaction.Fault) {
	t.Helper()
	return redaction.Parse([]byte(body), "policy.yaml")
}

func mustParse(t *testing.T, body string) *redaction.Policy {
	t.Helper()
	policy, fault := parse(t, body)
	if fault != nil {
		t.Fatalf("parsing a well-formed policy: %v", fault)
	}
	return policy
}

func TestACustomerPatternMasksAShapeTheDefaultsMiss(t *testing.T) {
	// An internal identifier with no standard shape: no built-in rule can or should know it.
	const internal = "ACME-7f3d9b1c4e5a6f7b8c9d0e1f2a3b4c5d"

	if _, fault := redaction.Default().Sweep(logsResult("issued " + internal)); fault != nil {
		t.Fatalf("sweeping with defaults: %v", fault)
	}

	policy := mustParse(t, `
redaction:
  version: 1
  patterns:
    - id: acme.internal_token
      pattern: 'ACME-[0-9a-f]{32}'
`)
	result := logsResult("issued " + internal)
	report, fault := policy.Sweep(result)
	if fault != nil {
		t.Fatalf("sweeping with the customer policy: %v", fault)
	}
	if strings.Contains(wire(t, result), internal) {
		t.Fatal("the customer's own secret shape was serialized")
	}
	if ids := report.GetFields()[0].GetRuleIds(); len(ids) != 1 || ids[0] != "acme.internal_token" {
		t.Fatalf("rule ids = %v, want the customer's rule", ids)
	}
}

// The built-in set is always evaluated. A customer policy adds to it and the additions do not
// displace it, which is what "may only add" means in practice.
func TestACustomerPolicyStillMasksTheBuiltInShapes(t *testing.T) {
	policy := mustParse(t, `
redaction:
  version: 1
  patterns:
    - id: acme.internal_token
      pattern: 'ACME-[0-9a-f]{32}'
`)
	result := logsResult("Authorization: Bearer " + bearerSecret)
	if _, fault := policy.Sweep(result); fault != nil {
		t.Fatalf("sweeping: %v", fault)
	}
	if strings.Contains(wire(t, result), bearerSecret) {
		t.Fatal("adding a customer rule stopped a built-in one being applied")
	}
}

func TestACustomerPolicyCannotDisableABuiltInRule(t *testing.T) {
	// The only way to displace a built-in is to redefine its identifier, so the identifier is
	// what is refused. There is no "disable" key to refuse — that is the point of strict decoding.
	_, fault := parse(t, `
redaction:
  version: 1
  patterns:
    - id: builtin.json_web_token
      pattern: 'never-matches-anything'
`)
	if fault == nil {
		t.Fatal("a policy redefining a built-in rule was accepted")
	}
	if !strings.Contains(fault.Reason, "builtin.") {
		t.Errorf("the refusal does not name the fault: %q", fault.Reason)
	}
}

func TestAnUnknownKeyIsRefusedRatherThanIgnored(t *testing.T) {
	// Written the way an operator who believed the defaults could be switched off would write it.
	_, fault := parse(t, `
redaction:
  version: 1
  disabled_rules:
    - builtin.credential_assignment
`)
	if fault == nil {
		t.Fatal("a policy carrying a key this build does not know was accepted, so its author " +
			"would believe a built-in rule was off while it was on")
	}
}

func TestAnUnparseablePolicyIsAFaultAndNotAFallback(t *testing.T) {
	policy, fault := parse(t, "redaction:\n  version: 1\n   patterns: [\n")
	if fault == nil {
		t.Fatal("a policy that does not parse was accepted")
	}
	if policy != nil {
		t.Fatal("a fault returned a usable policy; a typo must not fall back to the defaults")
	}
	if !strings.Contains(fault.Error(), "policy.yaml") {
		t.Errorf("the fault does not name the file: %q", fault.Error())
	}
}

func TestAPatternThatDoesNotCompileIsRefusedAndNeverEchoed(t *testing.T) {
	const broken = `ACME-(unclosed`
	_, fault := parse(t, `
redaction:
  version: 1
  patterns:
    - id: acme.broken
      pattern: '`+broken+`'
`)
	if fault == nil {
		t.Fatal("a policy carrying an uncompilable pattern was accepted")
	}
	if !strings.Contains(fault.Reason, "acme.broken") {
		t.Errorf("the refusal does not name the rule: %q", fault.Reason)
	}
	if strings.Contains(fault.Error(), broken) {
		t.Error("the fault echoed the pattern; the file that governs disclosure must not " +
			"reach the Relay's diagnostics through its own error path")
	}
}

func TestAnUnknownFormatVersionIsRefused(t *testing.T) {
	if _, fault := parse(t, "redaction:\n  version: 9\n"); fault == nil {
		t.Fatal("a policy declaring an unknown format version was accepted")
	}
	if _, fault := parse(t, "redaction:\n  patterns: []\n"); fault == nil {
		t.Fatal("a policy declaring no format version was accepted")
	}
}

func TestAFieldNameNobodyRecognisesIsRefused(t *testing.T) {
	_, fault := parse(t, `
redaction:
  version: 1
  masked_fields:
    - kubernetes_container_logs_v1.lines.contents
`)
	if fault == nil {
		t.Fatal("a policy naming a field that does not exist was accepted, so its author would " +
			"believe a field was masked while nothing was")
	}
	if !strings.Contains(fault.Reason, "lines.content") {
		t.Errorf("the refusal does not name what could have been meant: %q", fault.Reason)
	}
}

func TestAMaskedFieldIsReplacedWholeAndReported(t *testing.T) {
	policy := mustParse(t, `
redaction:
  version: 1
  masked_fields:
    - kubernetes_container_logs_v1.lines.content
`)
	result := logsResult("nothing secret here at all", "listening on :8080")
	report, fault := policy.Sweep(result)
	if fault != nil {
		t.Fatalf("sweeping: %v", fault)
	}

	sent := wire(t, result)
	for _, gone := range []string{"nothing secret here", "listening on :8080"} {
		if strings.Contains(sent, gone) {
			t.Errorf("%q survived a categorically masked field", gone)
		}
	}
	if count := report.GetFields()[0].GetMaskedOccurrenceCount(); count != 2 {
		t.Errorf("masked count = %d, want one per line", count)
	}
	if ids := report.GetFields()[0].GetRuleIds(); len(ids) != 1 || ids[0] != "policy.masked_field" {
		t.Errorf("rule ids = %v, want the categorical exclusion named", ids)
	}
}

func TestLimitsMayBeLoweredAndNeverRaised(t *testing.T) {
	policy := mustParse(t, `
redaction:
  version: 1
  limits:
    sweep_budget: 250ms
    max_input_bytes: 4096
`)
	if policy.Budget() != 250*time.Millisecond {
		t.Errorf("budget = %s, want the lowered value", policy.Budget())
	}
	if policy.MaxInputBytes() != 4096 {
		t.Errorf("max input = %d, want the lowered value", policy.MaxInputBytes())
	}

	if _, fault := parse(t, "redaction:\n  version: 1\n  limits:\n    sweep_budget: 1h\n"); fault == nil {
		t.Error("a policy raising the sweep budget was accepted")
	}
	if _, fault := parse(t, "redaction:\n  version: 1\n  limits:\n    max_input_bytes: 999999999\n"); fault == nil {
		t.Error("a policy raising the input bound was accepted")
	}
}

// Exceeding a bound fails closed. A sweep that gave up and sent what it had already looked at
// would be silent disclosure wearing the costume of a safety limit.
func TestExceedingTheInputBoundFailsClosedRatherThanSkipping(t *testing.T) {
	policy := mustParse(t, `
redaction:
  version: 1
  limits:
    max_input_bytes: 64
`)
	result := logsResult(strings.Repeat("x", 40)+" password=hunter2", strings.Repeat("y", 40))

	report, fault := policy.Sweep(result)
	if fault == nil {
		t.Fatal("a result past the input bound was swept anyway")
	}
	if report != nil {
		t.Fatal("a faulted sweep produced a report, which reads as a result that may be sent")
	}
	if !strings.Contains(fault.Reason, "refused") {
		t.Errorf("the fault does not say the result is refused: %q", fault.Reason)
	}
}

// Exceeding the time budget fails closed rather than being skipped. A pattern that is skipped is
// a secret that is sent, so the result is dropped whole instead.
func TestExceedingTheTimeBudgetFailsClosedRatherThanSkipping(t *testing.T) {
	// A budget nothing can finish inside, and enough real work that the clock actually advances
	// past it. A single short line would not do: a sweep that fast can measure as zero elapsed on
	// a platform whose monotonic clock is coarse, and a test that depended on that would pass or
	// fail by machine rather than by behaviour.
	policy := mustParse(t, `
redaction:
  version: 1
  limits:
    sweep_budget: 1ns
`)

	lines := make([]string, 0, 2000)
	for i := range 2000 {
		lines = append(lines,
			fmt.Sprintf("request %d finished status=200 latency=%dms password=hunter%d", i, i, i))
	}
	result := logsResult(lines...)

	report, fault := policy.Sweep(result)
	if fault == nil {
		t.Fatal("a sweep past its time budget completed anyway")
	}
	if report != nil {
		t.Fatal("a faulted sweep produced a report, which reads as a result that may be sent")
	}
	if !strings.Contains(fault.Reason, "refused") {
		t.Errorf("the fault does not say the result is refused: %q", fault.Reason)
	}
	if !strings.Contains(fault.Reason, "budget") {
		t.Errorf("the fault does not name the bound that was exceeded: %q", fault.Reason)
	}
}

func TestAnEmptyPolicyFileIsAFault(t *testing.T) {
	if _, fault := parse(t, ""); fault == nil {
		t.Fatal("an empty policy file was accepted; a truncated write must not read as no policy")
	}
}

func TestASecondDocumentIsRefused(t *testing.T) {
	_, fault := parse(t, "redaction:\n  version: 1\n---\nredaction:\n  version: 1\n")
	if fault == nil {
		t.Fatal("a second document was silently ignored, and the ignored one is exactly the " +
			"one an operator would swear they configured")
	}
}

// What is in force has to be verifiable rather than assumed — and verifiable without printing
// the patterns, which are the policy itself.
func TestTheEffectivePolicyIsVisibleWithoutItsPatterns(t *testing.T) {
	policy := mustParse(t, `
redaction:
  version: 1
  patterns:
    - id: acme.internal_token
      pattern: 'ACME-[0-9a-f]{32}'
  masked_fields:
    - kubernetes_namespace_events_v1.events.message
`)
	rules := strings.Join(policy.Rules(), " ")
	if !strings.Contains(rules, "acme.internal_token") ||
		!strings.Contains(rules, "builtin.json_web_token") {
		t.Errorf("the effective rule set is not visible: %v", policy.Rules())
	}
	if strings.Contains(rules, "ACME-[0-9a-f]") {
		t.Error("the diagnostics carry the pattern, not just its identifier")
	}
	if fields := policy.AlwaysMasked(); len(fields) != 1 {
		t.Errorf("categorically masked fields = %v", fields)
	}
	if policy.Source() != "policy.yaml" {
		t.Errorf("source = %q", policy.Source())
	}
}
