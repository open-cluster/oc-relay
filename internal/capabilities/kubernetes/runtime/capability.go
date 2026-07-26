package runtime

import (
	relayv1 "github.com/OCluster/opencluster-relay/gen/go/opencluster/relay/v1"
)

// The capability's registered identity. The compiled schema version is frozen: a
// semantic change mints v2 messages and a new version integer, never an edit here.
const (
	CapabilityID      = "kubernetes.workload.runtime"
	CapabilityVersion = 1
)

// Descriptor advertises this compiled capability for enrollment and every session
// hello.
func Descriptor() *relayv1.CapabilityDescriptor {
	return &relayv1.CapabilityDescriptor{
		CapabilityId:      CapabilityID,
		CapabilityVersion: CapabilityVersion,
	}
}
