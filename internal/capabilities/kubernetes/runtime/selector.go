// Package runtime implements the kubernetes.runtime.v1 capability: a read-only workload
// runtime projection matching the verified S1B-3 semantic contract. This file renders a
// workload's pod selector.
package runtime

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RenderSelector renders a workload's LabelSelector into the label-selector string used
// to LIST its pods, matching the S1B-3 contract exactly. It returns (rendered, true) when
// the selector is representable, and ("", false) when it is not — an unrepresentable
// selector (nil, zero terms, or an unknown operator) REFUSES enumeration rather than
// falling back to an empty filter that would list the whole namespace and misattribute
// unrelated pods (the S1B-3 exit-review bug fix). The caller maps a refusal to an
// unresolved_identity coverage gap and withholds all presence and absence evidence.
//
// Rendering order matches the oracle: matchLabels (key=value) first, then matchExpressions
// in source order — In as "key in (v1,v2)", NotIn as "key notin (v1,v2)", Exists as "key",
// DoesNotExist as "!key".
func RenderSelector(selector *metav1.LabelSelector) (string, bool) {
	if selector == nil {
		return "", false
	}

	terms := make([]string, 0, len(selector.MatchLabels)+len(selector.MatchExpressions))

	for key, value := range selector.MatchLabels {
		terms = append(terms, key+"="+value)
	}

	for _, req := range selector.MatchExpressions {
		term, ok := renderRequirement(req)
		if !ok {
			// Unknown operator: refuse the entire selector rather than list broadly.
			return "", false
		}
		terms = append(terms, term)
	}

	if len(terms) == 0 {
		// Zero terms would be an empty filter (whole-namespace list): refuse.
		return "", false
	}

	return strings.Join(terms, ","), true
}

func renderRequirement(req metav1.LabelSelectorRequirement) (string, bool) {
	switch req.Operator {
	case metav1.LabelSelectorOpIn:
		return req.Key + " in (" + strings.Join(req.Values, ",") + ")", true
	case metav1.LabelSelectorOpNotIn:
		return req.Key + " notin (" + strings.Join(req.Values, ",") + ")", true
	case metav1.LabelSelectorOpExists:
		return req.Key, true
	case metav1.LabelSelectorOpDoesNotExist:
		return "!" + req.Key, true
	default:
		return "", false
	}
}
