// Package inventory is the Relay's change detection: a local schedule that lists a
// scope's declared intent, diffs it against the previous tick, and queues deltas for
// the session to push. It exists because change history decays at the source and can
// only be recovered if somebody was recording — and it is deliberately NOT the job
// path: nothing here is leased or fenced, a lost message is recovered by
// resend-until-ack, and a lost window is recovered by a fresh baseline that the control
// plane records as a coverage boundary rather than silence.
package inventory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-relay/internal/kube"
)

// Reader is the bounded cluster surface this package detects changes through. Declared
// here because this package is its consumer; kube.InventoryLister implements it.
type Reader interface {
	ListWorkloadIntents(ctx context.Context, namespace string, limit int64) (*kube.InventoryPage, error)
	ListConfigMapVersions(ctx context.Context, namespace string, limit int64) (*kube.ConfigVersionPage, error)
	ListSecretVersions(ctx context.Context, namespace string, limit int64) (*kube.ConfigVersionPage, error)
}

const (
	// listLimit bounds every LIST a tick issues. One page only: a scope larger than
	// this reports truncated status rather than paginating, because the cost ceiling
	// of a tick must be a constant, not a function of cluster size.
	listLimit = 500
	// maxChangesPerDelta bounds one message. A wave larger than this — a mass
	// redeploy, a baseline of a big scope — is chunked into several deltas rather
	// than dropped or unbounded.
	maxChangesPerDelta = 512
	// maxPendingDeltas bounds the unacked buffer. Overflow — a control plane that
	// has not acknowledged for many ticks — clears the queue and schedules a fresh
	// baseline: the at-least-once contract is kept by re-observing, not by growing
	// memory without bound.
	maxPendingDeltas = 64
	// tickTimeout bounds one tick's cluster reads, so a hung API server degrades
	// into a faulted scope status instead of a stuck schedule.
	tickTimeout = 30 * time.Second
)

// scopeState is one Connection's synchronization state. Config fields are replaced
// under the synchronizer's lock; configVersion lets a tick that ran against a stale
// config discard its result instead of writing a diff across two different scopes.
type scopeState struct {
	connectionID   string
	policyRevision uint64
	interval       time.Duration
	namespaces     []string
	configVersion  int

	snapshot      Snapshot
	baselined     bool
	rebaseline    bool
	nextDue       time.Time
	lastCompleted time.Time
	faulted       bool
	truncated     bool
	pending       []*relayv1.InventoryDelta
}

// Synchronizer owns every scope's schedule, snapshot and pending deltas. It runs
// detection independently of the session: a disconnected Relay keeps observing, queues
// what it saw, and the session flushes the queue when the stream is back.
type Synchronizer struct {
	reader Reader
	local  Local
	// allowedNamespaces is the process-wide read allowlist; inventory must not reach
	// further than any capability may.
	allowedNamespaces map[string]bool
	now               func() time.Time

	mu     sync.Mutex
	scopes map[string]*scopeState
}

// NewSynchronizer builds one. now is injectable for tests; nil means the wall clock.
func NewSynchronizer(
	reader Reader, local Local, allowedNamespaces map[string]bool, now func() time.Time,
) *Synchronizer {
	if now == nil {
		now = time.Now
	}
	return &Synchronizer{
		reader:            reader,
		local:             local,
		allowedNamespaces: allowedNamespaces,
		now:               now,
		scopes:            make(map[string]*scopeState),
	}
}

