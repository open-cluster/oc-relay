// Package events implements kubernetes.namespace.events v1: what the cluster said it did
// in one namespace, in one window, bounded in count.
//
// It answers the question a workload read cannot. `kubernetes.workload.runtime` reports
// that a pod is in CrashLoopBackOff and has restarted eleven times; the scheduler's own
// account of why it could not place it, or the kubelet's account of why the image would
// not pull, lives here.
//
// Event messages are the one place this protocol admits free text a cluster wrote about
// itself. Controllers and admission webhooks write them, a customer or an attacker may run
// either, and a model reads the result — so the text is untrusted for its whole life,
// capped on the way out, and never an instruction.
package events

import (
	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// The capability's registered identity. The compiled schema version is frozen: a semantic
// change mints v2 messages and a new version integer, never an edit here.
const (
	CapabilityID      = "kubernetes.namespace.events"
	CapabilityVersion = 1
)

// Descriptor advertises this compiled capability for enrollment and every session hello.
func Descriptor() *relayv1.CapabilityDescriptor {
	return &relayv1.CapabilityDescriptor{
		CapabilityId:      CapabilityID,
		CapabilityVersion: CapabilityVersion,
	}
}
