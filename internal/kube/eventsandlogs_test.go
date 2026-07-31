package kube

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// These tests assert what actually leaves the client — the query the API server receives,
// and the bound it receives it under. The port is the only place a Kubernetes read is
// shaped, so a bound that is missing here is a bound that does not exist.

func TestReader_ListEventsSendsOneBoundedPageRequest(t *testing.T) {
	s := newAPIServer(t)
	s.handle("/api/v1/namespaces/shop/events", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, &corev1.EventList{Items: []corev1.Event{
			{ObjectMeta: metav1.ObjectMeta{Name: "one"}, Reason: "BackOff"},
		}})
	})

	page, err := s.reader(t).ListEvents(context.Background(), "shop", InvolvedObject{}, 40)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	if len(page.Events) != 1 || page.Truncated {
		t.Fatalf("expected one event and a complete page, got %d truncated=%v",
			len(page.Events), page.Truncated)
	}

	query := s.requests[0].URL.Query()
	if query.Get("limit") != "40" {
		t.Fatalf("the list must carry its bound, got limit=%q", query.Get("limit"))
	}
	if query.Has("fieldSelector") {
		t.Fatalf("an unnarrowed read must send no selector, got %q", query.Get("fieldSelector"))
	}
	if query.Has("watch") || query.Has("follow") {
		t.Fatal("this port issues no watch and no follow")
	}
}

func TestReader_ContinuationTokenMarksTheEventPageTruncated(t *testing.T) {
	s := newAPIServer(t)
	s.handle("/api/v1/namespaces/shop/events", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, &corev1.EventList{
			ListMeta: metav1.ListMeta{Continue: "more"},
			Items:    []corev1.Event{{ObjectMeta: metav1.ObjectMeta{Name: "one"}}},
		})
	})

	page, err := s.reader(t).ListEvents(context.Background(), "shop", InvolvedObject{}, 40)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	if !page.Truncated {
		t.Fatal("a continuation token is the completeness basis and must not be dropped")
	}
	if len(s.requests) != 1 {
		t.Fatalf("the continuation must never be followed, got %d requests", len(s.requests))
	}
}

func TestReader_NarrowingIsRenderedFromIdentifiersOnly(t *testing.T) {
	s := newAPIServer(t)
	s.handle("/api/v1/namespaces/shop/events", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, &corev1.EventList{})
	})

	_, err := s.reader(t).ListEvents(context.Background(), "shop",
		InvolvedObject{Kind: "Pod", Name: "checkout-7f", UID: "abc-123"}, 40)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}

	selector, _ := url.QueryUnescape(s.requests[0].URL.Query().Get("fieldSelector"))
	for _, want := range []string{
		"involvedObject.kind=Pod",
		"involvedObject.name=checkout-7f",
		"involvedObject.uid=abc-123",
	} {
		if !strings.Contains(selector, want) {
			t.Errorf("the narrowing must reach the API server: %q missing from %q", want, selector)
		}
	}
}

func TestReader_PartialNarrowingSendsOnlyWhatWasSet(t *testing.T) {
	s := newAPIServer(t)
	s.handle("/api/v1/namespaces/shop/events", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, &corev1.EventList{})
	})

	_, err := s.reader(t).ListEvents(context.Background(), "shop",
		InvolvedObject{UID: "abc-123"}, 40)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}

	selector, _ := url.QueryUnescape(s.requests[0].URL.Query().Get("fieldSelector"))
	if strings.Contains(selector, "involvedObject.name=") {
		t.Fatalf("a field left empty narrows nothing and must not be sent as an empty "+
			"equality that matches nothing; got %q", selector)
	}
	if !strings.Contains(selector, "involvedObject.uid=abc-123") {
		t.Fatalf("the field that was set must be sent, got %q", selector)
	}
}

func TestReader_NonPositiveEventLimitRefusesAnUnboundedRead(t *testing.T) {
	s := newAPIServer(t)
	if _, err := s.reader(t).ListEvents(context.Background(), "shop", InvolvedObject{}, 0); err == nil {
		t.Fatal("a non-positive limit must be refused rather than issued unbounded")
	}
	if len(s.requests) != 0 {
		t.Fatal("nothing may reach the API server on a refused bound")
	}
}

