package runtime

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/open-cluster/oc-relay/internal/capabilities"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// These tests pin the pod/container runtime mapping contract. The sharpest point is
// readiness POLARITY: a pod is ready only when a Ready condition explicitly reports
// status "True" — a missing condition list, a missing Ready condition, or any other
// status value means NOT ready. (This is the inverse of the EndpointSlice convention,
// where a nil ready field means ready; that convention belongs to service discovery and
// must never bleed into this mapping.)

func TestMapPod_ReadyPolarity(t *testing.T) {
	cases := []struct {
		name       string
		conditions []corev1.PodCondition
		want       bool
	}{
		{
			name:       "ready condition True",
			conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			want:       true,
		},
		{
			name:       "ready condition False",
			conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			want:       false,
		},
		{
			name:       "ready condition Unknown",
			conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionUnknown}},
			want:       false,
		},
		{
			name:       "no Ready condition present",
			conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}},
			want:       false,
		},
		{
			name:       "nil condition list",
			conditions: nil,
			want:       false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "web-0"},
				Status:     corev1.PodStatus{Conditions: tc.conditions},
			}
			got := MapPod(pod)
			if got.GetReady() != tc.want {
				t.Fatalf("Ready = %v, want %v", got.GetReady(), tc.want)
			}
		})
	}
}

func TestMapPod_ContainerStatePrecedence(t *testing.T) {
	started := metav1.NewTime(time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC))
	running := &corev1.ContainerStateRunning{StartedAt: started}
	terminated := &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}
	waiting := &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}

	cases := []struct {
		name          string
		state         corev1.ContainerState
		wantKind      relayv1.KubernetesContainerState_Kind
		wantWaiting   string
		wantRunning   bool
		wantTermState bool
	}{
		{
			name:        "running wins over terminated and waiting",
			state:       corev1.ContainerState{Running: running, Terminated: terminated, Waiting: waiting},
			wantKind:    relayv1.KubernetesContainerState_KIND_RUNNING,
			wantRunning: true,
		},
		{
			name:          "terminated wins over waiting",
			state:         corev1.ContainerState{Terminated: terminated, Waiting: waiting},
			wantKind:      relayv1.KubernetesContainerState_KIND_TERMINATED,
			wantTermState: true,
		},
		{
			name:        "waiting alone",
			state:       corev1.ContainerState{Waiting: waiting},
			wantKind:    relayv1.KubernetesContainerState_KIND_WAITING,
			wantWaiting: "CrashLoopBackOff",
		},
		{
			name:        "empty state falls to waiting with no reason",
			state:       corev1.ContainerState{},
			wantKind:    relayv1.KubernetesContainerState_KIND_WAITING,
			wantWaiting: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{Name: "app", State: tc.state}},
			}}
			containers := MapPod(pod).GetContainers()
			if len(containers) != 1 {
				t.Fatalf("expected 1 container, got %d", len(containers))
			}
			state := containers[0].GetState()
			if state.GetKind() != tc.wantKind {
				t.Fatalf("Kind = %v, want %v", state.GetKind(), tc.wantKind)
			}
			if state.GetWaitingReason() != tc.wantWaiting {
				t.Fatalf("WaitingReason = %q, want %q", state.GetWaitingReason(), tc.wantWaiting)
			}
			if gotRunning := state.GetRunningSince() != nil; gotRunning != tc.wantRunning {
				t.Fatalf("RunningSince present = %v, want %v", gotRunning, tc.wantRunning)
			}
			if gotTerm := state.GetTermination() != nil; gotTerm != tc.wantTermState {
				t.Fatalf("Termination present = %v, want %v", gotTerm, tc.wantTermState)
			}
		})
	}
}

// The control plane's OOM-absence detection reads the literal "OOMKilled" on BOTH the
// current-state termination and last_termination; this mapping must preserve the reason
// verbatim on both fields and default an empty reason to the stable literal "Unknown".
func TestMapPod_TerminationContract(t *testing.T) {
	finished := metav1.NewTime(time.Date(2026, 7, 20, 9, 30, 0, 0, time.UTC))
	pod := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{
			{
				Name: "oom-now",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Reason: "OOMKilled", ExitCode: 137, FinishedAt: finished,
				}},
			},
			{
				Name:  "oom-before",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137},
				},
			},
			{
				Name: "reasonless",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 2,
				}},
			},
		},
	}}

	containers := MapPod(pod).GetContainers()
	if len(containers) != 3 {
		t.Fatalf("expected 3 containers, got %d", len(containers))
	}

	oomNow := containers[0].GetState().GetTermination()
	if oomNow.GetReason() != "OOMKilled" || oomNow.GetExitCode() != 137 {
		t.Fatalf("current-state termination = %q/%d, want OOMKilled/137", oomNow.GetReason(), oomNow.GetExitCode())
	}
	if oomNow.GetFinishedAt() == nil || !oomNow.GetFinishedAt().AsTime().Equal(finished.Time) {
		t.Fatalf("FinishedAt not preserved: %v", oomNow.GetFinishedAt())
	}
	if containers[0].GetLastTermination() != nil {
		t.Fatalf("no last termination expected for oom-now")
	}

	oomBefore := containers[1]
	if oomBefore.GetState().GetKind() != relayv1.KubernetesContainerState_KIND_RUNNING {
		t.Fatalf("oom-before should be running now")
	}
	if oomBefore.GetLastTermination().GetReason() != "OOMKilled" {
		t.Fatalf("last_termination reason = %q, want OOMKilled", oomBefore.GetLastTermination().GetReason())
	}

	reasonless := containers[2].GetState().GetTermination()
	if reasonless.GetReason() != "Unknown" {
		t.Fatalf("empty termination reason must default to %q, got %q", "Unknown", reasonless.GetReason())
	}
	if reasonless.GetFinishedAt() != nil {
		t.Fatalf("zero FinishedAt must be absent, got %v", reasonless.GetFinishedAt())
	}
}

