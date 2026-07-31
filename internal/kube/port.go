// Package kube is the Relay's read-only Kubernetes port: the narrow boundary through
// which capabilities read cluster state. It exposes exactly the reads the compiled
// capabilities need — typed GETs of the three workload kinds and of a pod, a single
// bounded pod LIST, a single bounded event LIST, and one bounded container log read — and
// nothing that could mutate the cluster. The real implementation wraps a client-go typed
// clientset (no informers, no LIST/WATCH beyond the one bounded page, no follow on a log
// stream); tests substitute a fake reader.
//
// Every read here is bounded before it is issued and carries its own completeness basis
// back, because a read whose limit is invisible downstream is a read that can be mistaken
// for the whole truth.
package kube

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// ReadOutcome is the closed read-outcome taxonomy, mirroring the central contract and the
// wire enums. A not-found is NEVER absence evidence; an unrepresentable selector refuses
// enumeration rather than listing a whole namespace.
//
// It is one taxonomy across the port while each capability's WIRE enum carries only the
// values that capability can actually produce. The port is where a client-go failure stops
// being an HTTP status, so it needs every outcome any caller might see; a wire enum with
// unreachable values would not be the closed vocabulary it claims to be.
type ReadOutcome int

const (
	OutcomeSuccess ReadOutcome = iota
	OutcomeUnreachable
	OutcomeUnauthorized
	OutcomeWorkloadNotFound
	OutcomeNamespaceNotFound
	OutcomeSelectorUnrepresentable
	OutcomePodNotFound
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

// InvolvedObject narrows an event read to one object. Every field is an identifier
// compared for equality — never a pattern, an expression, or anything a caller could turn
// into a query language. A zero value means the whole namespace.
type InvolvedObject struct {
	Kind string
	Name string
	UID  string
}

// IsZero reports whether this narrows nothing.
func (o InvolvedObject) IsZero() bool {
	return o.Kind == "" && o.Name == "" && o.UID == ""
}

// EventPage is a single bounded event LIST page. Completeness is continuation-based for
// the same reason it is for pods, and it matters more here: the Kubernetes API neither
// sorts events nor filters them by time, so a truncated page is an arbitrary subset and no
// window filter applied to it can support a claim of absence.
type EventPage struct {
	Events    []corev1.Event
	Truncated bool
}

// EventReader lists what a cluster said it did, bounded to one page.
type EventReader interface {
	// ListEvents lists at most limit events in namespace, narrowed to involved when it is
	// not zero, following no continuation.
	ListEvents(ctx context.Context, namespace string, involved InvolvedObject, limit int64) (*EventPage, error)
}

// LogRequest is one bounded container log read. There is no follow, stream or tail-forever
// field, and its absence is structural: adding one would turn a read into an open channel
// out of the cluster.
type LogRequest struct {
	Namespace string
	Pod       string
	Container string
	// Previous selects the terminated instance rather than the running one.
	Previous bool
	// TailLines bounds how many lines are asked for, counted from the end of the log.
	TailLines int64
	// ReadCeiling bounds how many bytes this read may pull into the Relay. It is a memory
	// and network bound on what is FETCHED and is deliberately allowed to exceed the bound
	// on what is EMITTED: keeping the newest lines when a byte bound binds means having
	// read past it first. Nothing beyond the emitted bound ever leaves the cluster.
	ReadCeiling int64
	// Since, when non-zero, is the earliest emission time asked for.
	Since time.Time
}

// LogRead is what one bounded log read obtained. Truncated reports that the read stopped
// at ReadCeiling, which means the log had more to give and the last line is very likely
// cut mid-line.
type LogRead struct {
	Raw       []byte
	Truncated bool
}

// LogReader reads one container's output, bounded, with no follow.
type LogReader interface {
	GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error)
	ContainerLogs(ctx context.Context, request LogRequest) (*LogRead, error)
}
