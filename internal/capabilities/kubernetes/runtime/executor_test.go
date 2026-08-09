package runtime

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/open-cluster/oc-relay/internal/kube"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// fakeReader scripts the read-only port: workloads and a pod page per kind, or an
// injected outcome error.
type fakeReader struct {
	deployment  *appsv1.Deployment
	statefulSet *appsv1.StatefulSet
	daemonSet   *appsv1.DaemonSet
	page        *kube.PodPage
	getErr      error
	listErr     error

	gotSelector string
	gotLimit    int64
	block       chan struct{} // if non-nil, GET blocks until closed or ctx cancels
}

func (f *fakeReader) GetDeployment(ctx context.Context, _, _ string) (*appsv1.Deployment, error) {
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	return f.deployment, f.getErr
}

func (f *fakeReader) GetStatefulSet(ctx context.Context, _, _ string) (*appsv1.StatefulSet, error) {
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	return f.statefulSet, f.getErr
}

func (f *fakeReader) GetDaemonSet(ctx context.Context, _, _ string) (*appsv1.DaemonSet, error) {
	if err := f.wait(ctx); err != nil {
		return nil, err
	}
	return f.daemonSet, f.getErr
}

func (f *fakeReader) ListPods(_ context.Context, _, selector string, limit int64) (*kube.PodPage, error) {
	f.gotSelector = selector
	f.gotLimit = limit
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.page, nil
}

func (f *fakeReader) wait(ctx context.Context) error {
	if f.block == nil {
		return nil
	}
	select {
	case <-f.block:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func labelSelector(key, value string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{key: value}}
}

func deploymentJob(kind relayv1.WorkloadKind, namespace, name string, maxPods uint32) *relayv1.JobAssignment {
	return &relayv1.JobAssignment{
		JobId: "job-1", LeaseEpoch: 1,
		DeadlineBudget: durationpb.New(30 * time.Second),
		Arguments: &relayv1.CapabilityArguments{
			Arguments: &relayv1.CapabilityArguments_KubernetesWorkloadRuntimeV1{
				KubernetesWorkloadRuntimeV1: &relayv1.KubernetesWorkloadRuntimeArgsV1{
					Namespace: namespace, WorkloadKind: kind, WorkloadName: name, MaxPods: maxPods,
				},
			},
		},
	}
}

func kubeResult(t *testing.T, r *relayv1.JobResult) *relayv1.KubernetesWorkloadRuntimeResultV1 {
	t.Helper()
	res := r.GetResult().GetKubernetesWorkloadRuntimeV1()
	if res == nil {
		t.Fatalf("expected a kubernetes.workload.runtime result, got %+v", r)
	}
	return res
}

func TestExecutor_SuccessMapsWorkloadAndPodsWithCompletenessBasis(t *testing.T) {
	reader := &fakeReader{
		deployment: &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default", UID: "uid-1"},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr[int32](3),
				Selector: labelSelector("app", "api"),
			},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 2},
		},
		page: &kube.PodPage{
			Pods:      []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "api-abc"}}},
			Truncated: false,
		},
	}
	exec := NewExecutor(reader, Options{LocalMaxPods: 50})

	result := kubeResult(t, exec.Execute(context.Background(),
		deploymentJob(relayv1.WorkloadKind_WORKLOAD_KIND_DEPLOYMENT, "default", "api", 10)))

	if result.GetOutcome() != relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_SUCCESS {
		t.Fatalf("expected SUCCESS, got %v", result.GetOutcome())
	}
	if result.GetWorkload().GetKind() != "deployment" || result.GetWorkload().GetDesiredReplicas() != 3 {
		t.Fatalf("workload summary wrong: %+v", result.GetWorkload())
	}
	if result.GetReturnedPodCount() != 1 || !result.GetComplete() {
		t.Fatalf("expected 1 pod and complete, got count=%d complete=%v",
			result.GetReturnedPodCount(), result.GetComplete())
	}
	if reader.gotSelector != "app=api" {
		t.Fatalf("pod list must use the rendered selector, got %q", reader.gotSelector)
	}
	if result.GetReadAt() == nil {
		t.Fatal("read_at must be stamped")
	}
}

func TestExecutor_TruncatedPageIsNotComplete(t *testing.T) {
	reader := &fakeReader{
		deployment: &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
			Spec:       appsv1.DeploymentSpec{Selector: labelSelector("app", "api")},
		},
		page: &kube.PodPage{Pods: []corev1.Pod{{}}, Truncated: true},
	}
	exec := NewExecutor(reader, Options{LocalMaxPods: 50})

	result := kubeResult(t, exec.Execute(context.Background(),
		deploymentJob(relayv1.WorkloadKind_WORKLOAD_KIND_DEPLOYMENT, "default", "api", 10)))

	if result.GetComplete() {
		t.Fatal("a page with a continuation token must not be complete")
	}
}

func TestExecutor_EffectiveMaxPodsClampsToLocalCapAndBounds(t *testing.T) {
	reader := &fakeReader{
		deployment: &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
			Spec:       appsv1.DeploymentSpec{Selector: labelSelector("app", "api")},
		},
		page: &kube.PodPage{},
	}
	// Local cap 8 is below the dispatched 40: the effective bound (and applied_max_pods)
	// must be the local cap, and the list must be called with it.
	exec := NewExecutor(reader, Options{LocalMaxPods: 8})

	result := kubeResult(t, exec.Execute(context.Background(),
		deploymentJob(relayv1.WorkloadKind_WORKLOAD_KIND_DEPLOYMENT, "default", "api", 40)))

	if result.GetAppliedMaxPods() != 8 {
		t.Fatalf("applied_max_pods must cite the effective local bound, got %d", result.GetAppliedMaxPods())
	}
	if reader.gotLimit != 8 {
		t.Fatalf("the pod list must be bounded by the effective limit, got %d", reader.gotLimit)
	}
}

