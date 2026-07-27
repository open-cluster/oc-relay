package kube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// apiServer is a scripted Kubernetes API endpoint: one handler per path, plus a record
// of every request so tests can assert exactly what left the client.
type apiServer struct {
	t        *testing.T
	server   *httptest.Server
	handlers map[string]http.HandlerFunc

	requests []*http.Request
}

func newAPIServer(t *testing.T) *apiServer {
	t.Helper()
	s := &apiServer{t: t, handlers: map[string]http.HandlerFunc{}}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests = append(s.requests, r.Clone(r.Context()))
		if handler, ok := s.handlers[r.URL.Path]; ok {
			handler(w, r)
			return
		}
		writeStatusError(w, http.StatusNotFound, metav1.StatusReasonNotFound, "not scripted")
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *apiServer) handle(path string, handler http.HandlerFunc) {
	s.handlers[path] = handler
}

func (s *apiServer) reader(t *testing.T) *Reader {
	t.Helper()
	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: s.server.URL})
	if err != nil {
		t.Fatalf("building clientset: %v", err)
	}
	return NewReader(clientset)
}

func writeJSON(w http.ResponseWriter, object any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(object)
}

func writeStatusError(w http.ResponseWriter, code int, reason metav1.StatusReason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(&metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure,
		Code:     int32(code),
		Reason:   reason,
		Message:  message,
	})
}

func int32Ptr(v int32) *int32 { return &v }

func TestReader_TypedWorkloadGets(t *testing.T) {
	s := newAPIServer(t)
	s.handle("/apis/apps/v1/namespaces/shop/deployments/web", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, &appsv1.Deployment{
			TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop", UID: types.UID("uid-dep")},
			Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(3)},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: 2},
		})
	})
	s.handle("/apis/apps/v1/namespaces/shop/statefulsets/db", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, &appsv1.StatefulSet{
			TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
			ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop", UID: types.UID("uid-sts")},
			Spec:       appsv1.StatefulSetSpec{Replicas: int32Ptr(2)},
		})
	})
	s.handle("/apis/apps/v1/namespaces/shop/daemonsets/agent", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, &appsv1.DaemonSet{
			TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
			ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "shop", UID: types.UID("uid-ds")},
			Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 4},
		})
	})
	reader := s.reader(t)
	ctx := context.Background()

	deployment, err := reader.GetDeployment(ctx, "shop", "web")
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	if deployment.Name != "web" || *deployment.Spec.Replicas != 3 || deployment.Status.ReadyReplicas != 2 {
		t.Fatalf("deployment fields not mapped: %+v", deployment)
	}

	statefulSet, err := reader.GetStatefulSet(ctx, "shop", "db")
	if err != nil {
		t.Fatalf("GetStatefulSet: %v", err)
	}
	if statefulSet.Name != "db" || *statefulSet.Spec.Replicas != 2 {
		t.Fatalf("statefulset fields not mapped: %+v", statefulSet)
	}

	daemonSet, err := reader.GetDaemonSet(ctx, "shop", "agent")
	if err != nil {
		t.Fatalf("GetDaemonSet: %v", err)
	}
	if daemonSet.Name != "agent" || daemonSet.Status.DesiredNumberScheduled != 4 {
		t.Fatalf("daemonset fields not mapped: %+v", daemonSet)
	}
}

func TestReader_ListPodsSendsOneBoundedPageRequest(t *testing.T) {
	s := newAPIServer(t)
	s.handle("/api/v1/namespaces/shop/pods", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, &corev1.PodList{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
			Items: []corev1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "shop"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "web-2", Namespace: "shop"}},
			},
		})
	})
	reader := s.reader(t)

	selector := "app=web,tier in (frontend,edge),!legacy"
	page, err := reader.ListPods(context.Background(), "shop", selector, 17)
	if err != nil {
		t.Fatalf("ListPods: %v", err)
	}
	if len(page.Pods) != 2 || page.Pods[0].Name != "web-1" {
		t.Fatalf("pods not returned: %+v", page.Pods)
	}
	if page.Truncated {
		t.Fatal("a continuation-free page must be complete")
	}

	if len(s.requests) != 1 {
		t.Fatalf("expected exactly one LIST request, got %d", len(s.requests))
	}
	query := s.requests[0].URL.Query()
	if got := query.Get("labelSelector"); got != selector {
		t.Fatalf("complete selector must reach the API: got %q want %q", got, selector)
	}
	if got := query.Get("limit"); got != "17" {
		t.Fatalf("bounded page: limit must reach the API: got %q", got)
	}
	if query.Has("continue") {
		t.Fatal("a single bounded page must never follow a continuation token")
	}
}

func TestReader_ContinuationTokenMarksPageTruncated(t *testing.T) {
	s := newAPIServer(t)
	s.handle("/api/v1/namespaces/shop/pods", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, &corev1.PodList{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
			ListMeta: metav1.ListMeta{Continue: "next-token"},
			Items:    []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "shop"}}},
		})
	})
	reader := s.reader(t)

	page, err := reader.ListPods(context.Background(), "shop", "app=web", 1)
	if err != nil {
		t.Fatalf("ListPods: %v", err)
	}
	if !page.Truncated {
		t.Fatal("a continuation token means the read is incomplete")
	}
	if len(s.requests) != 1 {
		t.Fatalf("the continuation token must not be followed: got %d requests", len(s.requests))
	}
}

