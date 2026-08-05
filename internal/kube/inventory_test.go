package kube

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testDeployment() *appsv1.Deployment {
	replicas := int32(3)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "shop", Name: "api", UID: "uid-1", Generation: 7,
			Annotations: map[string]string{"deployment.kubernetes.io/revision": "12"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{{Name: "migrate", Image: "registry/migrate:v1"}},
					Containers: []corev1.Container{{
						Name:  "app",
						Image: "registry/app:v1",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
						Env: []corev1.EnvVar{{
							Name: "DB_PASSWORD",
							ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: "db-credentials"},
								Key:                  "password",
							}},
						}},
						EnvFrom: []corev1.EnvFromSource{{
							ConfigMapRef: &corev1.ConfigMapEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: "app-config"},
							},
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "settings",
						VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: "settings"},
						}},
					}},
					ImagePullSecrets: []corev1.LocalObjectReference{{Name: "registry-pull"}},
				},
			},
		},
	}
}

func TestInventory_ADeploymentReducesToDeclaredIntentOnly(t *testing.T) {
	intent := deploymentIntent(testDeployment())

	if intent.Kind != "deployment" || intent.Namespace != "shop" || intent.UID != "uid-1" {
		t.Fatalf("identity must survive the reduction, got %+v", intent)
	}
	if intent.Generation != 7 || intent.ControllerRevision != "12" || intent.Replicas != "3" {
		t.Fatalf("the declared revision and count must survive, got %+v", intent)
	}
	if len(intent.Containers) != 2 {
		t.Fatalf("init and app containers are both watched, got %d", len(intent.Containers))
	}
	if !intent.Containers[0].Init || intent.Containers[0].Image != "registry/migrate:v1" {
		t.Fatalf("the init container must be marked as such, got %+v", intent.Containers[0])
	}
	if intent.Containers[1].LimitsMemory != "256Mi" || intent.Containers[1].RequestsCPU != "" {
		t.Fatalf("declared resources travel, undeclared ones stay empty, got %+v", intent.Containers[1])
	}
}

func TestInventory_EveryReferenceSyntaxLandsInTheRollups(t *testing.T) {
	intent := deploymentIntent(testDeployment())

	if len(intent.ConfigMapRefs) != 2 ||
		intent.ConfigMapRefs[0] != "app-config" || intent.ConfigMapRefs[1] != "settings" {
		t.Fatalf("envFrom and volume ConfigMaps must both be referenced, sorted, got %v",
			intent.ConfigMapRefs)
	}
	if len(intent.SecretRefs) != 2 ||
		intent.SecretRefs[0] != "db-credentials" || intent.SecretRefs[1] != "registry-pull" {
		t.Fatalf("env and image-pull Secrets must both be referenced, sorted, got %v",
			intent.SecretRefs)
	}
}

func TestInventory_TheTemplateHashIsStableForAnUnchangedTemplateAndMovesWithIt(t *testing.T) {
	first := deploymentIntent(testDeployment())
	second := deploymentIntent(testDeployment())
	if first.TemplateHash != second.TemplateHash {
		t.Fatal("the same template must hash the same, or every tick invents a change")
	}

	changed := testDeployment()
	changed.Spec.Template.Spec.Containers[0].Command = []string{"serve", "--verbose"}
	moved := deploymentIntent(changed)
	if moved.TemplateHash == first.TemplateHash {
		t.Fatal("a template change outside the itemized fields must still move the hash")
	}
}

func TestInventory_ADaemonSetDeclaresNoReplicas(t *testing.T) {
	intent := daemonSetIntent(&appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "proxy", UID: "uid-2"},
	})
	if intent.Kind != "daemonset" || intent.Replicas != "" {
		t.Fatalf("a DaemonSet has no declared count and must not pretend to, got %+v", intent)
	}
}
