package logs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/open-cluster/oc-relay/internal/capabilities"
	"github.com/open-cluster/oc-relay/internal/kube"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// These tests pin what the control plane can observe about a log read: which lines came
// back, whether either bound bound, how much was withheld, and which of the several ways
// a container can be absent this one was. The refusal paths get more attention than the
// happy path deliberately — there is no parity oracle for this capability, so a wrong
// refusal would be found by a customer rather than by a differential comparison.

var readTime = time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

type fakeReader struct {
	pod    *corev1.Pod
	podErr error

	read    *kube.LogRead
	readErr error

	gotRequest kube.LogRequest
	logCalls   int
	block      chan struct{}
}

func (f *fakeReader) GetPod(_ context.Context, _, _ string) (*corev1.Pod, error) {
	if f.podErr != nil {
		return nil, f.podErr
	}
	return f.pod, nil
}

func (f *fakeReader) ContainerLogs(ctx context.Context, request kube.LogRequest) (*kube.LogRead, error) {
	f.logCalls++
	f.gotRequest = request
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.read, nil
}

// pod builds a pod carrying one container, optionally with a previous termination.
func pod(container string, restarted bool) *corev1.Pod {
	status := corev1.ContainerStatus{Name: container}
	if restarted {
		status.RestartCount = 3
		status.LastTerminationState = corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"},
		}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout-7f", Namespace: "shop"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: container}}},
		Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{status}},
	}
}

// stamped renders lines the way the API does with Timestamps enabled.
func stamped(lines ...string) []byte {
	var builder strings.Builder
	for index, line := range lines {
		builder.WriteString(readTime.Add(time.Duration(index) * time.Second).Format(time.RFC3339Nano))
		builder.WriteString(" ")
		builder.WriteString(line)
		builder.WriteString("\n")
	}
	return []byte(builder.String())
}

func job(namespace, podName, container string, previous bool, maxLines, maxBytes uint32) *relayv1.JobAssignment {
	return &relayv1.JobAssignment{
		JobId: "job-1", LeaseEpoch: 1,
		DeadlineBudget: durationpb.New(30 * time.Second),
		Arguments: &relayv1.CapabilityArguments{
			Arguments: &relayv1.CapabilityArguments_KubernetesContainerLogsV1{
				KubernetesContainerLogsV1: &relayv1.KubernetesContainerLogsArgsV1{
					Namespace:     namespace,
					PodName:       podName,
					ContainerName: container,
					Previous:      previous,
					MaxLines:      maxLines,
					MaxBytes:      maxBytes,
				},
			},
		},
	}
}

func newExecutor(reader kube.LogReader, options ...func(*Options)) *Executor {
	var opts Options
	for _, apply := range options {
		apply(&opts)
	}
	return NewExecutor(reader, opts)
}

func logsResult(t *testing.T, r *relayv1.JobResult) *relayv1.KubernetesContainerLogsResultV1 {
	t.Helper()
	result := r.GetResult().GetKubernetesContainerLogsV1()
	if result == nil {
		t.Fatalf("expected a kubernetes.container.logs result, got %+v", r)
	}
	return result
}

func TestExecutor_ReturnsLinesWithTheirTimestampsAndReportsACompleteRead(t *testing.T) {
	reader := &fakeReader{
		pod:  pod("api", false),
		read: &kube.LogRead{Raw: stamped("listening on :8080", "serving")},
	}

	result := logsResult(t, newExecutor(reader).Execute(context.Background(),
		job("shop", "checkout-7f", "api", false, 100, 65536)))

	if result.GetOutcome() != relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_SUCCESS {
		t.Fatalf("expected SUCCESS, got %v", result.GetOutcome())
	}
	if result.GetReturnedLineCount() != 2 || !result.GetComplete() {
		t.Fatalf("expected 2 lines and a complete read, got count=%d complete=%v",
			result.GetReturnedLineCount(), result.GetComplete())
	}
	if got := result.GetLines()[0]; got.GetContent() != "listening on :8080" {
		t.Fatalf("the timestamp prefix must be stripped from the content, got %q", got.GetContent())
	}
	if !result.GetLines()[0].GetAt().AsTime().Equal(readTime) {
		t.Fatalf("each line must carry its own emission time, got %v", result.GetLines()[0].GetAt())
	}
	if result.GetWithheldByteCount() != 0 {
		t.Fatalf("a complete read withheld nothing, got %d", result.GetWithheldByteCount())
	}
}