func TestReader_GetPodMapsNotFoundToAMissingPodRatherThanAMissingWorkload(t *testing.T) {
	s := newAPIServer(t)
	s.handle("/api/v1/namespaces/shop/pods/gone", func(w http.ResponseWriter, _ *http.Request) {
		writeStatusError(w, http.StatusNotFound, metav1.StatusReasonNotFound, "pods \"gone\" not found")
	})

	_, err := s.reader(t).GetPod(context.Background(), "shop", "gone")

	var readErr *ReadError
	if !errors.As(err, &readErr) || readErr.Outcome != OutcomePodNotFound {
		t.Fatalf("a 404 on a pod read is a missing pod, got %v", err)
	}
}

func TestReader_ContainerLogsRequestsBoundedTimestampedOutputAndNeverFollows(t *testing.T) {
	s := newAPIServer(t)
	s.handle("/api/v1/namespaces/shop/pods/checkout-7f/log", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("2026-07-31T12:00:00Z hello\n"))
	})

	since := time.Date(2026, 7, 31, 11, 0, 0, 0, time.UTC)
	read, err := s.reader(t).ContainerLogs(context.Background(), LogRequest{
		Namespace: "shop", Pod: "checkout-7f", Container: "api",
		Previous: true, TailLines: 11, ReadCeiling: 4096, Since: since,
	})
	if err != nil {
		t.Fatalf("reading logs: %v", err)
	}
	if read.Truncated {
		t.Fatal("a read well inside its ceiling is not truncated")
	}

	query := s.requests[0].URL.Query()
	for key, want := range map[string]string{
		"container":  "api",
		"previous":   "true",
		"timestamps": "true",
		"tailLines":  "11",
		// One past the ceiling, so hitting it is detectable rather than ambiguous.
		"limitBytes": "4097",
	} {
		if query.Get(key) != want {
			t.Errorf("%s must be %q, got %q", key, want, query.Get(key))
		}
	}
	if query.Get("sinceTime") == "" {
		t.Error("sinceTime must bound the read at the source")
	}
	if query.Has("follow") && query.Get("follow") != "false" {
		t.Error("this port never follows a log stream")
	}
}

func TestReader_ALogReadThatFillsItsCeilingIsReportedTruncatedAndCutToTheBound(t *testing.T) {
	s := newAPIServer(t)
	s.handle("/api/v1/namespaces/shop/pods/checkout-7f/log", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 200)))
	})

	read, err := s.reader(t).ContainerLogs(context.Background(), LogRequest{
		Namespace: "shop", Pod: "checkout-7f", Container: "api",
		TailLines: 11, ReadCeiling: 64,
	})
	if err != nil {
		t.Fatalf("reading logs: %v", err)
	}
	if !read.Truncated {
		t.Fatal("a read that filled its ceiling had more to give and must say so")
	}
	if len(read.Raw) != 64 {
		t.Fatalf("nothing beyond the ceiling may be kept, got %d bytes", len(read.Raw))
	}
}

func TestReader_NonPositiveLogBoundsRefuseAnUnboundedRead(t *testing.T) {
	s := newAPIServer(t)
	for name, request := range map[string]LogRequest{
		"no line bound": {Namespace: "shop", Pod: "p", Container: "c", TailLines: 0, ReadCeiling: 64},
		"no byte bound": {Namespace: "shop", Pod: "p", Container: "c", TailLines: 10, ReadCeiling: 0},
	} {
		if _, err := s.reader(t).ContainerLogs(context.Background(), request); err == nil {
			t.Errorf("%s must be refused rather than issued unbounded", name)
		}
	}
	if len(s.requests) != 0 {
		t.Fatal("nothing may reach the API server on a refused bound")
	}
}

func TestReader_LogRefusalMapsToUnauthorized(t *testing.T) {
	s := newAPIServer(t)
	s.handle("/api/v1/namespaces/shop/pods/checkout-7f/log", func(w http.ResponseWriter, _ *http.Request) {
		writeStatusError(w, http.StatusForbidden, metav1.StatusReasonForbidden, "no")
	})

	_, err := s.reader(t).ContainerLogs(context.Background(), LogRequest{
		Namespace: "shop", Pod: "checkout-7f", Container: "api",
		TailLines: 11, ReadCeiling: 4096,
	})

	var readErr *ReadError
	if !errors.As(err, &readErr) || readErr.Outcome != OutcomeUnauthorized {
		t.Fatalf("the log subresource is granted separately from pods, and a refusal on it "+
			"must arrive as one; got %v", err)
	}
}
