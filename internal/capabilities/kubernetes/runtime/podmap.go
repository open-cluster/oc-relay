package runtime

import (
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	relayv1 "github.com/OCluster/opencluster-relay/gen/go/opencluster/relay/v1"
)

// MapPod projects a pod's runtime state into the frozen kubernetes.runtime.v1 wire
// shape, matching the S1B-3 contract. This file is the single mapping boundary from
// Kubernetes API objects to the capability result; nothing else translates pod state.
// The pod must be non-nil (callers hold an API-read object); all sub-structs are value
// types, so a zero pod maps safely.
func MapPod(pod *corev1.Pod) *relayv1.KubernetesPodRuntime {
	containers := make([]*relayv1.KubernetesContainerRuntime, 0, len(pod.Status.ContainerStatuses))
	for _, status := range pod.Status.ContainerStatuses {
		containers = append(containers, mapContainer(status))
	}
	// Go's API types carry no absent-vs-empty distinction (phase is a plain string), so
	// an explicitly empty value and a missing one both default to "Unknown". Recorded
	// deviation: the central implementation defaults only a null phase and would pass a
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
		Name:              capIdentifier(pod.Name),
		Phase:             capReason(phase),
		NodeName:          capIdentifier(pod.Spec.NodeName),
		StartedAt:         startedAt,
		Ready:             podReady(pod.Status.Conditions),
		UnscheduledReason: capReason(unscheduledReason(pod.Status.Conditions)),
		Containers:        containers,
	}
}

// unscheduledReason reports the reason of a PodScheduled condition explicitly at
// "False" — empty for a scheduled (or not-yet-reported) pod, matching the oracle.
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
		Name:            capIdentifier(status.Name),
		Image:           capImage(status.Image),
		Ready:           status.Ready,
		RestartCount:    status.RestartCount,
		State:           mapState(status.State),
		LastTermination: mapTermination(status.LastTerminationState.Terminated),
	}
}

// mapState mirrors the oracle's precedence exactly: Running > Terminated > Waiting,
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
		waitingReason = capReason(state.Waiting.Reason)
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
		// The oracle reports "Unknown" for a reasonless termination; the literal reason
		// string is the central OOM-detection key, so the default must match exactly.
		reason = "Unknown"
	}
	return &relayv1.KubernetesContainerTermination{
		Reason:     capReason(reason),
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
// readiness is never inferred. The first Ready condition wins, matching the oracle.
func podReady(conditions []corev1.PodCondition) bool {
	for _, condition := range conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
