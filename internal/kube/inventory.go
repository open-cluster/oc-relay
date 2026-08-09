package kube

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	appsv1client "k8s.io/client-go/kubernetes/typed/apps/v1"
	"k8s.io/client-go/metadata"
)

// ContainerIntent is one container's declared intent: what the operator asked to run and
// with what resources. Nothing here can hold a runtime value.
type ContainerIntent struct {
	Name string
	// Init distinguishes an init container from an ordinary one; both are watched
	// because an init image change breaks startup exactly as an app image change does.
	Init           bool
	Image          string
	RequestsCPU    string
	RequestsMemory string
	LimitsCPU      string
	LimitsMemory   string
}

// WorkloadIntent is a workload reduced to its declared intent — the watched field set of
// inventory synchronization. It is built HERE, in the one package that may touch typed
// Kubernetes objects, so that no status field can survive into it: what the type cannot
// hold, no later widening of a diff can leak.
type WorkloadIntent struct {
	// Kind is the lowercase workload kind: "deployment", "statefulset", "daemonset" —
	// the same vocabulary the control plane's investigation scope speaks.
	Kind      string
	Namespace string
	Name      string
	UID       string
	// Generation is metadata.generation: the declared-intent revision, moved by spec
	// changes and never by status movement.
	Generation int64
	// ControllerRevision is the deployment.kubernetes.io/revision annotation where the
	// controller maintains one; empty otherwise. The StatefulSet and DaemonSet
	// controller revisions live in status and are deliberately not read.
	ControllerRevision string
	// Replicas is the DECLARED count as a string, empty when the spec does not declare
	// one (a DaemonSet, or an unset field). What is currently ready is state and has no
	// representation here.
	Replicas   string
	Containers []ContainerIntent
	// ConfigMapRefs and SecretRefs are the referenced names, sorted and deduplicated:
	// env sources, envFrom, volumes, projected volume sources and image pull secrets.
	// Names only — a reference is identity, its target's content is not watched.
	ConfigMapRefs []string
	SecretRefs    []string
	// TemplateHash is a digest of the whole pod template spec, so a template change
	// outside the itemized fields above is still visible as a change. Deterministic
	// within one Relay process; a process restart re-baselines, so serialization drift
	// across upgrades can never masquerade as a change.
	TemplateHash string
}

// InventoryPage is a bounded workload listing. Truncated reports that any underlying
// list returned a continuation token, which means changes in the unseen remainder are
// invisible until a complete tick — the caller reports that rather than guessing.
type InventoryPage struct {
	Workloads []WorkloadIntent
	Truncated bool
}

// ConfigVersion is a ConfigMap's or Secret's identity and resource version — and
// deliberately nothing else. It is produced from metadata-only reads, so a Secret's
// value never enters Relay memory on this path, let alone leaves it.
type ConfigVersion struct {
	Namespace       string
	Name            string
	UID             string
	ResourceVersion string
}

// ConfigVersionPage is a bounded metadata listing of ConfigMaps or Secrets.
type ConfigVersionPage struct {
	Versions  []ConfigVersion
	Truncated bool
}

var (
	configMapResource = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	secretResource    = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
)

// InventoryLister is the client-go implementation of the inventory read: three bounded
// workload lists reduced to declared intent, and metadata-only ConfigMap and Secret
// lists. It is a separate type from Reader because it needs the metadata client — the
// mechanism by which Secret content stays out of this process — and the capability
// reader must not grow one it has no use for.
type InventoryLister struct {
	apps appsv1client.AppsV1Interface
	meta metadata.Interface
}

// NewInventoryLister wraps the typed apps/v1 read surface and a metadata-only client.
func NewInventoryLister(clientset kubernetes.Interface, meta metadata.Interface) *InventoryLister {
	return &InventoryLister{apps: clientset.AppsV1(), meta: meta}
}

