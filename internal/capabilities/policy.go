package capabilities

// LocalPolicy is the customer-authored configuration a capability consults before it reads
// anything. It may only narrow: every field here can refuse a read or lower a bound, and
// none of them can permit a read the control plane did not ask for or raise a bound the
// control plane set. That asymmetry is the point — a server-side change must never be able
// to increase what leaves a customer's cluster.
type LocalPolicy struct {
	// AllowedNamespaces, when non-empty, is the complete set of namespaces any capability
	// may read. Empty means the operator has not narrowed it and the Relay's own RBAC is
	// the boundary, which is where it was before this existed.
	AllowedNamespaces map[string]bool
}

// AllowsNamespace reports whether policy permits reading this namespace.
func (p LocalPolicy) AllowsNamespace(namespace string) bool {
	if len(p.AllowedNamespaces) == 0 {
		return true
	}
	return p.AllowedNamespaces[namespace]
}

// Lower returns the effective value of a bound: the dispatched value clamped into the
// capability's compiled range, then lowered by the operator's local cap.
//
// The order carries the guarantee and is the whole of why this is one function rather than
// four lines repeated. The compiled floor and ceiling apply to what the CONTROL PLANE
// asked for — a dispatched zero becomes the floor rather than an empty read, and a
// dispatched value above the ceiling is cut to it. The local cap is applied LAST and
// therefore always wins, so an operator who caps a read below the capability's own floor
// gets the cap they configured. Applying the floor after the cap would raise it, which is
// the one thing local policy must never do.
func Lower(dispatched uint32, localCap int64, minimum, maximum int64) int64 {
	effective := int64(dispatched)
	if effective < minimum {
		effective = minimum
	}
	if effective > maximum {
		effective = maximum
	}
	if localCap > 0 && localCap < effective {
		effective = localCap
	}
	return effective
}
