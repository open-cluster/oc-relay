package redaction_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/open-cluster/oc-relay/internal/redaction"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// stubExecutor is the capability half of the seam. It is a stub rather than a real capability
// because what is under test is the enforcement point, not a cluster read — and it records
// whether it ran, which is how "refused BEFORE it touched the cluster" is observable at all.
type stubExecutor struct {
	ran    int
	result *relayv1.CapabilityResult
}

func (s *stubExecutor) Execute(
	_ context.Context, job *relayv1.JobAssignment,
) *relayv1.JobResult {
	s.ran++
	return &relayv1.JobResult{
		JobId:      job.GetJobId(),
		LeaseEpoch: job.GetLeaseEpoch(),
		Outcome:    &relayv1.JobResult_Result{Result: s.result},
	}
}

func job(capability string, version uint32) *relayv1.JobAssignment {
	return &relayv1.JobAssignment{
		JobId: "job-1", LeaseEpoch: 7,
		CapabilityId: capability, CapabilityVersion: version,
	}
}

// sent is the bytes the session would put on the stream for this result.
func sent(t *testing.T, result *relayv1.JobResult) string {
	t.Helper()
	encoded, err := proto.Marshal(result)
	if err != nil {
		t.Fatalf("serializing the job result: %v", err)
	}
	return string(encoded)
}

func TestTheEnforcementPointMasksBeforeAnythingIsSerialized(t *testing.T) {
	inner := &stubExecutor{result: logsResult("Authorization: Bearer " + bearerSecret)}
	guard := redaction.Load("")

	result := guard.Enforce(inner).Execute(context.Background(), job("kubernetes.container.logs", 1))

	if strings.Contains(sent(t, result), bearerSecret) {
		t.Fatal("the token reached the wire; the enforcement point is not on the path")
	}
	report := result.GetResult().GetRedaction()
	if report == nil || len(report.GetFields()) != 1 {
		t.Fatalf("the result carries no redaction report: %v", report)
	}
}

// A capability cannot opt out, and adding one cannot bypass the point: the sweep is driven by
// the declaration table over the result descriptor rather than by a per-capability branch.
func TestEveryCapabilityPassesThroughTheSamePoint(t *testing.T) {
	for _, capability := range []string{
		"kubernetes.container.logs", "kubernetes.namespace.events", "kubernetes.workload.runtime",
	} {
		t.Run(capability, func(t *testing.T) {
			inner := &stubExecutor{result: &relayv1.CapabilityResult{}}
			result := redaction.Load("").Enforce(inner).Execute(
				context.Background(), job(capability, 1))
			if result.GetFailure() != nil {
				t.Fatalf("a clean result was refused: %v", result.GetFailure().GetKind())
			}
			if inner.ran != 1 {
				t.Fatalf("the capability ran %d times", inner.ran)
			}
		})
	}
}

func TestAFaultedPolicyStopsCapabilitiesThatCouldEmitFreeText(t *testing.T) {
	guard := redaction.Load(writePolicy(t, "redaction:\n  version: 1\n  patterns:\n    - id: x\n"))
	if guard.Fault() == nil {
		t.Fatal("a policy with a pattern-less rule was accepted")
	}

	inner := &stubExecutor{result: logsResult("anything")}
	result := guard.Enforce(inner).Execute(
		context.Background(), job("kubernetes.container.logs", 1))

	if kind := result.GetFailure().GetKind(); kind != relayv1.JobFailure_KIND_LOCAL_POLICY_REFUSED {
		t.Fatalf("failure kind = %v, want the local-policy refusal", kind)
	}
	if inner.ran != 0 {
		t.Fatal("the cluster was read under a faulted policy; the refusal must come first")
	}
}

// The refusal is bounded to what could actually leak. A capability whose result carries no
// declared free-text field keeps working, because stopping it would be a self-inflicted outage
// rather than a disclosure control.
func TestAFaultedPolicyStillPermitsACapabilityWithNoFreeText(t *testing.T) {
	guard := redaction.Load(writePolicy(t, "redaction:\n  version: 4\n"))
	if guard.Fault() == nil {
		t.Fatal("a policy at an unknown version was accepted")
	}

	inner := &stubExecutor{result: &relayv1.CapabilityResult{
		Result: &relayv1.CapabilityResult_KubernetesWorkloadRuntimeV1{
			KubernetesWorkloadRuntimeV1: &relayv1.KubernetesWorkloadRuntimeResultV1{
				Outcome:  relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_SUCCESS,
				Workload: &relayv1.KubernetesWorkloadSummary{Kind: "Deployment", Name: "checkout"},
				Complete: true,
			},
		},
	}}
	result := guard.Enforce(inner).Execute(
		context.Background(), job("kubernetes.workload.runtime", 1))

	if result.GetFailure() != nil {
		t.Fatalf("a capability that cannot emit free text was refused: %v",
			result.GetFailure().GetKind())
	}
	if inner.ran != 1 {
		t.Fatalf("the capability ran %d times", inner.ran)
	}
}

// A capability this build does not know is treated as able to emit free text. The safe answer
// to "might this emit text nobody classified" is always yes.
func TestAnUnknownCapabilityIsRefusedUnderAFault(t *testing.T) {
	guard := redaction.Load(writePolicy(t, "redaction:\n  version: 4\n"))
	inner := &stubExecutor{result: &relayv1.CapabilityResult{}}

	result := guard.Enforce(inner).Execute(context.Background(), job("kubernetes.future.thing", 9))

	if kind := result.GetFailure().GetKind(); kind != relayv1.JobFailure_KIND_LOCAL_POLICY_REFUSED {
		t.Fatalf("failure kind = %v, want the local-policy refusal", kind)
	}
	if inner.ran != 0 {
		t.Fatal("an unknown capability ran under a faulted policy")
	}
}

// A sweep that faults mid-result drops the whole result. Sending the part that was already
// swept would be a partial read presented as a complete one.
func TestASweepFaultDropsTheResultEntirely(t *testing.T) {
	policyFile := writePolicy(t, "redaction:\n  version: 1\n  limits:\n    max_input_bytes: 8\n")
	inner := &stubExecutor{result: logsResult(strings.Repeat("a", 64), "password=hunter2")}

	result := redaction.Load(policyFile).Enforce(inner).Execute(
		context.Background(), job("kubernetes.container.logs", 1))

	if kind := result.GetFailure().GetKind(); kind != relayv1.JobFailure_KIND_LOCAL_POLICY_REFUSED {
		t.Fatalf("failure kind = %v, want the local-policy refusal", kind)
	}
	if strings.Contains(sent(t, result), "aaaa") {
		t.Fatal("part of a faulted result reached the wire")
	}
}

// A missing file is not a fault: it is an install nobody has configured, which the built-in
// defaults are there to cover.
func TestNoPolicyFileMeansTheBuiltInDefaults(t *testing.T) {
	guard := redaction.Load("")
	if guard.Fault() != nil {
		t.Fatalf("an unconfigured install faulted: %v", guard.Fault())
	}
	if len(guard.Policy().Rules()) == 0 {
		t.Fatal("an unconfigured install has no rules, so the first install is not safe by default")
	}
}

// A path that was configured and cannot be read IS a fault. An operator who named a file meant
// it to be enforced, and a missing one is a deployment error rather than a decision.
func TestAConfiguredPolicyFileThatCannotBeReadIsAFault(t *testing.T) {
	guard := redaction.Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if guard.Fault() == nil {
		t.Fatal("a configured policy file that does not exist was treated as no policy")
	}
}

func writePolicy(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "redaction.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the policy file: %v", err)
	}
	return path
}