// ListWorkloadIntents lists Deployments, StatefulSets and DaemonSets in namespace (""
// for every namespace), each as one bounded page, reduced to declared intent. A
// non-positive limit is refused rather than issued as an unbounded read.
func (l *InventoryLister) ListWorkloadIntents(
	ctx context.Context, namespace string, limit int64,
) (*InventoryPage, error) {
	if limit < 1 {
		return nil, fmt.Errorf(
			"workload inventory refused: non-positive limit %d would be an unbounded read", limit)
	}
	options := metav1.ListOptions{Limit: limit}
	page := &InventoryPage{}

	deployments, err := l.apps.Deployments(namespace).List(ctx, options)
	if err != nil {
		return nil, mapReadError(err)
	}
	page.Truncated = page.Truncated || deployments.Continue != ""
	for i := range deployments.Items {
		page.Workloads = append(page.Workloads, deploymentIntent(&deployments.Items[i]))
	}

	statefulSets, err := l.apps.StatefulSets(namespace).List(ctx, options)
	if err != nil {
		return nil, mapReadError(err)
	}
	page.Truncated = page.Truncated || statefulSets.Continue != ""
	for i := range statefulSets.Items {
		page.Workloads = append(page.Workloads, statefulSetIntent(&statefulSets.Items[i]))
	}

	daemonSets, err := l.apps.DaemonSets(namespace).List(ctx, options)
	if err != nil {
		return nil, mapReadError(err)
	}
	page.Truncated = page.Truncated || daemonSets.Continue != ""
	for i := range daemonSets.Items {
		page.Workloads = append(page.Workloads, daemonSetIntent(&daemonSets.Items[i]))
	}
	return page, nil
}

// ListConfigMapVersions lists ConfigMap identities and resource versions in namespace,
// one bounded metadata-only page.
func (l *InventoryLister) ListConfigMapVersions(
	ctx context.Context, namespace string, limit int64,
) (*ConfigVersionPage, error) {
	return l.listVersions(ctx, configMapResource, namespace, limit)
}

// ListSecretVersions lists Secret identities and resource versions in namespace, one
// bounded metadata-only page. The metadata client cannot return a Secret's data even if
// asked, which is what makes "content never leaves" structural on this path.
func (l *InventoryLister) ListSecretVersions(
	ctx context.Context, namespace string, limit int64,
) (*ConfigVersionPage, error) {
	return l.listVersions(ctx, secretResource, namespace, limit)
}

func (l *InventoryLister) listVersions(
	ctx context.Context, resource schema.GroupVersionResource, namespace string, limit int64,
) (*ConfigVersionPage, error) {
	if limit < 1 {
		return nil, fmt.Errorf(
			"%s inventory refused: non-positive limit %d would be an unbounded read",
			resource.Resource, limit)
	}
	list, err := l.meta.Resource(resource).Namespace(namespace).List(ctx, metav1.ListOptions{Limit: limit})
	if err != nil {
		return nil, mapReadError(err)
	}
	page := &ConfigVersionPage{Truncated: list.Continue != ""}
	for i := range list.Items {
		item := &list.Items[i]
		page.Versions = append(page.Versions, ConfigVersion{
			Namespace:       item.Namespace,
			Name:            item.Name,
			UID:             string(item.UID),
			ResourceVersion: item.ResourceVersion,
		})
	}
	return page, nil
}

func deploymentIntent(deployment *appsv1.Deployment) WorkloadIntent {
	intent := templateIntent("deployment", &deployment.ObjectMeta, &deployment.Spec.Template)
	intent.Replicas = declaredReplicas(deployment.Spec.Replicas)
	intent.ControllerRevision = deployment.Annotations["deployment.kubernetes.io/revision"]
	return intent
}

func statefulSetIntent(statefulSet *appsv1.StatefulSet) WorkloadIntent {
	intent := templateIntent("statefulset", &statefulSet.ObjectMeta, &statefulSet.Spec.Template)
	intent.Replicas = declaredReplicas(statefulSet.Spec.Replicas)
	return intent
}

func daemonSetIntent(daemonSet *appsv1.DaemonSet) WorkloadIntent {
	return templateIntent("daemonset", &daemonSet.ObjectMeta, &daemonSet.Spec.Template)
}

