package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	relayv1 "github.com/OCluster/opencluster-relay/gen/go/opencluster/relay/v1"
)

type scriptedExecutor struct {
	result *relayv1.JobResult
}

func (s *scriptedExecutor) Execute(_ context.Context, _ *relayv1.JobAssignment) *relayv1.JobResult {
	return s.result
}

func assignment() *relayv1.JobAssignment {
	return &relayv1.JobAssignment{
		JobId:             "job-1",
		OrgId:             "org-1",
		RegistrationId:    "reg-1",
		LeaseEpoch:        3,
		CapabilityId:      "kubernetes.workload.runtime",
		CapabilityVersion: 1,
	}
}

func capture() (*slog.Logger, *bytes.Buffer) {
	buffer := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(buffer, nil)), buffer
}

func TestExecutor_EmitsOneAuditLinePerJob(t *testing.T) {
	logger, buffer := capture()
	inner := &scriptedExecutor{result: &relayv1.JobResult{
		JobId: "job-1", LeaseEpoch: 3,
		Outcome: &relayv1.JobResult_Result{Result: &relayv1.CapabilityResult{
			Result: &relayv1.CapabilityResult_KubernetesWorkloadRuntimeV1{
				KubernetesWorkloadRuntimeV1: &relayv1.KubernetesWorkloadRuntimeResultV1{
					Outcome: relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_SUCCESS,
				},
			},
		}},
	}}

	result := NewExecutor(inner, logger).Execute(context.Background(), assignment())
	if result == nil || result.GetJobId() != "job-1" {
		t.Fatalf("result must pass through: %+v", result)
	}

	var line map[string]any
	if err := json.Unmarshal(buffer.Bytes(), &line); err != nil {
		t.Fatalf("audit line is not one JSON object: %q", buffer.String())
	}
	if line["job_id"] != "job-1" || line["capability"] != "kubernetes.workload.runtime.v1" {
		t.Fatalf("audit line must identify the job and capability: %v", line)
	}
	if line["outcome"] != "result" {
		t.Fatalf("a typed result audits as outcome=result: %v", line)
	}
	if bytesLogged, ok := line["result_bytes"].(float64); !ok || bytesLogged < 1 {
		t.Fatalf("audit line must record the result's byte count for egress verification: %v", line)
	}
}

func TestExecutor_AuditsFailureKind(t *testing.T) {
	logger, buffer := capture()
	inner := &scriptedExecutor{result: &relayv1.JobResult{
		JobId: "job-1", LeaseEpoch: 3,
		Outcome: &relayv1.JobResult_Failure{
			Failure: &relayv1.JobFailure{Kind: relayv1.JobFailure_KIND_TIMEOUT},
		},
	}}

	NewExecutor(inner, logger).Execute(context.Background(), assignment())

	if !strings.Contains(buffer.String(), "KIND_TIMEOUT") {
		t.Fatalf("failure kind must be audited: %q", buffer.String())
	}
}

func TestExecutor_StampsExecutionDurationProvenance(t *testing.T) {
	logger, _ := capture()
	inner := &scriptedExecutor{result: &relayv1.JobResult{JobId: "job-1", LeaseEpoch: 3,
		Outcome: &relayv1.JobResult_Failure{
			Failure: &relayv1.JobFailure{Kind: relayv1.JobFailure_KIND_INTERNAL},
		}}}

	result := NewExecutor(inner, logger).Execute(context.Background(), assignment())

	if result.GetExecutionDurationMs() < 0 {
		t.Fatalf("duration provenance must be non-negative: %d", result.GetExecutionDurationMs())
	}
}
