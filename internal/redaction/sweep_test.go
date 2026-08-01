package redaction_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/open-cluster/oc-relay/internal/redaction"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// The assertion that matters is about what leaves, so every test here serializes the result
// and searches the BYTES. A test that inspected the returned struct would pass against an
// implementation that masked one copy and sent another, which is exactly the defect the
// negative assertion exists to catch.

const bearerSecret = "eyJhbGciOiJIUzI1NiJ9.QUJDREVGR0hJSktMTU5PUFFSUw.c2lnbmF0dXJlLXZhbHVl"

func logsResult(lines ...string) *relayv1.CapabilityResult {
	entries := make([]*relayv1.KubernetesLogLine, 0, len(lines))
	for _, line := range lines {
		entries = append(entries, &relayv1.KubernetesLogLine{Content: line})
	}
	return &relayv1.CapabilityResult{
		Result: &relayv1.CapabilityResult_KubernetesContainerLogsV1{
			KubernetesContainerLogsV1: &relayv1.KubernetesContainerLogsResultV1{
				Outcome:           relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_SUCCESS,
				Lines:             entries,
				ReturnedLineCount: int32(len(entries)),
				Complete:          true,
			},
		},
	}
}

// wire is what would actually be sent, as bytes.
func wire(t *testing.T, result *relayv1.CapabilityResult) string {
	t.Helper()
	encoded, err := proto.Marshal(result)
	if err != nil {
		t.Fatalf("serializing the result: %v", err)
	}
	return string(encoded)
}

func TestABearerTokenIsReplacedByTheMarkerAndDoesNotLeave(t *testing.T) {
	result := logsResult("GET /v1/orders failed: Authorization: Bearer " + bearerSecret)

	report, fault := redaction.Default().Sweep(result)
	if fault != nil {
		t.Fatalf("sweeping a well-formed result: %v", fault)
	}

	sent := wire(t, result)
	if strings.Contains(sent, bearerSecret) {
		t.Fatal("the token was serialized; redaction did not stop it leaving the cluster")
	}
	if !strings.Contains(sent, "[redacted:") {
		t.Fatal("nothing marks the masked span, so a reader cannot tell masking from absence")
	}
	if report == nil || len(report.GetFields()) != 1 {
		t.Fatalf("expected one reported field, got %v", report.GetFields())
	}
	field := report.GetFields()[0]
	if field.GetFieldName() != "kubernetes_container_logs_v1.lines.content" {
		t.Errorf("field name = %q", field.GetFieldName())
	}
	if field.GetMaskedOccurrenceCount() != 1 {
		t.Errorf("masked count = %d, want 1", field.GetMaskedOccurrenceCount())
	}
	if len(field.GetRuleIds()) == 0 {
		t.Error("no rule identifier reported, so nobody knows which rule to adjust")
	}
}

// The value is never sent in ANY form. A partial reveal leaks the identifying part and a
// digest of a low-entropy secret is recoverable, so both are failures here.
func TestNoFragmentOfTheSecretIsSerialized(t *testing.T) {
	result := logsResult("token=" + bearerSecret)
	if _, fault := redaction.Default().Sweep(result); fault != nil {
		t.Fatalf("sweeping: %v", fault)
	}

	sent := wire(t, result)
	for size := 12; size < len(bearerSecret); size += 7 {
		if strings.Contains(sent, bearerSecret[:size]) {
			t.Fatalf("a %d-character prefix of the secret survived serialization", size)
		}
	}
	if strings.Contains(sent, bearerSecret[len(bearerSecret)-12:]) {
		t.Fatal("the tail of the secret survived serialization")
	}
}

// Masking protects secrets; it must not destroy the substance of an investigation.
func TestStatusesExitCodesAndTimestampsAreUntouched(t *testing.T) {
	result := &relayv1.CapabilityResult{
		Result: &relayv1.CapabilityResult_KubernetesWorkloadRuntimeV1{
			KubernetesWorkloadRuntimeV1: &relayv1.KubernetesWorkloadRuntimeResultV1{
				Outcome: relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_SUCCESS,
				Workload: &relayv1.KubernetesWorkloadSummary{
					Kind: "Deployment", Name: "checkout", Namespace: "shop",
					Uid: "0f4d2a11-1111-2222-3333-444455556666",
				},
				Pods: []*relayv1.KubernetesPodRuntime{{
					Name: "checkout-abc", Phase: "Running", NodeName: "node-1",
					Containers: []*relayv1.KubernetesContainerRuntime{{
						Name: "api", Image: "registry.example.com/checkout:1.4.2",
						LastTermination: &relayv1.KubernetesContainerTermination{
							Reason: "OOMKilled", ExitCode: 137,
						},
					}},
				}},
				Complete: true,
			},
		},
	}

	report, fault := redaction.Default().Sweep(result)
	if fault != nil {
		t.Fatalf("sweeping: %v", fault)
	}
	if len(report.GetFields()) != 0 {
		t.Fatalf("a result carrying no secret reported masking: %v", report.GetFields())
	}

	sent := wire(t, result)
	for _, substance := range []string{
		"Deployment", "checkout", "shop", "Running", "node-1", "OOMKilled",
		"registry.example.com/checkout:1.4.2", "0f4d2a11-1111-2222-3333-444455556666",
	} {
		if !strings.Contains(sent, substance) {
			t.Errorf("%q was removed; masking destroyed the substance of the investigation",
				substance)
		}
	}
	summary := result.GetKubernetesWorkloadRuntimeV1()
	if code := summary.GetPods()[0].GetContainers()[0].GetLastTermination().GetExitCode(); code != 137 {
		t.Errorf("exit code = %d, want 137", code)
	}
}