func TestExecutor_PreviousSelectsTheTerminatedInstance(t *testing.T) {
	reader := &fakeReader{
		pod:  pod("api", true),
		read: &kube.LogRead{Raw: stamped("panic: out of memory")},
	}

	result := logsResult(t, newExecutor(reader).Execute(context.Background(),
		job("shop", "checkout-7f", "api", true, 100, 65536)))

	if result.GetOutcome() != relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_SUCCESS {
		t.Fatalf("expected SUCCESS, got %v", result.GetOutcome())
	}
	if !reader.gotRequest.Previous {
		t.Fatal("the previous flag must reach the port; reading the replacement container " +
			"would answer a different question")
	}
}

func TestExecutor_PreviousOnAContainerThatNeverRestartedIsItsOwnOutcome(t *testing.T) {
	reader := &fakeReader{pod: pod("api", false)}

	result := logsResult(t, newExecutor(reader).Execute(context.Background(),
		job("shop", "checkout-7f", "api", true, 100, 65536)))

	if result.GetOutcome() !=
		relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_PREVIOUS_CONTAINER_NOT_FOUND {
		t.Fatalf("a container that has never died has no previous instance, and that is not "+
			"the same as a wrong container name; got %v", result.GetOutcome())
	}
	if reader.logCalls != 0 {
		t.Fatal("the absence is decided from the pod, not by asking the API for a stream " +
			"and reading its error message")
	}
	if result.GetComplete() {
		t.Fatal("a not-found is never a complete read")
	}
}

func TestExecutor_ContainerNotOnThePodIsTypedAndNeverReachesTheLogStream(t *testing.T) {
	reader := &fakeReader{pod: pod("api", false)}

	result := logsResult(t, newExecutor(reader).Execute(context.Background(),
		job("shop", "checkout-7f", "sidecar", false, 100, 65536)))

	if result.GetOutcome() != relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_CONTAINER_NOT_FOUND {
		t.Fatalf("expected CONTAINER_NOT_FOUND, got %v", result.GetOutcome())
	}
	if reader.logCalls != 0 {
		t.Fatal("a container that is not on the pod must not produce a log request")
	}
}

func TestExecutor_InitAndEphemeralContainersAreRealContainers(t *testing.T) {
	target := pod("api", false)
	target.Spec.InitContainers = []corev1.Container{{Name: "migrate"}}
	target.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "migrate"}}
	reader := &fakeReader{pod: target, read: &kube.LogRead{Raw: stamped("migrating")}}

	result := logsResult(t, newExecutor(reader).Execute(context.Background(),
		job("shop", "checkout-7f", "migrate", false, 100, 65536)))

	if result.GetOutcome() != relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_SUCCESS {
		t.Fatalf("an init container is a container; got %v", result.GetOutcome())
	}
}

func TestExecutor_MissingPodIsTypedRatherThanAFailure(t *testing.T) {
	reader := &fakeReader{podErr: &kube.ReadError{Outcome: kube.OutcomePodNotFound}}

	result := logsResult(t, newExecutor(reader).Execute(context.Background(),
		job("shop", "gone", "api", false, 100, 65536)))

	if result.GetOutcome() != relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_POD_NOT_FOUND {
		t.Fatalf("expected POD_NOT_FOUND, got %v", result.GetOutcome())
	}
}

func TestExecutor_UnauthorizedIsNeverReportedAsEmpty(t *testing.T) {
	fromPod := &fakeReader{podErr: &kube.ReadError{Outcome: kube.OutcomeUnauthorized}}
	if got := logsResult(t, newExecutor(fromPod).Execute(context.Background(),
		job("shop", "checkout-7f", "api", false, 100, 65536))); got.GetOutcome() !=
		relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_UNAUTHORIZED {
		t.Fatalf("a refused pod read is UNAUTHORIZED, got %v", got.GetOutcome())
	}

	// The log subresource is granted separately from pods, so a Relay may read the pod and
	// be refused its output. That refusal must not arrive as a container with nothing to say.
	fromStream := &fakeReader{
		pod:     pod("api", false),
		readErr: &kube.ReadError{Outcome: kube.OutcomeUnauthorized},
	}
	result := logsResult(t, newExecutor(fromStream).Execute(context.Background(),
		job("shop", "checkout-7f", "api", false, 100, 65536)))
	if result.GetOutcome() != relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_UNAUTHORIZED {
		t.Fatalf("a refused log stream is UNAUTHORIZED, got %v", result.GetOutcome())
	}
	if result.GetComplete() || result.GetReturnedLineCount() != 0 {
		t.Fatal("a refused read is empty AND incomplete; reporting it as an empty complete " +
			"read is how an RBAC mistake becomes a certified negative")
	}
}

