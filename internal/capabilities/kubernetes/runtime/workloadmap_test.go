package runtime

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// These tests pin the S1B-3 per-kind replica-counter contract. The sharp point:
// Deployment and StatefulSet read desired from spec.replicas (nil coalesces to zero)
// and the ready/updated/available counters from status, while DaemonSet has no
// spec.replicas at all — desired is status.desiredNumberScheduled and ready is
// status.numberReady. Mixing the sources misreports every DaemonSet.

func int32Ptr(v int32) *int32 { return &v }

func TestMapDeploymentSummary_Counters(t *testing.T) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "prod", Generation: 4},
		Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(5)},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas:      3,
			UpdatedReplicas:    4,
			AvailableReplicas:  2,
			ObservedGeneration: 4,
		},
	}
	got := MapDeploymentSummary(deployment)
	if got.GetKind() != "deployment" {
		t.Fatalf("Kind = %q, want deployment", got.GetKind())
	}
	if got.GetDesiredReplicas() != 5 || got.GetReadyReplicas() != 3 ||
		got.GetUpdatedReplicas() != 4 || got.GetAvailableReplicas() != 2 {
		t.Fatalf("counters = %d/%d/%d/%d, want 5/3/4/2", got.GetDesiredReplicas(),
			got.GetReadyReplicas(), got.GetUpdatedReplicas(), got.GetAvailableReplicas())
	}
	if got.GetGeneration() != 4 || got.GetObservedGeneration() != 4 {
		t.Fatalf("generations = %d/%d, want 4/4", got.GetGeneration(), got.GetObservedGeneration())
	}
}

func TestMapDeploymentSummary_NilSpecReplicasIsZero(t *testing.T) {
	got := MapDeploymentSummary(&appsv1.Deployment{})
	if got.GetDesiredReplicas() != 0 {
		t.Fatalf("nil spec.replicas must coalesce to 0, got %d", got.GetDesiredReplicas())
	}
}

func TestMapStatefulSetSummary_Counters(t *testing.T) {
	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "prod"},
		Spec:       appsv1.StatefulSetSpec{Replicas: int32Ptr(3)},
		Status: appsv1.StatefulSetStatus{
			ReadyReplicas:      2,
			UpdatedReplicas:    3,
			AvailableReplicas:  1,
			ObservedGeneration: 7,
		},
	}
	got := MapStatefulSetSummary(statefulSet)
	if got.GetKind() != "statefulset" {
		t.Fatalf("Kind = %q, want statefulset", got.GetKind())
	}
	if got.GetDesiredReplicas() != 3 || got.GetReadyReplicas() != 2 ||
		got.GetUpdatedReplicas() != 3 || got.GetAvailableReplicas() != 1 {
		t.Fatalf("counters = %d/%d/%d/%d, want 3/2/3/1", got.GetDesiredReplicas(),
			got.GetReadyReplicas(), got.GetUpdatedReplicas(), got.GetAvailableReplicas())
	}
	if got.GetObservedGeneration() != 7 {
		t.Fatalf("observed generation = %d, want 7", got.GetObservedGeneration())
	}
}

// The DaemonSet trap: every counter comes from STATUS — desiredNumberScheduled,
// numberReady, updatedNumberScheduled, numberAvailable. There is no spec.replicas.
func TestMapDaemonSetSummary_CountersComeFromStatus(t *testing.T) {
	daemonSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "kube-system"},
		Status: appsv1.DaemonSetStatus{
			DesiredNumberScheduled: 6,
			NumberReady:            5,
			UpdatedNumberScheduled: 4,
			NumberAvailable:        3,
			ObservedGeneration:     2,
		},
	}
	got := MapDaemonSetSummary(daemonSet)
	if got.GetKind() != "daemonset" {
		t.Fatalf("Kind = %q, want daemonset", got.GetKind())
	}
	if got.GetDesiredReplicas() != 6 || got.GetReadyReplicas() != 5 ||
		got.GetUpdatedReplicas() != 4 || got.GetAvailableReplicas() != 3 {
		t.Fatalf("counters = %d/%d/%d/%d, want 6/5/4/3", got.GetDesiredReplicas(),
			got.GetReadyReplicas(), got.GetUpdatedReplicas(), got.GetAvailableReplicas())
	}
	if got.GetObservedGeneration() != 2 {
		t.Fatalf("observed generation = %d, want 2", got.GetObservedGeneration())
	}
}