func TestExecutor_UnknownKindIsRejectedNeverFallsThroughToDaemonSet(t *testing.T) {
	reader := &fakeReader{daemonSet: &appsv1.DaemonSet{}}
	exec := NewExecutor(reader, Options{LocalMaxPods: 50})

	result := exec.Execute(context.Background(),
		deploymentJob(relayv1.WorkloadKind_WORKLOAD_KIND_UNSPECIFIED, "default", "api", 10))

	if result.GetFailure().GetKind() != relayv1.JobFailure_KIND_ARGUMENTS_REJECTED {
		t.Fatalf("an unspecified kind must be rejected, got %+v", result)
	}
}

func TestExecutor_InvalidNamespaceOrNameRejected(t *testing.T) {
	exec := NewExecutor(&fakeReader{}, Options{LocalMaxPods: 50})
	for _, tc := range []struct{ ns, name string }{
		{"Default", "api"}, // uppercase namespace
		{"default", "Api"}, // uppercase name
		{"", "api"},        // empty namespace
		{"default", ""},    // empty name
	} {
		result := exec.Execute(context.Background(),
			deploymentJob(relayv1.WorkloadKind_WORKLOAD_KIND_DEPLOYMENT, tc.ns, tc.name, 10))
		if result.GetFailure().GetKind() != relayv1.JobFailure_KIND_ARGUMENTS_REJECTED {
			t.Fatalf("ns=%q name=%q must be rejected, got %+v", tc.ns, tc.name, result)
		}
	}
}

func TestExecutor_UnrepresentableSelectorRefusesEnumeration(t *testing.T) {
	reader := &fakeReader{
		deployment: &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
			// matchExpressions with an unknown operator → unrepresentable.
			Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: "Bogus"},
				},
			}},
		},
		page: &kube.PodPage{Pods: []corev1.Pod{{}, {}}},
	}
	exec := NewExecutor(reader, Options{LocalMaxPods: 50})

	result := kubeResult(t, exec.Execute(context.Background(),
		deploymentJob(relayv1.WorkloadKind_WORKLOAD_KIND_DEPLOYMENT, "default", "api", 10)))

	if result.GetOutcome() != relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_SELECTOR_UNREPRESENTABLE {
		t.Fatalf("expected SELECTOR_UNREPRESENTABLE, got %v", result.GetOutcome())
	}
	if reader.gotSelector != "" {
		t.Fatal("an unrepresentable selector must refuse the pod list, not list the namespace")
	}
	if result.GetReturnedPodCount() != 0 {
		t.Fatal("no pods may be returned for an unrepresentable selector")
	}
}

func TestExecutor_ReadOutcomeTaxonomyFromPortErrors(t *testing.T) {
	for outcome, want := range map[kube.ReadOutcome]relayv1.KubernetesReadOutcome{
		kube.OutcomeWorkloadNotFound: relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_WORKLOAD_NOT_FOUND,
		kube.OutcomeUnauthorized:     relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_UNAUTHORIZED,
		kube.OutcomeUnreachable:      relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_UNREACHABLE,
	} {
		reader := &fakeReader{getErr: &kube.ReadError{Outcome: outcome}}
		exec := NewExecutor(reader, Options{LocalMaxPods: 50})
		result := kubeResult(t, exec.Execute(context.Background(),
			deploymentJob(relayv1.WorkloadKind_WORKLOAD_KIND_DEPLOYMENT, "default", "api", 10)))
		if result.GetOutcome() != want {
			t.Fatalf("outcome %v must map to %v, got %v", outcome, want, result.GetOutcome())
		}
	}
}

func TestExecutor_DeadlineBudgetExpiryIsTimeout(t *testing.T) {
	reader := &fakeReader{block: make(chan struct{})} // GET blocks forever
	exec := NewExecutor(reader, Options{LocalMaxPods: 50})
	job := deploymentJob(relayv1.WorkloadKind_WORKLOAD_KIND_DEPLOYMENT, "default", "api", 10)
	job.DeadlineBudget = durationpb.New(50 * time.Millisecond)

	result := exec.Execute(context.Background(), job)

	if result.GetFailure().GetKind() != relayv1.JobFailure_KIND_TIMEOUT {
		t.Fatalf("a budget expiry must be KIND_TIMEOUT, got %+v", result)
	}
}

func TestExecutor_CallerCancellationIsCancellation(t *testing.T) {
	reader := &fakeReader{block: make(chan struct{})}
	exec := NewExecutor(reader, Options{LocalMaxPods: 50})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan *relayv1.JobResult, 1)
	go func() {
		done <- exec.Execute(ctx, deploymentJob(relayv1.WorkloadKind_WORKLOAD_KIND_DEPLOYMENT, "default", "api", 10))
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()

	result := <-done
	if result.GetFailure().GetKind() != relayv1.JobFailure_KIND_CANCELLED {
		t.Fatalf("a caller cancellation must be KIND_CANCELLED, got %+v", result)
	}
}

func ptr[T any](v T) *T { return &v }