func TestRedactionIsDeterministicAcrossRepeatedRuns(t *testing.T) {
	line := "db=postgres://svc:hunter2@db.internal:5432/orders retry=3 status=refused"

	var first string
	for run := range 8 {
		result := logsResult(line)
		if _, fault := redaction.Default().Sweep(result); fault != nil {
			t.Fatalf("sweeping: %v", fault)
		}
		sent := wire(t, result)
		if run == 0 {
			first = sent
			continue
		}
		if sent != first {
			t.Fatal("two sweeps of identical input produced different output; a recorded " +
				"transcript cannot stay valid and two runs of one scenario cannot be compared")
		}
	}
	if strings.Contains(first, "hunter2") {
		t.Fatal("the connection string's credential leaked")
	}
}

// Occurrences are counted across every element of a repeated field, and the rule
// identifiers are deduplicated rather than repeated once per hit.
func TestOccurrencesAreCountedAcrossEveryLine(t *testing.T) {
	result := logsResult(
		"attempt 1 password=hunter2",
		"attempt 2 password=hunter3",
		"attempt 3 succeeded",
	)
	report, fault := redaction.Default().Sweep(result)
	if fault != nil {
		t.Fatalf("sweeping: %v", fault)
	}
	if len(report.GetFields()) != 1 {
		t.Fatalf("expected one reported field, got %v", report.GetFields())
	}
	if count := report.GetFields()[0].GetMaskedOccurrenceCount(); count != 2 {
		t.Errorf("masked count = %d, want 2", count)
	}
	if ids := report.GetFields()[0].GetRuleIds(); len(ids) != 1 {
		t.Errorf("rule ids = %v, want one deduplicated identifier", ids)
	}
}

// An event message is free text a cluster writes about itself, and applications echo their
// own configuration into it. It is swept; the reason and the object beside it are not.
func TestEventMessagesAreSweptAndTheirReasonsAreNot(t *testing.T) {
	result := &relayv1.CapabilityResult{
		Result: &relayv1.CapabilityResult_KubernetesNamespaceEventsV1{
			KubernetesNamespaceEventsV1: &relayv1.KubernetesNamespaceEventsResultV1{
				Outcome: relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_SUCCESS,
				Events: []*relayv1.KubernetesEvent{{
					Type: "Warning", Reason: "FailedMount",
					Message:        "secret AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMIKfiCYEXAMPLEKEYabcd1234 rejected",
					InvolvedObject: &relayv1.KubernetesInvolvedObject{Kind: "Pod", Name: "checkout-abc"},
				}},
				Complete: true,
			},
		},
	}

	report, fault := redaction.Default().Sweep(result)
	if fault != nil {
		t.Fatalf("sweeping: %v", fault)
	}
	if len(report.GetFields()) != 1 ||
		report.GetFields()[0].GetFieldName() != "kubernetes_namespace_events_v1.events.message" {
		t.Fatalf("reported fields = %v", report.GetFields())
	}

	sent := wire(t, result)
	if strings.Contains(sent, "wJalrXUtnFEMIKfiCYEXAMPLEKEYabcd1234") {
		t.Fatal("the secret key leaked through an event message")
	}
	for _, substance := range []string{"Warning", "FailedMount", "Pod", "checkout-abc"} {
		if !strings.Contains(sent, substance) {
			t.Errorf("%q was removed from an event that carried no secret there", substance)
		}
	}
}

// A result with nothing to mask carries no report at all, so an absent report is readable as
// "nothing was masked" rather than as "redaction did not run".
func TestACleanResultCarriesNoReport(t *testing.T) {
	result := logsResult("listening on :8080", "ready")
	report, fault := redaction.Default().Sweep(result)
	if fault != nil {
		t.Fatalf("sweeping: %v", fault)
	}
	if report != nil {
		t.Fatalf("a clean result reported %v", report.GetFields())
	}
	if result.GetRedaction() != nil {
		t.Fatal("a clean result carries a redaction report on the wire")
	}
}