func TestExecutor_MoreLinesThanTheBoundKeepsTheNewestAndReportsTruncation(t *testing.T) {
	// The port is asked for one line past the bound so that binding is detectable at all.
	lines := make([]string, 0, 11)
	for index := range 11 {
		lines = append(lines, fmt.Sprintf("line-%02d", index))
	}
	reader := &fakeReader{pod: pod("api", false), read: &kube.LogRead{Raw: stamped(lines...)}}

	result := logsResult(t, newExecutor(reader).Execute(context.Background(),
		job("shop", "checkout-7f", "api", false, 10, 65536)))

	if !result.GetTruncatedByLineBound() || result.GetComplete() {
		t.Fatalf("the line bound bound: truncated=%v complete=%v",
			result.GetTruncatedByLineBound(), result.GetComplete())
	}
	if result.GetReturnedLineCount() != 10 {
		t.Fatalf("the effective bound is what is returned, got %d", result.GetReturnedLineCount())
	}
	if got := result.GetLines()[0].GetContent(); got != "line-01" {
		t.Fatalf("the NEWEST lines are the ones that explain a failure, so the oldest is "+
			"what is dropped; first returned line was %q", got)
	}
	if result.GetWithheldByteCount() == 0 {
		t.Fatal("a truncated read must say how much of what it saw it did not return")
	}
}

func TestExecutor_OneLineLongerThanTheByteBoundIsTruncatedByBytes(t *testing.T) {
	// A line cap alone is defeated by exactly this: one line, well inside the line bound,
	// carrying more than the whole read is allowed to.
	reader := &fakeReader{
		pod:  pod("api", false),
		read: &kube.LogRead{Raw: stamped("short", strings.Repeat("x", 9000))},
	}

	result := logsResult(t, newExecutor(reader).Execute(context.Background(),
		job("shop", "checkout-7f", "api", false, 100, 4096)))

	if !result.GetTruncatedByByteBound() || result.GetComplete() {
		t.Fatalf("the byte bound must bind independently of the line bound: "+
			"truncated=%v complete=%v", result.GetTruncatedByByteBound(), result.GetComplete())
	}
	if result.GetReturnedByteCount() > 4096 {
		t.Fatalf("more bytes left the cluster than the bound allowed: %d",
			result.GetReturnedByteCount())
	}
}

func TestExecutor_AFetchThatHitTheReadCeilingIsIncompleteAndDropsThePartialLine(t *testing.T) {
	// The ceiling landed mid-line: the raw has no final newline, so the last thing in it is
	// half a sentence rather than something the container finished saying.
	cut := append(stamped("first", "second"), []byte(readTime.Format(time.RFC3339Nano)+" third-but-")...)
	reader := &fakeReader{pod: pod("api", false), read: &kube.LogRead{Raw: cut, Truncated: true}}

	result := logsResult(t, newExecutor(reader).Execute(context.Background(),
		job("shop", "checkout-7f", "api", false, 100, 65536)))

	if result.GetComplete() {
		t.Fatal("a fetch that stopped at its ceiling is not a complete read")
	}
	if !result.GetTruncatedByByteBound() {
		t.Fatal("stopping at a byte ceiling is a byte truncation")
	}
	if result.GetReturnedLineCount() != 2 {
		t.Fatalf("the partial final line must be dropped, got %d lines", result.GetReturnedLineCount())
	}
	for _, line := range result.GetLines() {
		if strings.HasPrefix(line.GetContent(), "third") {
			t.Fatal("a fragment must never be reported as a line the container wrote")
		}
	}
}

