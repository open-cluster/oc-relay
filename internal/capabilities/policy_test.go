package capabilities

import "testing"

// The asymmetry these tests pin is the product's promise to an operator: a server-side
// change can never increase what leaves their cluster. Everything else about bound
// resolution is convenience; this is the guarantee.
func TestLower_LocalCapAlwaysWinsIncludingBelowTheCompiledFloor(t *testing.T) {
	const (
		floor   = 10
		ceiling = 200
	)
	for name, tc := range map[string]struct {
		dispatched uint32
		localCap   int64
		want       int64
	}{
		"no cap leaves the dispatched value alone":     {dispatched: 50, localCap: 0, want: 50},
		"a cap below the dispatched value lowers it":   {dispatched: 50, localCap: 20, want: 20},
		"a cap above the dispatched value raises none": {dispatched: 50, localCap: 500, want: 50},
		// The defect this ordering exists to prevent: a floor applied after the cap would
		// hand the operator a bound larger than the one they configured.
		"a cap below the compiled floor still wins":    {dispatched: 50, localCap: 3, want: 3},
		"a dispatched zero becomes the floor":          {dispatched: 0, localCap: 0, want: floor},
		"a dispatched value above the ceiling is cut":  {dispatched: 1_000_000, localCap: 0, want: ceiling},
		"a cap cannot be escaped by a huge dispatched": {dispatched: 1_000_000, localCap: 25, want: 25},
	} {
		if got := Lower(tc.dispatched, tc.localCap, floor, ceiling); got != tc.want {
			t.Errorf("%s: Lower(%d, %d) = %d, want %d", name, tc.dispatched, tc.localCap, got, tc.want)
		}
	}
}

func TestLocalPolicy_AnEmptyAllowlistNarrowsNothing(t *testing.T) {
	if !(LocalPolicy{}).AllowsNamespace("anything") {
		t.Fatal("an operator who configured no allowlist has not excluded every namespace")
	}
}

func TestLocalPolicy_AnAllowlistIsTheCompleteSet(t *testing.T) {
	policy := LocalPolicy{AllowedNamespaces: map[string]bool{"shop": true}}
	if !policy.AllowsNamespace("shop") {
		t.Fatal("a listed namespace must be allowed")
	}
	if policy.AllowsNamespace("payments") {
		t.Fatal("an allowlist is a complete set, not a set of hints")
	}
}