// ApplyPolicy adopts a control-plane request for one Connection, constrained by local
// configuration: namespaces intersected, interval floored. A scope whose effective
// namespaces changed is re-baselined, because a diff across two different boundaries
// would report the difference in boundary as changes in the cluster.
func (s *Synchronizer) ApplyPolicy(policy *relayv1.InventorySynchronizationPolicy) {
	if !s.local.Enabled || policy.GetConnectionId() == "" {
		return
	}
	namespaces := s.effectiveNamespaces(policy.GetNamespaces())
	interval := policy.GetRequestedInterval().AsDuration()
	if interval < s.local.MinimumInterval {
		interval = s.local.MinimumInterval
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	scope, exists := s.scopes[policy.GetConnectionId()]
	if !exists {
		scope = &scopeState{connectionID: policy.GetConnectionId()}
		s.scopes[policy.GetConnectionId()] = scope
	}
	// The control plane re-sends policies to a live session on a cadence, so an UNCHANGED
	// policy must change nothing here — least of all the schedule, which a reset on every
	// refresh would collapse to the refresh cadence.
	if exists && scope.policyRevision == policy.GetRevision() &&
		scope.interval == interval && equalNamespaces(scope.namespaces, namespaces) {
		return
	}
	if exists && !equalNamespaces(scope.namespaces, namespaces) {
		scope.rebaseline = true
	}
	scope.policyRevision = policy.GetRevision()
	scope.interval = interval
	scope.namespaces = namespaces
	scope.configVersion++
	scope.nextDue = s.now()
}

// Ack drops one delivered delta from the pending queue.
func (s *Synchronizer) Ack(deltaID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, scope := range s.scopes {
		for i, delta := range scope.pending {
			if delta.GetDeltaId() == deltaID {
				scope.pending = append(scope.pending[:i], scope.pending[i+1:]...)
				return
			}
		}
	}
}

// Pending returns every queued delta, oldest first per scope, for the session's resend
// loop. The returned slice is a copy; the deltas themselves are immutable once queued.
func (s *Synchronizer) Pending() []*relayv1.InventoryDelta {
	s.mu.Lock()
	defer s.mu.Unlock()
	var pending []*relayv1.InventoryDelta
	for _, connectionID := range s.sortedScopeIDs() {
		pending = append(pending, s.scopes[connectionID].pending...)
	}
	return pending
}

// Statuses reports every scope's freshness for the heartbeat: last completed tick,
// faulted, truncated. A scope that has never completed carries no stamp, which the
// control plane reads as "not yet confirmed" rather than as current.
func (s *Synchronizer) Statuses() []*relayv1.InventoryScopeStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	var statuses []*relayv1.InventoryScopeStatus
	for _, connectionID := range s.sortedScopeIDs() {
		scope := s.scopes[connectionID]
		status := &relayv1.InventoryScopeStatus{
			ConnectionId:   scope.connectionID,
			PolicyRevision: scope.policyRevision,
			Faulted:        scope.faulted,
			Truncated:      scope.truncated,
		}
		if !scope.lastCompleted.IsZero() {
			status.LastCompletedAt = timestamppb.New(scope.lastCompleted)
		}
		statuses = append(statuses, status)
	}
	return statuses
}

// Run drives the schedule until the context ends. Detection deliberately outlives any
// one connection: what is observed while offline is queued and flushed later.
func (s *Synchronizer) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.RunDue(ctx)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// RunDue ticks every scope whose interval has elapsed. Serial on purpose: one cluster
// answers these lists, and two concurrent ticks would just contend for it.
func (s *Synchronizer) RunDue(ctx context.Context) {
	s.mu.Lock()
	due := make([]*scopeState, 0, len(s.scopes))
	now := s.now()
	for _, connectionID := range s.sortedScopeIDs() {
		scope := s.scopes[connectionID]
		if !scope.nextDue.After(now) {
			scope.nextDue = now.Add(scope.interval)
			due = append(due, scope)
		}
	}
	s.mu.Unlock()

	for _, scope := range due {
		s.tick(ctx, scope)
	}
}

// tick observes one scope and queues what changed. The cluster reads run without the
// lock — a slow API server must not block Statuses under a heartbeat — and the result
// is discarded if the scope's configuration moved while they ran.
func (s *Synchronizer) tick(ctx context.Context, scope *scopeState) {
	s.mu.Lock()
	namespaces := scope.namespaces
	configVersion := scope.configVersion
	previous := scope.snapshot
	baseline := !scope.baselined || scope.rebaseline
	revision := scope.policyRevision
	s.mu.Unlock()

	readCtx, cancel := context.WithTimeout(ctx, tickTimeout)
	defer cancel()
	observedAt := s.now()
	current, truncated, err := s.collect(readCtx, namespaces)

	s.mu.Lock()
	defer s.mu.Unlock()
	if scope.configVersion != configVersion {
		return
	}
	if err != nil {
		scope.faulted = true
		return
	}

	var changes []*relayv1.InventoryObjectChange
	if baseline {
		changes = Baseline(current)
	} else {
		changes = Diff(previous, current)
	}
	for _, delta := range packDeltas(scope.connectionID, revision, baseline, observedAt, changes) {
		scope.pending = append(scope.pending, delta)
	}
	// Overflow: the control plane has not acknowledged for many ticks. Everything
	// queued is superseded by a fresh baseline, which the control plane records as a
	// coverage discontinuity — the honest statement of what an unflushed backlog is.
	if len(scope.pending) > maxPendingDeltas {
		scope.pending = nil
		scope.rebaseline = true
	} else {
		scope.rebaseline = false
	}
	scope.snapshot = current
	scope.baselined = true
	scope.faulted = false
	scope.truncated = truncated
	scope.lastCompleted = observedAt
}