func TestExecutor_AFetchCutExactlyOnALineBoundaryKeepsEveryWholeLine(t *testing.T) {
	// The ceiling landed on the newline, so every line present is one the container
	// finished. Dropping the last one here would discard a whole line to no purpose — and
	// the newest line is the one a crash is explained by.
	reader := &fakeReader{
		pod:  pod("api", false),
		read: &kube.LogRead{Raw: stamped("first", "second", "third"), Truncated: true},
	}

	result := logsResult(t, newExecutor(reader).Execute(context.Background(),
		job("shop", "checkout-7f", "api", false, 100, 65536)))

	if result.GetReturnedLineCount() != 3 {
		t.Fatalf("every whole line must survive a cut that fell on a boundary, got %d",
			result.GetReturnedLineCount())
	}
	if result.GetComplete() {
		t.Fatal("the read still saw less than the log holds, so it is still incomplete")
	}
}

func TestExecutor_AFinalLineWithoutANewlineIsStillALineTheContainerWrote(t *testing.T) {
	// A process that died mid-write leaves no trailing newline. That last line is very
	// often the panic.
	reader := &fakeReader{
		pod: pod("api", false),
		read: &kube.LogRead{
			Raw: append(stamped("starting"),
				[]byte(readTime.Format(time.RFC3339Nano)+" panic: runtime error")...),
		},
	}

	result := logsResult(t, newExecutor(reader).Execute(context.Background(),
		job("shop", "checkout-7f", "api", false, 100, 65536)))

	if result.GetReturnedLineCount() != 2 {
		t.Fatalf("a complete read's unterminated final line is real output, got %d lines",
			result.GetReturnedLineCount())
	}
	if got := result.GetLines()[1].GetContent(); got != "panic: runtime error" {
		t.Fatalf("the last thing a dying process said must survive, got %q", got)
	}
	if !result.GetComplete() {
		t.Fatal("nothing bound this read, so it is complete")
	}
}

func TestExecutor_LocalCapsLowerBothBoundsAndAreReported(t *testing.T) {
	reader := &fakeReader{pod: pod("api", false), read: &kube.LogRead{}}
	exec := newExecutor(reader, func(o *Options) {
		o.LocalMaxLines, o.LocalMaxBytes = 20, 8192
	})

	result := logsResult(t, exec.Execute(context.Background(),
		job("shop", "checkout-7f", "api", false, 2000, 262144)))

	if result.GetAppliedMaxLines() != 20 || result.GetAppliedMaxBytes() != 8192 {
		t.Fatalf("the effective bounds must be the local ones, got lines=%d bytes=%d",
			result.GetAppliedMaxLines(), result.GetAppliedMaxBytes())
	}
	if reader.gotRequest.TailLines != 21 {
		t.Fatalf("the port is asked for one line past the bound so that binding is "+
			"detectable, got %d", reader.gotRequest.TailLines)
	}
	if reader.gotRequest.ReadCeiling < 8192 {
		t.Fatalf("the fetch ceiling must be at least the emitted bound, got %d",
			reader.gotRequest.ReadCeiling)
	}
}

func TestExecutor_NoDispatchedValueCanRaiseALocalCap(t *testing.T) {
	reader := &fakeReader{pod: pod("api", false), read: &kube.LogRead{}}
	exec := newExecutor(reader, func(o *Options) { o.LocalMaxBytes = 2048 })

	result := logsResult(t, exec.Execute(context.Background(),
		job("shop", "checkout-7f", "api", false, 4_000_000, 4_000_000)))

	if result.GetAppliedMaxBytes() != 2048 {
		t.Fatalf("a local byte cap below the capability's own floor must still win, got %d",
			result.GetAppliedMaxBytes())
	}
}

func TestExecutor_NamespaceOutsideTheLocalAllowlistIsRefusedByPolicy(t *testing.T) {
	reader := &fakeReader{pod: pod("api", false), read: &kube.LogRead{}}
	exec := newExecutor(reader, func(o *Options) {
		o.Policy = capabilities.LocalPolicy{AllowedNamespaces: map[string]bool{"shop": true}}
	})

	result := exec.Execute(context.Background(), job("payments", "checkout-7f", "api", false, 10, 4096))

	if result.GetFailure().GetKind() != relayv1.JobFailure_KIND_LOCAL_POLICY_REFUSED {
		t.Fatalf("a namespace outside the allowlist must be refused where the operator "+
			"controls it, got %+v", result)
	}
	if reader.logCalls != 0 {
		t.Fatal("policy is enforced before the cluster is touched")
	}
}

