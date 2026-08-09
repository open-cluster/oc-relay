// Package logs implements kubernetes.container.logs v1: what one container said, bounded
// in lines, in bytes and in time.
//
// It is the read that carries the answer in most workload failures — the application's own
// account of what happened, and particularly the account of the container that DIED rather
// than the one that replaced it. It is also the most dangerous thing the product does, on
// two independent axes, and both are why this package is small and refuses more than it
// returns.
//
// Log content is written by software an attacker may control and is read by a model, so it
// is untrusted for its whole life and can never become an instruction. It is also where
// applications print secrets nobody meant to print, which is why Relay-side redaction gates
// this capability against any cluster holding real data: synthetic scenario clusters until
// that exists, and no design-partner installation before it.
//
// There is no follow, stream or tail-forever mode, and no field for one exists. That is
// structural rather than a default, because a read that can be held open is a channel out
// of a customer's cluster.
package logs

import (
	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// The capability's registered identity. The compiled schema version is frozen: a semantic
// change mints v2 messages and a new version integer, never an edit here.
const (
	CapabilityID      = "kubernetes.container.logs"
	CapabilityVersion = 1
)

// Descriptor advertises this compiled capability for enrollment and every session hello.
func Descriptor() *relayv1.CapabilityDescriptor {
	return &relayv1.CapabilityDescriptor{
		CapabilityId:      CapabilityID,
		CapabilityVersion: CapabilityVersion,
	}
}