// collect builds one snapshot of the scope: workload intents plus the versions of the
// ConfigMaps and Secrets those workloads reference. Referenced only — an unreferenced
// ConfigMap's changes are not context for any workload this scope watches.
func (s *Synchronizer) collect(
	ctx context.Context, namespaces []string,
) (Snapshot, bool, error) {
	snapshot := Snapshot{}
	truncated := false
	referencedConfigMaps := map[string]map[string]bool{}
	referencedSecrets := map[string]map[string]bool{}

	for _, namespace := range namespaces {
		workloads, err := s.reader.ListWorkloadIntents(ctx, namespace, listLimit)
		if err != nil {
			return nil, false, err
		}
		truncated = truncated || workloads.Truncated
		for _, intent := range workloads.Workloads {
			key, state := workloadState(intent)
			snapshot[key] = state
			noteReferences(referencedConfigMaps, intent.Namespace, intent.ConfigMapRefs)
			noteReferences(referencedSecrets, intent.Namespace, intent.SecretRefs)
		}
	}

	for _, namespace := range namespaces {
		configMaps, err := s.reader.ListConfigMapVersions(ctx, namespace, listLimit)
		if err != nil {
			return nil, false, err
		}
		truncated = truncated || configMaps.Truncated
		for _, version := range configMaps.Versions {
			if referencedConfigMaps[version.Namespace][version.Name] {
				key, state := configState(
					relayv1.InventoryObjectKind_INVENTORY_OBJECT_KIND_CONFIGMAP, version)
				snapshot[key] = state
			}
		}

		secrets, err := s.reader.ListSecretVersions(ctx, namespace, listLimit)
		if err != nil {
			return nil, false, err
		}
		truncated = truncated || secrets.Truncated
		for _, version := range secrets.Versions {
			if referencedSecrets[version.Namespace][version.Name] {
				key, state := configState(
					relayv1.InventoryObjectKind_INVENTORY_OBJECT_KIND_SECRET, version)
				snapshot[key] = state
			}
		}
	}
	return snapshot, truncated, nil
}

// effectiveNamespaces resolves what a tick actually lists: the control plane's request
// intersected with every local constraint. An empty request means "what local
// configuration allows"; unrestricted on both sides means one cluster-wide list, which
// is the empty namespace to client-go.
func (s *Synchronizer) effectiveNamespaces(requested []string) []string {
	if len(requested) > 0 {
		var effective []string
		for _, namespace := range requested {
			if s.namespaceAllowed(namespace) {
				effective = append(effective, namespace)
			}
		}
		sort.Strings(effective)
		return effective
	}
	if s.local.Namespaces != nil {
		var effective []string
		for namespace := range s.local.Namespaces {
			if s.allowedNamespaces == nil || s.allowedNamespaces[namespace] {
				effective = append(effective, namespace)
			}
		}
		sort.Strings(effective)
		return effective
	}
	if s.allowedNamespaces != nil {
		effective := make([]string, 0, len(s.allowedNamespaces))
		for namespace := range s.allowedNamespaces {
			effective = append(effective, namespace)
		}
		sort.Strings(effective)
		return effective
	}
	return []string{""}
}

func (s *Synchronizer) namespaceAllowed(namespace string) bool {
	if s.local.Namespaces != nil && !s.local.Namespaces[namespace] {
		return false
	}
	if s.allowedNamespaces != nil && !s.allowedNamespaces[namespace] {
		return false
	}
	return true
}

func (s *Synchronizer) sortedScopeIDs() []string {
	ids := make([]string, 0, len(s.scopes))
	for id := range s.scopes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// packDeltas chunks a change list into bounded messages. Every chunk of a baseline
// carries the baseline mark and the same observation time, so the control plane can
// treat them as one observation delivered in parts.
func packDeltas(
	connectionID string, revision uint64, baseline bool, observedAt time.Time,
	changes []*relayv1.InventoryObjectChange,
) []*relayv1.InventoryDelta {
	if len(changes) == 0 && !baseline {
		return nil
	}
	var deltas []*relayv1.InventoryDelta
	for first := true; first || len(changes) > 0; first = false {
		chunk := changes
		if len(chunk) > maxChangesPerDelta {
			chunk = chunk[:maxChangesPerDelta]
		}
		changes = changes[len(chunk):]
		deltas = append(deltas, &relayv1.InventoryDelta{
			DeltaId:        newDeltaID(),
			ConnectionId:   connectionID,
			PolicyRevision: revision,
			Baseline:       baseline,
			ObservedAt:     timestamppb.New(observedAt),
			Changes:        chunk,
		})
	}
	return deltas
}

// newDeltaID mints a random identifier. It carries no meaning — the server-side dedup
// key is derived from the changes themselves — it only lets an ack name what to stop
// resending.
func newDeltaID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		// Falling back to a constant would make every delta look acked by the first
		// ack. A failed system randomness source is a broken host; stop loudly.
		panic("system randomness unavailable: " + err.Error())
	}
	return hex.EncodeToString(raw)
}

func equalNamespaces(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func noteReferences(into map[string]map[string]bool, namespace string, names []string) {
	if len(names) == 0 {
		return
	}
	set, ok := into[namespace]
	if !ok {
		set = map[string]bool{}
		into[namespace] = set
	}
	for _, name := range names {
		set[name] = true
	}
}
