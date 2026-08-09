package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
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

// GetPod reads one pod. A 404 maps to OutcomePodNotFound rather than the workload
// not-found the other GETs use, because the two send an investigator to different places:
// a missing workload is a wrong name in a plan, a missing pod is a pod that has gone.
func (r *Reader) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	pod, err := r.core.Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, mapReadErrorAs(err, OutcomePodNotFound)
	}
	return pod, nil
}

// ListEvents performs the single bounded event LIST. The narrowing is built from typed
// identifiers through the field-selector encoder — never from caller-supplied text — so
// there is no path by which a selector expression could be smuggled in. A non-positive
// limit is refused rather than issued as an unbounded read.
//
// The continuation token is never followed; its presence marks the page truncated, which
// is the completeness basis consumers rely on. Nothing here filters by time: the API
// cannot, so the window is applied by the caller over a page whose completeness this
// reports.
func (r *Reader) ListEvents(
	ctx context.Context, namespace string, involved InvolvedObject, limit int64,
) (*EventPage, error) {
	if limit < 1 {
		return nil, fmt.Errorf("event list refused: non-positive limit %d would be an unbounded read", limit)
	}

	options := metav1.ListOptions{Limit: limit}
	if selector := involvedObjectSelector(involved); selector != "" {
		options.FieldSelector = selector
	}

	list, err := r.core.Events(namespace).List(ctx, options)
	if err != nil {
		return nil, mapReadError(err)
	}
	return &EventPage{Events: list.Items, Truncated: list.Continue != ""}, nil
}

// involvedObjectSelector renders the narrowing. Only fields that were set take part, so an
// object identified by UID alone narrows by UID alone rather than by an empty name that
// would match nothing.
func involvedObjectSelector(involved InvolvedObject) string {
	if involved.IsZero() {
		return ""
	}
	set := fields.Set{}
	if involved.Kind != "" {
		set["involvedObject.kind"] = involved.Kind
	}
	if involved.Name != "" {
		set["involvedObject.name"] = involved.Name
	}
	if involved.UID != "" {
		set["involvedObject.uid"] = involved.UID
	}
	return set.AsSelector().String()
}

// ContainerLogs reads one container's output through the pods/log subresource, bounded in
// lines by the API and in bytes by the read ceiling. Timestamps are requested so each line
// carries its own provenance; Follow is never set, and the option does not appear here at
// all rather than appearing set to false.
//
// LimitBytes is set one byte above the ceiling so that hitting it is DETECTABLE: a read
// that returns exactly the ceiling is indistinguishable from a log that happened to be
// exactly that long, and a completeness basis that cannot tell those apart is not one.
func (r *Reader) ContainerLogs(ctx context.Context, request LogRequest) (*LogRead, error) {
	if request.TailLines < 1 || request.ReadCeiling < 1 {
		return nil, fmt.Errorf(
			"log read refused: non-positive bounds (%d lines, %d bytes) would be an unbounded read",
			request.TailLines, request.ReadCeiling)
	}

	tail, probe := request.TailLines, request.ReadCeiling+1
	options := &corev1.PodLogOptions{
		Container:  request.Container,
		Previous:   request.Previous,
		Timestamps: true,
		TailLines:  &tail,
		LimitBytes: &probe,
	}
	if !request.Since.IsZero() {
		since := metav1.NewTime(request.Since)
		options.SinceTime = &since
	}

	stream, err := r.core.Pods(request.Namespace).GetLogs(request.Pod, options).Stream(ctx)
	if err != nil {
		return nil, mapReadError(err)
	}
	defer func() { _ = stream.Close() }()

	// Read at most the probe. The API already bounds itself to it, so this is the backstop
	// that makes the bound true regardless of what the other end does.
	raw, err := io.ReadAll(io.LimitReader(stream, probe))
	if err != nil {
		return nil, mapReadError(err)
	}
	if int64(len(raw)) > request.ReadCeiling {
		return &LogRead{Raw: raw[:request.ReadCeiling], Truncated: true}, nil
	}
	return &LogRead{Raw: raw}, nil
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
	return mapReadErrorAs(err, OutcomeWorkloadNotFound)
}

// mapReadErrorAs is mapReadError with the caller naming what a 404 means for the read it
// issued. Only the caller knows what it asked for, and reporting a missing pod as a
// missing workload would send whoever reads the outcome to the wrong question.
//
// The API server's own message is never consulted to make this distinction. It is free
// text that changes between releases, and a taxonomy decided by string matching is a
// taxonomy that silently reclassifies itself on upgrade.
func mapReadErrorAs(err error, notFound ReadOutcome) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	switch {
	case apierrors.IsNotFound(err):
		return &ReadError{Outcome: notFound, Cause: err}
	case apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err):
		return &ReadError{Outcome: OutcomeUnauthorized, Cause: err}
	default:
		return &ReadError{Outcome: OutcomeUnreachable, Cause: err}
	}
}
