package inventory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-relay/internal/kube"
)

// ObjectKey identifies one object incarnation. UID is part of identity, not an
// attribute of it: a workload deleted and recreated under the same name is a different
// key, so its history can never read as a mutation of the old one.
type ObjectKey struct {
	Kind      relayv1.InventoryObjectKind
	Namespace string
	Name      string
	UID       string
}

// ObjectState is everything watched about one object: its revision and the watched
// fields' values. There is nowhere to put a status field, which is the point.
type ObjectState struct {
	// Revision is the dedup-key component: for workloads a composite of
	// metadata.generation and a digest of the watched fields, for ConfigMaps and
	// Secrets the resourceVersion. Generation alone would miss a change that moves no
	// generation (the Deployment revision annotation is metadata); a field digest alone
	// would collapse a value that changed and changed back into the original row. The
	// composite survives both.
	Revision string
	Fields   map[string]string
}

// Snapshot is one tick's view of a scope's watched objects.
type Snapshot map[ObjectKey]ObjectState

var workloadKinds = map[string]relayv1.InventoryObjectKind{
	"deployment":  relayv1.InventoryObjectKind_INVENTORY_OBJECT_KIND_DEPLOYMENT,
	"statefulset": relayv1.InventoryObjectKind_INVENTORY_OBJECT_KIND_STATEFULSET,
	"daemonset":   relayv1.InventoryObjectKind_INVENTORY_OBJECT_KIND_DAEMONSET,
}

// workloadState reduces a declared intent to its watched fields. Field names speak the
// workload's own terms so an operator reading a ledger entry recognises what moved.
func workloadState(intent kube.WorkloadIntent) (ObjectKey, ObjectState) {
	key := ObjectKey{
		Kind:      workloadKinds[intent.Kind],
		Namespace: intent.Namespace,
		Name:      intent.Name,
		UID:       intent.UID,
	}

	fields := map[string]string{}
	if intent.Replicas != "" {
		fields["spec.replicas"] = intent.Replicas
	}
	if intent.ControllerRevision != "" {
		fields["metadata.annotations[deployment.kubernetes.io/revision]"] = intent.ControllerRevision
	}
	for _, container := range intent.Containers {
		list := "containers"
		if container.Init {
			list = "initContainers"
		}
		prefix := fmt.Sprintf("spec.template.spec.%s[%s]", list, container.Name)
		fields[prefix+".image"] = container.Image
		setIfPresent(fields, prefix+".resources.requests.cpu", container.RequestsCPU)
		setIfPresent(fields, prefix+".resources.requests.memory", container.RequestsMemory)
		setIfPresent(fields, prefix+".resources.limits.cpu", container.LimitsCPU)
		setIfPresent(fields, prefix+".resources.limits.memory", container.LimitsMemory)
	}
	// The reference rollups are synthesized names rather than real paths, because a
	// reference can arrive through env, envFrom, a volume or a projected source and the
	// fact an investigation needs is "this workload stopped referencing that Secret",
	// not which of four syntaxes carried it.
	setIfPresent(fields, "references.configMaps", strings.Join(intent.ConfigMapRefs, ","))
	setIfPresent(fields, "references.secrets", strings.Join(intent.SecretRefs, ","))
	fields["spec.template"] = intent.TemplateHash

	return key, ObjectState{
		Revision: fmt.Sprintf("g%d.%s", intent.Generation, digestFields(fields)),
		Fields:   fields,
	}
}

// configState reduces a ConfigMap or Secret to identity and version. The single watched
// field is the resource version itself: the fact of a change is recorded, its content
// never is.
func configState(kind relayv1.InventoryObjectKind, version kube.ConfigVersion) (ObjectKey, ObjectState) {
	key := ObjectKey{
		Kind:      kind,
		Namespace: version.Namespace,
		Name:      version.Name,
		UID:       version.UID,
	}
	return key, ObjectState{
		Revision: version.ResourceVersion,
		Fields:   map[string]string{"metadata.resourceVersion": version.ResourceVersion},
	}
}

func setIfPresent(fields map[string]string, name, value string) {
	if value != "" {
		fields[name] = value
	}
}

// digestFields hashes the watched-field map in sorted order, so the same declared
// intent always produces the same revision component regardless of map iteration.
func digestFields(fields map[string]string) string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)

	hash := sha256.New()
	for _, name := range names {
		hash.Write([]byte(name))
		hash.Write([]byte{0})
		hash.Write([]byte(fields[name]))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))[:12]
}

// sortedKeys returns a snapshot's keys in deterministic order, so a baseline or a diff
// always lists objects the same way and a resent delta is byte-identical.
func sortedKeys(snapshot Snapshot) []ObjectKey {
	keys := make([]ObjectKey, 0, len(snapshot))
	for key := range snapshot {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.UID < b.UID
	})
	return keys
}
