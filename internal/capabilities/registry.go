package capabilities

import (
	"context"
	"sort"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// Executor runs one capability job to a typed result. Declared here as well as in the
// session package because both consume it and neither owns it: the session needs something
// to hand an assignment to, and the registry needs something to route one to.
type Executor interface {
	Execute(ctx context.Context, job *relayv1.JobAssignment) *relayv1.JobResult
}

// registered is what a registry routes on. The version is half the key, because schema
// versions are frozen: a request for v2 is a request for semantics a build carrying only
// v1 does not have, and answering it with v1 would produce a result indistinguishable
// downstream from one produced under the right contract.
type registered struct {
	id      string
	version uint32
}

// Registry is the closed compiled capability set: what this Relay advertises, and what it
// routes an assignment to. It is closed by construction — there is no lookup by name in a
// table a message could add to, no dynamic loading, and no fall-through — which is the
// structural half of the guarantee that a Relay never becomes a remote shell.
type Registry struct {
	executors map[registered]Executor
}

// NewRegistry returns an empty registry. A Relay that registered nothing advertises
// nothing and refuses everything, which is the correct behaviour for a build with no
// compiled capabilities rather than a state worth guarding against.
func NewRegistry() *Registry {
	return &Registry{executors: make(map[registered]Executor)}
}

// Register adds one compiled capability. Registering the same identity twice replaces it;
// there is exactly one composition root, so a duplicate is a wiring mistake that a panic
// would turn into an outage and a replacement turns into a deterministic outcome.
func (r *Registry) Register(descriptor *relayv1.CapabilityDescriptor, executor Executor) {
	r.executors[registered{
		id:      descriptor.GetCapabilityId(),
		version: descriptor.GetCapabilityVersion(),
	}] = executor
}

// Descriptors is what the hello advertises, ordered so a relay's attestation is stable
// across restarts. An unstable order would make every reconnection look like a change to
// whatever compares the two.
func (r *Registry) Descriptors() []*relayv1.CapabilityDescriptor {
	descriptors := make([]*relayv1.CapabilityDescriptor, 0, len(r.executors))
	for key := range r.executors {
		descriptors = append(descriptors, &relayv1.CapabilityDescriptor{
			CapabilityId:      key.id,
			CapabilityVersion: key.version,
		})
	}
	sort.Slice(descriptors, func(i, j int) bool {
		if descriptors[i].GetCapabilityId() == descriptors[j].GetCapabilityId() {
			return descriptors[i].GetCapabilityVersion() < descriptors[j].GetCapabilityVersion()
		}
		return descriptors[i].GetCapabilityId() < descriptors[j].GetCapabilityId()
	})
	return descriptors
}

// Execute routes an assignment to the capability that implements it.
//
// The session refuses anything this Relay never advertised before an assignment reaches
// here, so a miss at this point means the advertised set and the registered set disagree —
// a wiring defect rather than a control plane asking for too much. It is answered with the
// same typed refusal either way, because executing whatever is to hand and reporting it
// under the assignment's capability is the one answer that would be worse than refusing.
func (r *Registry) Execute(ctx context.Context, job *relayv1.JobAssignment) *relayv1.JobResult {
	executor, ok := r.executors[registered{
		id:      job.GetCapabilityId(),
		version: job.GetCapabilityVersion(),
	}]
	if !ok {
		return Failure(job, relayv1.JobFailure_KIND_UNSUPPORTED_CAPABILITY_VERSION)
	}
	return executor.Execute(ctx, job)
}