func TestExecutor_RejectsArgumentsTheControlPlaneShouldNeverHaveSent(t *testing.T) {
	for name, assignment := range map[string]*relayv1.JobAssignment{
		"uppercase namespace":           job("Shop", "checkout-7f", "api", false, 10, 4096),
		"empty pod":                     job("shop", "", "api", false, 10, 4096),
		"empty container":               job("shop", "checkout-7f", "", false, 10, 4096),
		"pod that is not an identifier": job("shop", "checkout 7f", "api", false, 10, 4096),
	} {
		reader := &fakeReader{pod: pod("api", false), read: &kube.LogRead{}}
		result := newExecutor(reader).Execute(context.Background(), assignment)
		if result.GetFailure().GetKind() != relayv1.JobFailure_KIND_ARGUMENTS_REJECTED {
			t.Fatalf("%s must be rejected, got %+v", name, result)
		}
		if reader.logCalls != 0 {
			t.Fatalf("%s must be rejected before the cluster is touched", name)
		}
	}
}

func TestExecutor_ALineWithoutAReadableTimestampKeepsItsContentAndCarriesNoTime(t *testing.T) {
	reader := &fakeReader{
		pod:  pod("api", false),
		read: &kube.LogRead{Raw: []byte("not-a-timestamp and the rest of the line\n")},
	}

	result := logsResult(t, newExecutor(reader).Execute(context.Background(),
		job("shop", "checkout-7f", "api", false, 100, 65536)))

	line := result.GetLines()[0]
	if line.GetAt() != nil {
		t.Fatal("an unreadable timestamp must be reported as absent, never replaced with " +
			"this Relay's clock")
	}
	if line.GetContent() != "not-a-timestamp and the rest of the line" {
		t.Fatalf("the content must survive intact, got %q", line.GetContent())
	}
}

func TestExecutor_OneEnormousLineIsCappedSoItCannotBeTheWholeRead(t *testing.T) {
	reader := &fakeReader{
		pod:  pod("api", false),
		read: &kube.LogRead{Raw: stamped(strings.Repeat("y", capabilities.MaxLogLineChars*2))},
	}

	result := logsResult(t, newExecutor(reader).Execute(context.Background(),
		job("shop", "checkout-7f", "api", false, 100, 262144)))

	if got := len(result.GetLines()[0].GetContent()); got != capabilities.MaxLogLineChars {
		t.Fatalf("a single line must be capped, got %d chars", got)
	}
}

func TestExecutor_SinceTimeReachesThePort(t *testing.T) {
	reader := &fakeReader{pod: pod("api", false), read: &kube.LogRead{}}
	assignment := job("shop", "checkout-7f", "api", false, 100, 65536)
	assignment.GetArguments().GetKubernetesContainerLogsV1().SinceTime = timestamppb.New(readTime)

	newExecutor(reader).Execute(context.Background(), assignment)

	if !reader.gotRequest.Since.Equal(readTime) {
		t.Fatalf("since_time must bound the read at the source, got %v", reader.gotRequest.Since)
	}
}

func TestExecutor_BudgetExpiryIsTimeoutAndCancellationIsCancellation(t *testing.T) {
	blocked := &fakeReader{pod: pod("api", false), block: make(chan struct{})}
	assignment := job("shop", "checkout-7f", "api", false, 100, 65536)
	assignment.DeadlineBudget = durationpb.New(50 * time.Millisecond)

	if got := newExecutor(blocked).Execute(context.Background(), assignment); got.GetFailure().GetKind() !=
		relayv1.JobFailure_KIND_TIMEOUT {
		t.Fatalf("a budget expiry must be KIND_TIMEOUT, got %+v", got)
	}

	stalled := &fakeReader{pod: pod("api", false), block: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *relayv1.JobResult, 1)
	go func() {
		done <- newExecutor(stalled).Execute(ctx, job("shop", "checkout-7f", "api", false, 100, 65536))
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()

	if got := <-done; got.GetFailure().GetKind() != relayv1.JobFailure_KIND_CANCELLED {
		t.Fatalf("a caller cancellation must be KIND_CANCELLED, got %+v", got)
	}
}

func TestDescriptor_NamesTheCompiledIdentityAndVersion(t *testing.T) {
	descriptor := Descriptor()
	if descriptor.GetCapabilityId() != "kubernetes.container.logs" ||
		descriptor.GetCapabilityVersion() != 1 {
		t.Fatalf("the advertised descriptor is the contract: %+v", descriptor)
	}
}
