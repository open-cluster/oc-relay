package runtime

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// Per-kind workload summary mapping into the versioned public schema. Deployment and
// StatefulSet read desired from spec.replicas (nil coalesces to zero) and the
// remaining counters from status; DaemonSet has no spec.replicas — every counter is
// a status field. The kind literal is the normalized lowercase vocabulary the
// control plane validates against.

// counters carries the per-kind replica counters into the shared summary core.
type counters struct {
	desired            int32
	ready              int32
	updated            int32
	available          int32
	observedGeneration int64
}

func MapDeploymentSummary(deployment *appsv1.Deployment) *relayv1.KubernetesWorkloadSummary {
	return summary("deployment", deployment.ObjectMeta, deployment.Spec.Template,
		deployment.Spec.Selector, counters{
			desired:            int32OrZero(deployment.Spec.Replicas),
			ready:              deployment.Status.ReadyReplicas,
			updated:            deployment.Status.UpdatedReplicas,
			available:          deployment.Status.AvailableReplicas,
			observedGeneration: deployment.Status.ObservedGeneration,
		})
}

func MapStatefulSetSummary(statefulSet *appsv1.StatefulSet) *relayv1.KubernetesWorkloadSummary {
	return summary("statefulset", statefulSet.ObjectMeta, statefulSet.Spec.Template,
		statefulSet.Spec.Selector, counters{
			desired:            int32OrZero(statefulSet.Spec.Replicas),
			ready:              statefulSet.Status.ReadyReplicas,
			updated:            statefulSet.Status.UpdatedReplicas,
			available:          statefulSet.Status.AvailableReplicas,
			observedGeneration: statefulSet.Status.ObservedGeneration,
		})
}

func MapDaemonSetSummary(daemonSet *appsv1.DaemonSet) *relayv1.KubernetesWorkloadSummary {
	return summary("daemonset", daemonSet.ObjectMeta, daemonSet.Spec.Template,
		daemonSet.Spec.Selector, counters{
			desired:            daemonSet.Status.DesiredNumberScheduled,
			ready:              daemonSet.Status.NumberReady,
			updated:            daemonSet.Status.UpdatedNumberScheduled,
			available:          daemonSet.Status.NumberAvailable,
			observedGeneration: daemonSet.Status.ObservedGeneration,
		})
}

func summary(
	kind string,
	meta metav1.ObjectMeta,
	template corev1.PodTemplateSpec,
	selector *metav1.LabelSelector,
	c counters,
) *relayv1.KubernetesWorkloadSummary {
	declared := template.Spec.Containers
	images := make([]string, 0, len(declared))
	resources := make([]*relayv1.KubernetesContainerResources, 0, len(declared))
	for _, container := range declared {
		images = append(images, capImage(container.Image))
		resources = append(resources, &relayv1.KubernetesContainerResources{
			Container:     capIdentifier(container.Name),
			CpuRequest:    quantityOf(container.Resources.Requests, corev1.ResourceCPU),
			CpuLimit:      quantityOf(container.Resources.Limits, corev1.ResourceCPU),
			MemoryRequest: quantityOf(container.Resources.Requests, corev1.ResourceMemory),
			MemoryLimit:   quantityOf(container.Resources.Limits, corev1.ResourceMemory),
		})
	}

	// The complete selector rendering; an unrepresentable selector reports empty here
	// and the read itself refuses enumeration (SELECTOR_UNREPRESENTABLE).
	selectorSummary := ""
	if rendered, ok := RenderSelector(selector); ok {
		selectorSummary = capImage(rendered)
	}

	return &relayv1.KubernetesWorkloadSummary{
		Kind:               kind,
		Name:               capIdentifier(meta.Name),
		Namespace:          capIdentifier(meta.Namespace),
		Uid:                capIdentifier(string(meta.UID)),
		DesiredReplicas:    c.desired,
		ReadyReplicas:      c.ready,
		UpdatedReplicas:    c.updated,
		AvailableReplicas:  c.available,
		Generation:         meta.Generation,
		ObservedGeneration: c.observedGeneration,
		CreatedAt:          mapTime(meta.CreationTimestamp),
		ContainerImages:    images,
		ContainerResources: resources,
		SelectorSummary:    selectorSummary,
	}
}

// quantityOf renders a declared resource quantity, empty when the entry is absent.
func quantityOf(list corev1.ResourceList, name corev1.ResourceName) string {
	quantity, ok := list[name]
	if !ok {
		return ""
	}
	return capQuantity(quantity.String())
}

func int32OrZero(v *int32) int32 {
	if v == nil {
		return 0
	}
	return *v
}