func templateIntent(
	kind string, meta *metav1.ObjectMeta, template *corev1.PodTemplateSpec,
) WorkloadIntent {
	intent := WorkloadIntent{
		Kind:         kind,
		Namespace:    meta.Namespace,
		Name:         meta.Name,
		UID:          string(meta.UID),
		Generation:   meta.Generation,
		TemplateHash: hashTemplate(template),
	}
	for i := range template.Spec.InitContainers {
		intent.Containers = append(intent.Containers,
			containerIntent(&template.Spec.InitContainers[i], true))
	}
	for i := range template.Spec.Containers {
		intent.Containers = append(intent.Containers,
			containerIntent(&template.Spec.Containers[i], false))
	}
	intent.ConfigMapRefs, intent.SecretRefs = referencedConfigs(&template.Spec)
	return intent
}

func containerIntent(container *corev1.Container, init bool) ContainerIntent {
	return ContainerIntent{
		Name:           container.Name,
		Init:           init,
		Image:          container.Image,
		RequestsCPU:    quantity(container.Resources.Requests, corev1.ResourceCPU),
		RequestsMemory: quantity(container.Resources.Requests, corev1.ResourceMemory),
		LimitsCPU:      quantity(container.Resources.Limits, corev1.ResourceCPU),
		LimitsMemory:   quantity(container.Resources.Limits, corev1.ResourceMemory),
	}
}

func quantity(list corev1.ResourceList, name corev1.ResourceName) string {
	value, ok := list[name]
	if !ok {
		return ""
	}
	return value.String()
}

func declaredReplicas(replicas *int32) string {
	if replicas == nil {
		return ""
	}
	return strconv.FormatInt(int64(*replicas), 10)
}

// referencedConfigs walks every place a pod template names a ConfigMap or Secret: env
// value sources, envFrom, volumes, projected volume sources and image pull secrets.
// Names only, sorted and deduplicated so the same template always renders the same
// reference list.
func referencedConfigs(spec *corev1.PodSpec) (configMaps, secrets []string) {
	configMapSet := map[string]bool{}
	secretSet := map[string]bool{}

	containers := make([]*corev1.Container, 0, len(spec.InitContainers)+len(spec.Containers))
	for i := range spec.InitContainers {
		containers = append(containers, &spec.InitContainers[i])
	}
	for i := range spec.Containers {
		containers = append(containers, &spec.Containers[i])
	}
	for _, container := range containers {
		for _, env := range container.Env {
			if env.ValueFrom == nil {
				continue
			}
			if ref := env.ValueFrom.ConfigMapKeyRef; ref != nil {
				configMapSet[ref.Name] = true
			}
			if ref := env.ValueFrom.SecretKeyRef; ref != nil {
				secretSet[ref.Name] = true
			}
		}
		for _, envFrom := range container.EnvFrom {
			if ref := envFrom.ConfigMapRef; ref != nil {
				configMapSet[ref.Name] = true
			}
			if ref := envFrom.SecretRef; ref != nil {
				secretSet[ref.Name] = true
			}
		}
	}

	for i := range spec.Volumes {
		volume := &spec.Volumes[i]
		if volume.ConfigMap != nil {
			configMapSet[volume.ConfigMap.Name] = true
		}
		if volume.Secret != nil {
			secretSet[volume.Secret.SecretName] = true
		}
		if volume.Projected != nil {
			for _, source := range volume.Projected.Sources {
				if source.ConfigMap != nil {
					configMapSet[source.ConfigMap.Name] = true
				}
				if source.Secret != nil {
					secretSet[source.Secret.Name] = true
				}
			}
		}
	}
	for _, pull := range spec.ImagePullSecrets {
		secretSet[pull.Name] = true
	}
	return sortedNames(configMapSet), sortedNames(secretSet)
}

func sortedNames(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// hashTemplate digests the whole pod template spec. json.Marshal over client-go types is
// deterministic within one build of this process, which is all the guarantee needed: the
// hash only ever compares two observations by the SAME process, because a restart
// re-baselines before it diffs anything.
func hashTemplate(template *corev1.PodTemplateSpec) string {
	encoded, err := json.Marshal(template.Spec)
	if err != nil {
		// A pod template that cannot marshal does not occur with client-go types; if it
		// ever does, a constant sentinel means "unknown" rather than a spurious diff.
		return "unhashable"
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:6])
}