// Images and resources are DECLARED values from the pod template spec (not from
// running pods), in container order; missing resource entries map to empty strings;
// the selector summary is the complete rendering and is empty exactly when the
// selector is unrepresentable.
func TestMapDeploymentSummary_TemplateAndSelector(t *testing.T) {
	longImage := strings.Repeat("i", 300)
	deployment := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "web"},
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "env", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"dev"}},
				},
			},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "app",
							Image: "registry.example/app:1.2",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("2Gi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("1"),
								},
							},
						},
						{Name: "sidecar", Image: longImage},
					},
				},
			},
		},
	}

	got := MapDeploymentSummary(deployment)
	images := got.GetContainerImages()
	if len(images) != 2 || images[0] != "registry.example/app:1.2" || len(images[1]) != 256 {
		t.Fatalf("images = %v (len[1]=%d), want declared order with 256-cap", images, len(images[1]))
	}

	resources := got.GetContainerResources()
	if len(resources) != 2 {
		t.Fatalf("expected 2 resource entries, got %d", len(resources))
	}
	app := resources[0]
	if app.GetContainer() != "app" || app.GetCpuRequest() != "500m" ||
		app.GetMemoryRequest() != "2Gi" || app.GetCpuLimit() != "1" {
		t.Fatalf("app resources = %q cpuReq=%q memReq=%q cpuLim=%q",
			app.GetContainer(), app.GetCpuRequest(), app.GetMemoryRequest(), app.GetCpuLimit())
	}
	if app.GetMemoryLimit() != "" {
		t.Fatalf("missing memory limit must be empty, got %q", app.GetMemoryLimit())
	}
	sidecar := resources[1]
	if sidecar.GetCpuRequest() != "" || sidecar.GetCpuLimit() != "" ||
		sidecar.GetMemoryRequest() != "" || sidecar.GetMemoryLimit() != "" {
		t.Fatalf("resourceless container must map to all-empty quantities")
	}

	if got.GetSelectorSummary() != "app=web,env notin (dev)" {
		t.Fatalf("selector summary = %q, want complete rendering", got.GetSelectorSummary())
	}
}

func TestMapDeploymentSummary_UnrepresentableSelectorIsEmpty(t *testing.T) {
	deployment := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
		Selector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "app", Operator: metav1.LabelSelectorOperator("Superset")},
			},
		},
	}}
	if got := MapDeploymentSummary(deployment).GetSelectorSummary(); got != "" {
		t.Fatalf("unrepresentable selector summary must be empty, got %q", got)
	}
}

func TestCapQuantity_Bounds(t *testing.T) {
	if got := capQuantity(strings.Repeat("9", 64)); len(got) != 32 {
		t.Fatalf("quantity must cap at 32, got len %d", len(got))
	}
	if got := capQuantity("500m"); got != "500m" {
		t.Fatalf("under-cap quantity must pass through, got %q", got)
	}
}

// The caps must hold ON the mapping path, not just as standalone functions: an over-cap
// selector rendering and an over-cap quantity string both leave the summary bounded.
func TestMapDeploymentSummary_CapsHoldOnTheMappingPath(t *testing.T) {
	hugeQuantity := resource.MustParse("123456789012345678901234567890123456789")
	deployment := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
		Selector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{
				{
					Key:      "app",
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{strings.Repeat("v", 300)},
				},
			},
		},
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: hugeQuantity},
				},
			}},
		}},
	}}

	got := MapDeploymentSummary(deployment)
	if len(got.GetSelectorSummary()) != 256 {
		t.Fatalf("over-cap selector summary must cap at 256, got len %d",
			len(got.GetSelectorSummary()))
	}
	if quantity := got.GetContainerResources()[0].GetCpuRequest(); len(quantity) != 32 {
		t.Fatalf("over-cap quantity must cap at 32 on the mapping path, got len %d (%q)",
			len(quantity), quantity)
	}
}
