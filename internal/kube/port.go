// Package kube is the Relay's read-only Kubernetes port: the narrow boundary through
// which capabilities read cluster state. It exposes exactly the reads the R1 workload-
// runtime capability needs — typed GETs of the three workload kinds and a single bounded
// pod LIST — and nothing that could mutate the cluster. The real implementation wraps a
// client-go typed clientset (no informers, no LIST/WATCH beyond the one bounded page);
// tests substitute a fake reader.
package kube

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// ReadOutcome is the closed read-outcome taxonomy, mirroring the central contract and the
// wire enum. A not-found is NEVER absence evidence; an unrepresentable selector refuses
// enumeration rather than listing a whole namespace.
type ReadOutcome int

const (
	OutcomeSuccess ReadOutcome = iota
	OutcomeUnreachable
	OutcomeUnauthorized
	OutcomeWorkloadNotFound
	OutcomeNamespaceNotFound
	OutcomeSelectorUnrepresentable
)

// ReadError carries a mapped outcome from the port so the executor never inspects raw
// client-go error types. The real adapter maps apierrors (403/401 → Unauthorized, 404 →
// WorkloadNotFound, else Unreachable); the underlying cause is retained but never its
// free-text message on the wire.
type ReadError struct {
	Outcome ReadOutcome
	Cause   error
}

func (e *ReadError) Error() string {
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return "kube read error"
}

func (e *ReadError) Unwrap() error { return e.Cause }

// PodPage is a single bounded LIST page: the pods returned plus whether a continuation
// token remained. Completeness is continuation-based — a page with no continue token is a
// complete, list-without-continuation read.
type PodPage struct {
	Pods      []corev1.Pod
	Truncated bool
}

// WorkloadReader is the read-only surface. Every method returns a *ReadError on failure so
// the caller maps an outcome without touching client-go error types. A read that succeeds
// returns the typed object and a nil error.
type WorkloadReader interface {
	GetDeployment(ctx context.Context, namespace, name string) (*appsv1.Deployment, error)
	GetStatefulSet(ctx context.Context, namespace, name string) (*appsv1.StatefulSet, error)
	GetDaemonSet(ctx context.Context, namespace, name string) (*appsv1.DaemonSet, error)
	// ListPods lists at most limit pods matching selector in namespace, following no
	// continuation — one bounded page.
	ListPods(ctx context.Context, namespace, selector string, limit int64) (*PodPage, error)
}