func TestReader_EmptySelectorRefusesEnumeration(t *testing.T) {
	s := newAPIServer(t)
	reader := s.reader(t)

	for _, selector := range []string{"", "   "} {
		_, err := reader.ListPods(context.Background(), "shop", selector, 10)
		var readErr *ReadError
		if !errors.As(err, &readErr) || readErr.Outcome != OutcomeSelectorUnrepresentable {
			t.Fatalf("selector %q: want SelectorUnrepresentable refusal, got %v", selector, err)
		}
	}
	if len(s.requests) != 0 {
		t.Fatalf("an empty selector must never reach the API (would enumerate the namespace): %d requests", len(s.requests))
	}
}

func TestReader_NonPositiveLimitRefusesEnumeration(t *testing.T) {
	s := newAPIServer(t)
	reader := s.reader(t)

	for _, limit := range []int64{0, -1} {
		if _, err := reader.ListPods(context.Background(), "shop", "app=web", limit); err == nil {
			t.Fatalf("limit %d: an unbounded list must be refused", limit)
		}
	}
	if len(s.requests) != 0 {
		t.Fatalf("a non-positive limit must never reach the API: %d requests", len(s.requests))
	}
}

func TestReader_NotFoundMapsToWorkloadNotFound(t *testing.T) {
	s := newAPIServer(t)
	s.handle("/apis/apps/v1/namespaces/shop/deployments/gone", func(w http.ResponseWriter, _ *http.Request) {
		writeStatusError(w, http.StatusNotFound, metav1.StatusReasonNotFound, `deployments.apps "gone" not found`)
	})
	reader := s.reader(t)

	_, err := reader.GetDeployment(context.Background(), "shop", "gone")
	var readErr *ReadError
	if !errors.As(err, &readErr) || readErr.Outcome != OutcomeWorkloadNotFound {
		t.Fatalf("want WorkloadNotFound, got %v", err)
	}
	if !apierrors.IsNotFound(readErr.Cause) {
		t.Fatalf("cause must retain the typed API error for local diagnostics: %v", readErr.Cause)
	}
}

func TestReader_UnauthorizedAndForbiddenMapToUnauthorized(t *testing.T) {
	cases := []struct {
		name   string
		code   int
		reason metav1.StatusReason
	}{
		{name: "forbidden", code: http.StatusForbidden, reason: metav1.StatusReasonForbidden},
		{name: "unauthorized", code: http.StatusUnauthorized, reason: metav1.StatusReasonUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newAPIServer(t)
			s.handle("/apis/apps/v1/namespaces/shop/statefulsets/db", func(w http.ResponseWriter, _ *http.Request) {
				writeStatusError(w, tc.code, tc.reason, "RBAC: access denied")
			})
			reader := s.reader(t)

			_, err := reader.GetStatefulSet(context.Background(), "shop", "db")
			var readErr *ReadError
			if !errors.As(err, &readErr) || readErr.Outcome != OutcomeUnauthorized {
				t.Fatalf("want Unauthorized, got %v", err)
			}
		})
	}
}

func TestReader_UnreachableAPIMapsToUnreachable(t *testing.T) {
	s := newAPIServer(t)
	reader := s.reader(t)
	s.server.Close()

	_, err := reader.GetDaemonSet(context.Background(), "shop", "agent")
	var readErr *ReadError
	if !errors.As(err, &readErr) || readErr.Outcome != OutcomeUnreachable {
		t.Fatalf("want Unreachable, got %v", err)
	}
}

func TestReader_CallerCancellationPassesThroughUnmapped(t *testing.T) {
	s := newAPIServer(t)
	reader := s.reader(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := reader.GetDeployment(ctx, "shop", "web")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation must pass through for the executor to classify: %v", err)
	}
	var readErr *ReadError
	if errors.As(err, &readErr) {
		t.Fatalf("a cancellation must not be mapped into a read outcome: %v", readErr)
	}
}

func TestReader_DeadlineExpiryPassesThroughUnmapped(t *testing.T) {
	s := newAPIServer(t)
	s.handle("/api/v1/namespaces/shop/pods", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never answer; only the client's deadline ends this call
	})
	reader := s.reader(t)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := reader.ListPods(ctx, "shop", "app=web", 5)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a deadline expiry must pass through for the executor to classify as timeout: %v", err)
	}
	var readErr *ReadError
	if errors.As(err, &readErr) {
		t.Fatalf("a deadline expiry must not be mapped into a read outcome: %v", readErr)
	}
}

func TestReader_ClusterFingerprintIsKubeSystemNamespaceUID(t *testing.T) {
	s := newAPIServer(t)
	s.handle("/api/v1/namespaces/kube-system", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, &corev1.Namespace{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
			ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("fp-1234")},
		})
	})
	reader := s.reader(t)

	fingerprint, err := reader.ClusterFingerprint(context.Background())
	if err != nil {
		t.Fatalf("ClusterFingerprint: %v", err)
	}
	if fingerprint != "fp-1234" {
		t.Fatalf("fingerprint must be the kube-system namespace UID: got %q", fingerprint)
	}
}

func TestReader_ClusterFingerprintErrorIsMapped(t *testing.T) {
	s := newAPIServer(t)
	s.handle("/api/v1/namespaces/kube-system", func(w http.ResponseWriter, _ *http.Request) {
		writeStatusError(w, http.StatusForbidden, metav1.StatusReasonForbidden, "RBAC: access denied")
	})
	reader := s.reader(t)

	_, err := reader.ClusterFingerprint(context.Background())
	var readErr *ReadError
	if !errors.As(err, &readErr) || readErr.Outcome != OutcomeUnauthorized {
		t.Fatalf("want Unauthorized, got %v", err)
	}
}
