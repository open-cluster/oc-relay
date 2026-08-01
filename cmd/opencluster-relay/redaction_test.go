package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/open-cluster/oc-relay/internal/redaction"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// The composition root's own guarantee: what the Relay writes about a job is written about the
// result AFTER masking. These test the wiring rather than the engine — the engine has its own
// suite, and what can only go wrong here is the order.

const leakedSecret = "AKIAIOSFODNN7EXAMPLE"

type loggingExecutorStub struct{}

func (loggingExecutorStub) Execute(
	_ context.Context, job *relayv1.JobAssignment,
) *relayv1.JobResult {
	return &relayv1.JobResult{
		JobId:      job.GetJobId(),
		LeaseEpoch: job.GetLeaseEpoch(),
		Outcome: &relayv1.JobResult_Result{Result: &relayv1.CapabilityResult{
			Result: &relayv1.CapabilityResult_KubernetesContainerLogsV1{
				KubernetesContainerLogsV1: &relayv1.KubernetesContainerLogsResultV1{
					Outcome: relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_SUCCESS,
					Lines: []*relayv1.KubernetesLogLine{
						{Content: "using key " + leakedSecret + " for uploads"},
					},
					ReturnedLineCount: 1,
					Complete:          true,
				},
			},
		}},
	}
}

func TestTheRelayLogsTheRedactedResultAndNotWhatItRemoved(t *testing.T) {
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))

	executor := guardedExecutor(loggingExecutorStub{}, redaction.Load(""), logger)
	result := executor.Execute(context.Background(), &relayv1.JobAssignment{
		JobId: "job-1", LeaseEpoch: 3,
		CapabilityId: "kubernetes.container.logs", CapabilityVersion: 1,
	})

	if strings.Contains(logs.String(), leakedSecret) {
		t.Fatal("the Relay logged the secret it had just removed")
	}

	// The audit line measures the redacted result. A line reporting the unredacted size would
	// leak the length of what was masked, which is the one measurement the marker exists to
	// avoid producing.
	encoded, err := proto.Marshal(result)
	if err != nil {
		t.Fatalf("serializing: %v", err)
	}
	if !strings.Contains(logs.String(), `"result_bytes":`+strconv.Itoa(len(encoded))) {
		t.Errorf("the audit line does not measure the redacted result; log was %s", logs.String())
	}
	if strings.Contains(string(encoded), leakedSecret) {
		t.Fatal("the secret reached the wire through the composed executor")
	}
}

func TestTheEffectivePolicyIsReportedWithoutItsPatterns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "redaction.yaml")
	if err := os.WriteFile(path, []byte(`
redaction:
  version: 1
  patterns:
    - id: acme.internal_token
      pattern: 'ACME-[0-9a-f]{32}'
`), 0o600); err != nil {
		t.Fatal(err)
	}

	logs := &bytes.Buffer{}
	redaction.Load(path).Describe(slog.New(slog.NewJSONHandler(logs, nil)))

	written := logs.String()
	if !strings.Contains(written, "acme.internal_token") {
		t.Error("the effective policy is not visible in the Relay's own diagnostics")
	}
	if strings.Contains(written, "ACME-[0-9a-f]") {
		t.Error("the diagnostics carry the raw policy; the file that governs disclosure must " +
			"not be printed by the process it governs")
	}
}

func TestAFaultedPolicyIsReportedAsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "redaction.yaml")
	if err := os.WriteFile(path, []byte("redaction:\n  version: 77\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	logs := &bytes.Buffer{}
	redaction.Load(path).Describe(slog.New(slog.NewJSONHandler(logs, nil)))

	if !strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Errorf("a faulted policy was not reported as an error: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "version 77") {
		t.Errorf("the report does not name the fault: %s", logs.String())
	}
}

func TestTheDryRunReportsLocallyAndRefusesUnderAFault(t *testing.T) {
	sample := filepath.Join(t.TempDir(), "sample.log")
	if err := os.WriteFile(sample,
		[]byte("started\nkey="+leakedSecret+"\nready\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	if err := dryRun(redaction.Load(""), sample, out); err != nil {
		t.Fatalf("a dry run against the defaults: %v", err)
	}
	if strings.Contains(out.String(), leakedSecret) {
		t.Fatal("the dry run printed the secret back at the operator")
	}
	for _, expected := range []string{"builtin.cloud_access_key_id", "started", "ready"} {
		if !strings.Contains(out.String(), expected) {
			t.Errorf("the report does not carry %q", expected)
		}
	}

	// A faulted policy previews nothing. Showing what the DEFAULTS would have done to text the
	// operator's own policy was meant to govern is the one answer that would mislead them.
	faulted := filepath.Join(t.TempDir(), "redaction.yaml")
	if err := os.WriteFile(faulted, []byte("redaction:\n  version: 77\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := dryRun(redaction.Load(faulted), sample, &bytes.Buffer{}); err == nil {
		t.Fatal("a dry run under a faulted policy previewed the defaults")
	}
}
