package runtime

import (
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/open-cluster/oc-relay/internal/capabilities"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// MapPod converts a Kubernetes Pod into the versioned runtime result.
// It is the single mapping boundary between Kubernetes API objects and the
// public capability schema. Missing optional Kubernetes fields are represented
// by their documented zero values.
func MapPod(pod *corev1.Pod) *relayv1.KubernetesPodRuntime {
	containers := make([]*relayv1.KubernetesContainerRuntime, 0, len(pod.Status.ContainerStatuses))
	for _, status := range pod.Status.ContainerStatuses {
		containers = append(containers, mapContainer(status))
	}
	// Go's API types carry no absent-vs-empty distinction (phase is a plain string), so
	// an explicitly empty value and a missing one both default to "Unknown". Recorded
	// deviation: the control-plane reference behavior defaults only a null phase and would pass a
	// hostile explicit "" through; every real pod behaves identically on both sides.
	// The same applies to the termination reason default below.
	phase := string(pod.Status.Phase)
	if phase == "" {
		phase = "Unknown"
	}
	startedAt := (*timestamppb.Timestamp)(nil)
	if pod.Status.StartTime != nil {
		startedAt = mapTime(*pod.Status.StartTime)
	}
	return &relayv1.KubernetesPodRuntime{
		Name:              capabilities.CapIdentifier(pod.Name),
		Phase:             capabilities.CapReason(phase),
		NodeName:          capabilities.CapIdentifier(pod.Spec.NodeName),
		StartedAt:         startedAt,
		Ready:             podReady(pod.Status.Conditions),
		UnscheduledReason: capabilities.CapReason(unscheduledReason(pod.Status.Conditions)),
		Containers:        containers,
	}
}

// unscheduledReason reports the reason of a PodScheduled condition explicitly at
// "False" — empty for a scheduled (or not-yet-reported) pod.
func unscheduledReason(conditions []corev1.PodCondition) string {
	for _, condition := range conditions {
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
			return condition.Reason
		}
	}
	return ""
}

func mapContainer(status corev1.ContainerStatus) *relayv1.KubernetesContainerRuntime {
	return &relayv1.KubernetesContainerRuntime{
		Name:            capabilities.CapIdentifier(status.Name),
		Image:           capabilities.CapImage(status.Image),
		Ready:           status.Ready,
		RestartCount:    status.RestartCount,
		State:           mapState(status.State),
		LastTermination: mapTermination(status.LastTerminationState.Terminated),
	}
}

// mapState maps container state with precedence Running > Terminated > Waiting,
// with a fully empty state reported as Waiting with no reason.
func mapState(state corev1.ContainerState) *relayv1.KubernetesContainerState {
	if state.Running != nil {
		return &relayv1.KubernetesContainerState{
			Kind:         relayv1.KubernetesContainerState_KIND_RUNNING,
			RunningSince: mapTime(state.Running.StartedAt),
		}
	}
	if state.Terminated != nil {
		return &relayv1.KubernetesContainerState{
			Kind:        relayv1.KubernetesContainerState_KIND_TERMINATED,
			Termination: mapTermination(state.Terminated),
		}
	}
	waitingReason := ""
	if state.Waiting != nil {
		waitingReason = capabilities.CapReason(state.Waiting.Reason)
	}
	return &relayv1.KubernetesContainerState{
		Kind:          relayv1.KubernetesContainerState_KIND_WAITING,
		WaitingReason: waitingReason,
	}
}

func mapTermination(terminated *corev1.ContainerStateTerminated) *relayv1.KubernetesContainerTermination {
	if terminated == nil {
		return nil
	}
	reason := terminated.Reason
	if reason == "" {
		// A reasonless termination reports "Unknown"; the literal reason
		// string is the OOM-detection key consumed centrally, so the default must be stable.
		reason = "Unknown"
	}
	return &relayv1.KubernetesContainerTermination{
		Reason:     capabilities.CapReason(reason),
		ExitCode:   terminated.ExitCode,
		FinishedAt: mapTime(terminated.FinishedAt),
	}
}

// mapTime maps a source-observed timestamp; the zero value is absent, never epoch.
func mapTime(t metav1.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t.Time)
}

// podReady is true only when a Ready condition explicitly reports "True". A missing
// condition list, a missing Ready condition, or any other status value is NOT ready —
// readiness is never inferred. The first Ready condition in source order wins.
func podReady(conditions []corev1.PodCondition) bool {
	for _, condition := range conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
