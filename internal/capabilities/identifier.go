package capabilities

import "regexp"

// Kubernetes identifier shapes, validated on receipt by every capability that names an
// object. The control plane validates before dispatch and the Relay re-validates here;
// neither side trusts the other, which is the whole reason the check exists twice.
var (
	dns1123Label     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	dns1123Subdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	// A Kubernetes kind literal is an UpperCamelCase identifier ("Pod", "ReplicaSet").
	// Bounded and anchored so a kind can never carry a comma, an equals sign, or anything
	// else that would mean something to a field-selector encoder.
	kindLiteral = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	// A UID as Kubernetes mints it. Anchored for the same reason as the kind.
	uidLiteral = regexp.MustCompile(`^[a-zA-Z0-9-]+$`)
)

// Namespace bounds: a DNS-1123 label.
const maxNamespaceChars = 63

// ValidNamespace reports whether a namespace is one Kubernetes could have.
func ValidNamespace(value string) bool {
	return len(value) > 0 && len(value) <= maxNamespaceChars && dns1123Label.MatchString(value)
}

// ValidObjectName reports whether a name is one Kubernetes could have.
func ValidObjectName(value string) bool {
	return len(value) > 0 && len(value) <= MaxIdentifierChars && dns1123Subdomain.MatchString(value)
}

// ValidContainerName reports whether a container name is one Kubernetes could have.
// Container names are DNS-1123 labels, not subdomains.
func ValidContainerName(value string) bool {
	return len(value) > 0 && len(value) <= maxNamespaceChars && dns1123Label.MatchString(value)
}

// ValidKind reports whether a kind literal is one Kubernetes could have.
func ValidKind(value string) bool {
	return len(value) > 0 && len(value) <= maxNamespaceChars && kindLiteral.MatchString(value)
}

// ValidUID reports whether a UID is one Kubernetes could have.
func ValidUID(value string) bool {
	return len(value) > 0 && len(value) <= MaxIdentifierChars && uidLiteral.MatchString(value)
}
