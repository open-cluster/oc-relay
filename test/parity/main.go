// Command parity executes one kubernetes.workload.runtime.v1 read through the REAL
// production components — the hardened kubeconfig loader, the client-go reader, and
// the capability executor — and prints the wire result as protojson. It is the Go half
// of the cross-implementation differential gate: a driver feeds the same cluster to
// this command and to the control plane's reader and compares normalized semantics.
// The local pod cap is pinned to the requested bound so the local-cap layer never
// diverges the differential for a non-bug reason.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
	"k8s.io/client-go/kubernetes"

	"github.com/open-cluster/oc-relay/internal/capabilities/kubernetes/runtime"
	"github.com/open-cluster/oc-relay/internal/kube"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

func main() {
	kubeconfig := flag.String("kubeconfig", "", "path to the harness kubeconfig (required)")
	namespace := flag.String("namespace", "default", "workload namespace")
	kindName := flag.String("kind", "deployment", "workload kind: deployment|statefulset|daemonset")
	name := flag.String("name", "", "workload name (required)")
	maxPods := flag.Int64("max-pods", 50, "dispatched pod bound")
	deadlineMs := flag.Int64("deadline-ms", 30_000, "deadline budget in milliseconds")
	flag.Parse()

	if err := run(*kubeconfig, *namespace, *kindName, *name, *maxPods, *deadlineMs); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(kubeconfigPath, namespace, kindName, name string, maxPods, deadlineMs int64) error {
	kind, ok := map[string]relayv1.WorkloadKind{
		"deployment":  relayv1.WorkloadKind_WORKLOAD_KIND_DEPLOYMENT,
		"statefulset": relayv1.WorkloadKind_WORKLOAD_KIND_STATEFULSET,
		"daemonset":   relayv1.WorkloadKind_WORKLOAD_KIND_DAEMONSET,
	}[kindName]
	if !ok {
		return fmt.Errorf("unknown workload kind %q", kindName)
	}

	restConfig, err := kube.LoadKubeconfig(kubeconfigPath)
	if err != nil {
		return fmt.Errorf("loading kubeconfig: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("building clientset: %w", err)
	}

	executor := runtime.NewExecutor(kube.NewReader(clientset), maxPods)
	job := &relayv1.JobAssignment{
		JobId:             "parity",
		LeaseEpoch:        1,
		CapabilityId:      runtime.CapabilityID,
		CapabilityVersion: runtime.CapabilityVersion,
		DeadlineBudget:    durationpb.New(time.Duration(deadlineMs) * time.Millisecond),
		Arguments: &relayv1.CapabilityArguments{
			Arguments: &relayv1.CapabilityArguments_KubernetesWorkloadRuntimeV1{
				KubernetesWorkloadRuntimeV1: &relayv1.KubernetesWorkloadRuntimeArgsV1{
					Namespace:    namespace,
					WorkloadKind: kind,
					WorkloadName: name,
					MaxPods:      uint32(maxPods),
				},
			},
		},
	}

	started := time.Now()
	result := executor.Execute(context.Background(), job)
	result.ExecutionDurationMs = time.Since(started).Milliseconds()

	encoded, err := protojson.Marshal(result)
	if err != nil {
		return fmt.Errorf("encoding result: %w", err)
	}
	fmt.Println(string(encoded))
	return nil
}
