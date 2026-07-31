package runtime

import (
	"context"
	"errors"

	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/open-cluster/oc-relay/internal/capabilities"
	"github.com/open-cluster/oc-relay/internal/kube"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// Pod-list bounds: one bounded page, the customer's local caps may lower the effective
// value but never raise it above MaxWorkloadPods, and never below MinWorkloadPods.
const (
	MinWorkloadPods = 5
	MaxWorkloadPods = 50
)

// Executor runs the kubernetes.workload.runtime.v1 capability through the read-only kube
// port, holding the capability's semantic contract: a closed workload vocabulary
// validated before any cluster contact, complete selector rendering that REFUSES
// enumeration rather than listing a namespace, a single bounded pod page whose
// continuation-free flag is the completeness basis, the closed read-outcome taxonomy, and
// a deadline budget measured from receipt — a budget expiry is a timeout, a caller
// cancellation is a cancellation, and the two are never confused.
type Executor struct {
	reader  kube.WorkloadReader
	options Options
}

// Options is the executor's customer-authored configuration. Every field may narrow a read
// and none may widen one.
type Options struct {
	// Policy may refuse a namespace outright.
	Policy capabilities.LocalPolicy
	// LocalMaxPods lowers the pod bound. Zero means the operator set none.
	LocalMaxPods int64
}

// NewExecutor builds the capability executor.
func NewExecutor(reader kube.WorkloadReader, options Options) *Executor {
	return &Executor{reader: reader, options: options}
}

// Execute runs one workload-runtime job to a typed result. The deadline budget and the
// timeout-versus-cancellation distinction are the shared capability discipline; what is
// specific to this capability is everything below read.
func (e *Executor) Execute(ctx context.Context, job *relayv1.JobAssignment) *relayv1.JobResult {
	args := job.GetArguments().GetKubernetesWorkloadRuntimeV1()
	if !validArgs(args) {
		return capabilities.Failure(job, relayv1.JobFailure_KIND_ARGUMENTS_REJECTED)
	}
	// A namespace the operator excluded is excluded from EVERY read. A guarantee that held
	// for two capabilities and not the third would be worse than none, because an operator
	// would believe it held for all.
	if !e.options.Policy.AllowsNamespace(args.GetNamespace()) {
		return capabilities.Failure(job, relayv1.JobFailure_KIND_LOCAL_POLICY_REFUSED)
	}

	return capabilities.Run(ctx, job, func(execCtx context.Context) capabilities.Reading {
		result := e.read(execCtx, args)
		return capabilities.Reading{
			Result: &relayv1.CapabilityResult{
				Result: &relayv1.CapabilityResult_KubernetesWorkloadRuntimeV1{
					KubernetesWorkloadRuntimeV1: result,
				},
			},
			Reached: result.GetOutcome() == relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_SUCCESS,
		}
	})
}

// read performs the workload GET, selector rendering, and bounded pod LIST, assembling the
// capability result under the closed outcome taxonomy.
func (e *Executor) read(ctx context.Context,
	args *relayv1.KubernetesWorkloadRuntimeArgsV1) *relayv1.KubernetesWorkloadRuntimeResultV1 {
	appliedMax := e.effectiveMaxPods(args.GetMaxPods())

	workload, selector, outcome := e.fetchWorkload(ctx, args)
	if outcome != kube.OutcomeSuccess {
		return &relayv1.KubernetesWorkloadRuntimeResultV1{
			Outcome:        wireOutcome(outcome),
			AppliedMaxPods: uint32(appliedMax),
		}
	}

	// An unrepresentable selector REFUSES enumeration — it never degrades to an empty
	// filter that would list the whole namespace.
	rendered, ok := RenderSelector(selector)
	if !ok {
		return &relayv1.KubernetesWorkloadRuntimeResultV1{
			Outcome:        relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_SELECTOR_UNREPRESENTABLE,
			Workload:       workload,
			AppliedMaxPods: uint32(appliedMax),
		}
	}

	page, err := e.reader.ListPods(ctx, args.GetNamespace(), rendered, appliedMax)
	if err != nil {
		return &relayv1.KubernetesWorkloadRuntimeResultV1{
			Outcome:        wireOutcome(outcomeOf(err)),
			Workload:       workload,
			AppliedMaxPods: uint32(appliedMax),
		}
	}

	pods := make([]*relayv1.KubernetesPodRuntime, 0, len(page.Pods))
	for i := range page.Pods {
		pods = append(pods, MapPod(&page.Pods[i]))
	}

	return &relayv1.KubernetesWorkloadRuntimeResultV1{
		Outcome:          relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_SUCCESS,
		Workload:         workload,
		Pods:             pods,
		ReturnedPodCount: int32(len(pods)),
		// The completeness basis the central certificate consumes: a single page with no
		// continuation token is a complete, list-without-continuation read.
		Complete:       !page.Truncated,
		ReadAt:         timestamppb.Now(),
		AppliedMaxPods: uint32(appliedMax),
	}
}

// fetchWorkload dispatches by the closed workload kind and maps the summary. An unknown
// kind is rejected upstream (validArgs), so it never falls through to a default branch.
func (e *Executor) fetchWorkload(ctx context.Context, args *relayv1.KubernetesWorkloadRuntimeArgsV1,
) (*relayv1.KubernetesWorkloadSummary, *metav1.LabelSelector, kube.ReadOutcome) {
	namespace, name := args.GetNamespace(), args.GetWorkloadName()
	switch args.GetWorkloadKind() {
	case relayv1.WorkloadKind_WORKLOAD_KIND_DEPLOYMENT:
		workload, err := e.reader.GetDeployment(ctx, namespace, name)
		if err != nil {
			return nil, nil, outcomeOf(err)
		}
		return MapDeploymentSummary(workload), workload.Spec.Selector, kube.OutcomeSuccess
	case relayv1.WorkloadKind_WORKLOAD_KIND_STATEFULSET:
		workload, err := e.reader.GetStatefulSet(ctx, namespace, name)
		if err != nil {
			return nil, nil, outcomeOf(err)
		}
		return MapStatefulSetSummary(workload), workload.Spec.Selector, kube.OutcomeSuccess
	case relayv1.WorkloadKind_WORKLOAD_KIND_DAEMONSET:
		workload, err := e.reader.GetDaemonSet(ctx, namespace, name)
		if err != nil {
			return nil, nil, outcomeOf(err)
		}
		return MapDaemonSetSummary(workload), workload.Spec.Selector, kube.OutcomeSuccess
	default:
		return nil, nil, kube.OutcomeUnreachable // unreachable: validArgs rejected it
	}
}

func (e *Executor) effectiveMaxPods(dispatched uint32) int64 {
	return capabilities.Lower(dispatched, e.options.LocalMaxPods, MinWorkloadPods, MaxWorkloadPods)
}

func validArgs(args *relayv1.KubernetesWorkloadRuntimeArgsV1) bool {
	if args == nil {
		return false
	}
	if args.GetWorkloadKind() == relayv1.WorkloadKind_WORKLOAD_KIND_UNSPECIFIED {
		return false
	}
	return capabilities.ValidNamespace(args.GetNamespace()) &&
		capabilities.ValidObjectName(args.GetWorkloadName())
}

func outcomeOf(err error) kube.ReadOutcome {
	var readErr *kube.ReadError
	if errors.As(err, &readErr) {
		return readErr.Outcome
	}
	return kube.OutcomeUnreachable
}

func wireOutcome(outcome kube.ReadOutcome) relayv1.KubernetesReadOutcome {
	switch outcome {
	case kube.OutcomeSuccess:
		return relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_SUCCESS
	case kube.OutcomeUnauthorized:
		return relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_UNAUTHORIZED
	case kube.OutcomeWorkloadNotFound:
		return relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_WORKLOAD_NOT_FOUND
	case kube.OutcomeNamespaceNotFound:
		return relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_NAMESPACE_NOT_FOUND
	case kube.OutcomeSelectorUnrepresentable:
		return relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_SELECTOR_UNREPRESENTABLE
	default:
		return relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_UNREACHABLE
	}
}
