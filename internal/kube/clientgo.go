package kube

import (
	"context"
	"errors"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	appsv1client "k8s.io/client-go/kubernetes/typed/apps/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
)

// Reader is the client-go implementation of WorkloadReader: typed reads only, one
// bounded pod page per call, and every Kubernetes API failure mapped into the closed
// ReadOutcome taxonomy before it leaves this package. Context errors pass through
// unmapped so the capability layer can distinguish the caller's cancellation from a
// deadline expiry — a distinction an outcome enum must never absorb.
type Reader struct {
	apps appsv1client.AppsV1Interface
	core corev1client.CoreV1Interface
}

// NewReader wraps a typed clientset. Only the apps/v1 and core/v1 read surfaces are
// retained; nothing in this package can reach a write, exec, port-forward, or watch verb.
func NewReader(clientset kubernetes.Interface) *Reader {
	return &Reader{apps: clientset.AppsV1(), core: clientset.CoreV1()}
}

func (r *Reader) GetDeployment(ctx context.Context, namespace, name string) (*appsv1.Deployment, error) {
	deployment, err := r.apps.Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, mapReadError(err)
	}
	return deployment, nil
}

func (r *Reader) GetStatefulSet(ctx context.Context, namespace, name string) (*appsv1.StatefulSet, error) {
	statefulSet, err := r.apps.StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, mapReadError(err)
	}
	return statefulSet, nil
}

func (r *Reader) GetDaemonSet(ctx context.Context, namespace, name string) (*appsv1.DaemonSet, error) {
	daemonSet, err := r.apps.DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, mapReadError(err)
	}
	return daemonSet, nil
}

// ListPods performs the single bounded LIST. It refuses an empty selector — an empty
// filter would enumerate the whole namespace and attribute unrelated pods to the
// workload — and refuses a non-positive limit rather than issue an unbounded read.
// The continuation token is never followed; its presence marks the page truncated,
// which is the completeness basis consumers rely on.
func (r *Reader) ListPods(ctx context.Context, namespace, selector string, limit int64) (*PodPage, error) {
	if strings.TrimSpace(selector) == "" {
		return nil, &ReadError{Outcome: OutcomeSelectorUnrepresentable}
	}
	if limit < 1 {
		return nil, fmt.Errorf("pod list refused: non-positive limit %d would be an unbounded read", limit)
	}

	list, err := r.core.Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
		Limit:         limit,
	})
	if err != nil {
		return nil, mapReadError(err)
	}
	return &PodPage{Pods: list.Items, Truncated: list.Continue != ""}, nil
}

// ClusterFingerprint reads the kube-system namespace UID — the stable identity a
// registration is pinned to server-side, so a re-pointed or rebuilt cluster is detected
// rather than silently mis-attributed.
func (r *Reader) ClusterFingerprint(ctx context.Context) (string, error) {
	namespace, err := r.core.Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return "", mapReadError(err)
	}
	return string(namespace.UID), nil
}

// mapReadError maps a client-go failure into the closed outcome taxonomy. Context
// errors pass through unmapped (the caller owns the cancel-vs-timeout distinction).
// Every 404 maps to WorkloadNotFound — a not-found is a typed outcome, never absence
// evidence, and no namespace-level 404 path is distinguished. 401 and 403 collapse to
// one Unauthorized outcome: which of the two the API server chose is not a fact worth
// exporting. Everything else is Unreachable. The cause is retained for local
// diagnostics; only the outcome enum ever reaches the wire.
func mapReadError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	switch {
	case apierrors.IsNotFound(err):
		return &ReadError{Outcome: OutcomeWorkloadNotFound, Cause: err}
	case apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err):
		return &ReadError{Outcome: OutcomeUnauthorized, Cause: err}
	default:
		return &ReadError{Outcome: OutcomeUnreachable, Cause: err}
	}
}
