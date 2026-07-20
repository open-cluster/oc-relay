package runtime

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// These tests pin the S1B-3 selector contract the Go capability must match for parity.
// The recorded S1B-3 exit-review bug: a matchExpressions-only workload built an EMPTY
// selector and listed the WHOLE namespace, misattributing unrelated pods' runtime state.
// The fix renders the COMPLETE selector (matchLabels + matchExpressions in order) and,
// when it cannot represent the selector, REFUSES to enumerate rather than falling back to
// an empty filter that would match everything.

func TestRenderSelector_MatchLabelsOnly(t *testing.T) {
	sel := &metav1.LabelSelector{
		MatchLabels: map[string]string{"app": "web", "tier": "frontend"},
	}
	got, ok := RenderSelector(sel)
	if !ok {
		t.Fatalf("expected representable selector, got refusal")
	}
	// matchLabels render as key=value; map order is non-deterministic in Go, so both
	// orderings are acceptable — the label-selector semantics are order-independent.
	if got != "app=web,tier=frontend" && got != "tier=frontend,app=web" {
		t.Fatalf("unexpected render: %q", got)
	}
}

func TestRenderSelector_MatchExpressionsOnly_IsNotEmpty(t *testing.T) {
	// The exact bug class: matchExpressions with no matchLabels must NOT render empty.
	sel := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"web", "api"}},
		},
	}
	got, ok := RenderSelector(sel)
	if !ok {
		t.Fatalf("representable matchExpressions selector wrongly refused")
	}
	if got == "" {
		t.Fatalf("matchExpressions-only selector rendered EMPTY — the S1B-3 whole-namespace bug")
	}
	if got != "app in (web,api)" {
		t.Fatalf("unexpected In render: %q", got)
	}
}

func TestRenderSelector_AllOperators(t *testing.T) {
	sel := &metav1.LabelSelector{
		MatchLabels: map[string]string{"app": "web"},
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "env", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"dev", "test"}},
			{Key: "canary", Operator: metav1.LabelSelectorOpExists},
			{Key: "legacy", Operator: metav1.LabelSelectorOpDoesNotExist},
		},
	}
	got, ok := RenderSelector(sel)
	if !ok {
		t.Fatalf("representable selector wrongly refused")
	}
	// matchLabels first (single label = deterministic), then expressions in order.
	want := "app=web,env notin (dev,test),canary,!legacy"
	if got != want {
		t.Fatalf("render mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestRenderSelector_UnknownOperator_Refuses(t *testing.T) {
	sel := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "app", Operator: metav1.LabelSelectorOperator("Superset"), Values: []string{"x"}},
		},
	}
	if got, ok := RenderSelector(sel); ok {
		t.Fatalf("unknown operator must refuse (got %q), never fall back to a broad filter", got)
	}
}

func TestRenderSelector_NilAndEmpty_Refuse(t *testing.T) {
	// A nil selector and a selector with zero terms are unrepresentable: refuse rather
	// than enumerate the whole namespace. (A workload with a genuinely empty selector is
	// a degenerate case the tool surfaces as unresolved_identity, never as presence.)
	if _, ok := RenderSelector(nil); ok {
		t.Fatalf("nil selector must refuse")
	}
	if _, ok := RenderSelector(&metav1.LabelSelector{}); ok {
		t.Fatalf("zero-term selector must refuse")
	}
}