// Every string admitted from the source is untrusted and capped at the contract
// boundary with the control plane's per-field ceilings: identifiers 253,
// reasons 128, images 256.
func TestMapPod_IdentityFieldsAndCaps(t *testing.T) {
	longName := strings.Repeat("n", 300)
	longReason := strings.Repeat("r", 200)
	longImage := strings.Repeat("i", 300)
	started := metav1.NewTime(time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC))

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: longName},
		Spec:       corev1.PodSpec{NodeName: "node-a"},
		Status: corev1.PodStatus{
			Phase:     corev1.PodPhase(longReason),
			StartTime: &started,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: longReason},
			},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "app",
				Image: longImage,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: longReason}},
			}},
		},
	}

	got := MapPod(pod)
	if len(got.GetName()) != 253 {
		t.Fatalf("pod name must cap at 253, got %d", len(got.GetName()))
	}
	if len(got.GetPhase()) != 128 {
		t.Fatalf("phase must cap at 128, got %d", len(got.GetPhase()))
	}
	if got.GetNodeName() != "node-a" {
		t.Fatalf("NodeName = %q, want node-a", got.GetNodeName())
	}
	if got.GetStartedAt() == nil || !got.GetStartedAt().AsTime().Equal(started.Time) {
		t.Fatalf("StartedAt not preserved: %v", got.GetStartedAt())
	}
	if len(got.GetUnscheduledReason()) != 128 {
		t.Fatalf("unscheduled reason must cap at 128, got %d", len(got.GetUnscheduledReason()))
	}
	container := got.GetContainers()[0]
	if len(container.GetImage()) != 256 {
		t.Fatalf("image must cap at 256, got %d", len(container.GetImage()))
	}
	if len(container.GetState().GetWaitingReason()) != 128 {
		t.Fatalf("waiting reason must cap at 128, got %d", len(container.GetState().GetWaitingReason()))
	}
}

// ready and restart_count pass through from the container status untouched — both are
// core parity fields, pinned so a dropped or negated mapping cannot survive the suite.
func TestMapPod_ContainerPassthroughFields(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{
			{Name: "healthy", Ready: true, RestartCount: 0},
			{Name: "crashing", Ready: false, RestartCount: 7},
		},
	}}
	containers := MapPod(pod).GetContainers()
	if !containers[0].GetReady() || containers[0].GetRestartCount() != 0 {
		t.Fatalf("healthy container: Ready=%v RestartCount=%d, want true/0",
			containers[0].GetReady(), containers[0].GetRestartCount())
	}
	if containers[1].GetReady() || containers[1].GetRestartCount() != 7 {
		t.Fatalf("crashing container: Ready=%v RestartCount=%d, want false/7",
			containers[1].GetReady(), containers[1].GetRestartCount())
	}
}

// The cap counts runes (recorded deviation from the control plane's UTF-16 slicing): an
// over-cap non-BMP string keeps exactly maxChars runes and never emits a lone
// surrogate or a torn UTF-8 sequence.
func TestCapChars_NonBMPInput(t *testing.T) {
	astral := strings.Repeat("\U0001D51E", 300)
	capped := capabilities.CapIdentifier(astral)
	if got := utf8.RuneCountInString(capped); got != 253 {
		t.Fatalf("astral identifier must cap at 253 runes, got %d", got)
	}
	if !utf8.ValidString(capped) {
		t.Fatalf("capped string must remain valid UTF-8")
	}
	if under := strings.Repeat("\U0001D51E", 100); capabilities.CapIdentifier(under) != under {
		t.Fatalf("under-cap non-BMP string must pass through unchanged")
	}
}

func TestMapPod_DefaultsAndScheduledPod(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, Reason: "Scheduled"},
			},
		},
	}
	got := MapPod(pod)
	if got.GetPhase() != "Unknown" {
		t.Fatalf("empty phase must default to Unknown, got %q", got.GetPhase())
	}
	if got.GetNodeName() != "" {
		t.Fatalf("absent node must be empty, got %q", got.GetNodeName())
	}
	if got.GetStartedAt() != nil {
		t.Fatalf("absent start time must be nil, got %v", got.GetStartedAt())
	}
	if got.GetUnscheduledReason() != "" {
		t.Fatalf("a scheduled pod must carry no unscheduled reason, got %q", got.GetUnscheduledReason())
	}
}
