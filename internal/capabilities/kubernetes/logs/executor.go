package logs

import (
	"context"
	"errors"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"

	"github.com/open-cluster/oc-relay/internal/capabilities"
	"github.com/open-cluster/oc-relay/internal/kube"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// Log bounds. Both are enforced and neither substitutes for the other: a line cap alone is
// defeated by one very long line, and a byte cap alone makes the result unpredictable in
// shape. The customer's local caps may lower either and can never raise one.
const (
	MinLogLines = 10
	MaxLogLines = 2000
	MinLogBytes = 4 * 1024
	MaxLogBytes = 256 * 1024
)

// How much this Relay may FETCH relative to what it may EMIT, and the absolute ceiling on
// that fetch.
//
// The overdraft exists so the newest lines survive a byte bound. The Kubernetes API seeks
// to the tail and then streams FORWARD, so a byte limit passed straight through cuts the
// end of the log — which is the part that explains a crash. Fetching a multiple and
// keeping the newest that fit inverts that. Nothing beyond the emitted bound ever leaves
// the cluster; this bounds memory and network, not disclosure.
const (
	readOverdraft  = 4
	maxReadCeiling = 4 * 1024 * 1024
)

// Options is the executor's customer-authored configuration. Every field may narrow a read
// and none of them may widen one.
type Options struct {
	Policy        capabilities.LocalPolicy
	LocalMaxLines int64
	LocalMaxBytes int64
}

// Executor runs the kubernetes.container.logs v1 capability through the read-only kube
// port.
//
// It resolves the container from the POD rather than from the log endpoint's error
// message. The API answers a wrong container name and a container with no previous
// instance with the same status and different free text, and a taxonomy decided by string
// matching silently reclassifies itself the first time a Kubernetes release rewords one of
// them. Reading the pod costs one call and makes both answers structural.
type Executor struct {
	reader  kube.LogReader
	options Options
}

// NewExecutor builds the capability executor.
func NewExecutor(reader kube.LogReader, options Options) *Executor {
	return &Executor{reader: reader, options: options}
}

// Execute runs one container-logs job to a typed result.
func (e *Executor) Execute(ctx context.Context, job *relayv1.JobAssignment) *relayv1.JobResult {
	args := job.GetArguments().GetKubernetesContainerLogsV1()
	if !validArgs(args) {
		return capabilities.Failure(job, relayv1.JobFailure_KIND_ARGUMENTS_REJECTED)
	}
	if !e.options.Policy.AllowsNamespace(args.GetNamespace()) {
		return capabilities.Failure(job, relayv1.JobFailure_KIND_LOCAL_POLICY_REFUSED)
	}

	return capabilities.Run(ctx, job, func(execCtx context.Context) capabilities.Reading {
		result := e.read(execCtx, args)
		return capabilities.Reading{
			Result: &relayv1.CapabilityResult{
				Result: &relayv1.CapabilityResult_KubernetesContainerLogsV1{
					KubernetesContainerLogsV1: result,
				},
			},
			Reached: result.GetOutcome() != relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_UNREACHABLE,
		}
	})
}

// read resolves the container, performs the bounded log read, and assembles the result
// under the closed outcome taxonomy.
func (e *Executor) read(
	ctx context.Context, args *relayv1.KubernetesContainerLogsArgsV1,
) *relayv1.KubernetesContainerLogsResultV1 {
	lineBound := capabilities.Lower(args.GetMaxLines(), e.options.LocalMaxLines, MinLogLines, MaxLogLines)
	byteBound := capabilities.Lower(args.GetMaxBytes(), e.options.LocalMaxBytes, MinLogBytes, MaxLogBytes)

	// The effective bounds travel whatever the outcome, so a reader never has to know
	// whether the read succeeded in order to know what it was allowed to return.
	basis := &relayv1.KubernetesContainerLogsResultV1{
		AppliedMaxLines: uint32(lineBound),
		AppliedMaxBytes: uint32(byteBound),
	}

	pod, err := e.reader.GetPod(ctx, args.GetNamespace(), args.GetPodName())
	if err != nil {
		basis.Outcome = wireOutcome(outcomeOf(err))
		return basis
	}
	if outcome, ok := resolveContainer(pod, args.GetContainerName(), args.GetPrevious()); !ok {
		basis.Outcome = outcome
		return basis
	}

	read, err := e.reader.ContainerLogs(ctx, kube.LogRequest{
		Namespace: args.GetNamespace(),
		Pod:       args.GetPodName(),
		Container: args.GetContainerName(),
		Previous:  args.GetPrevious(),
		// One line past the bound, so a log of exactly the bound's length is
		// distinguishable from one that was cut at it. A completeness basis that cannot
		// tell those apart is not one.
		TailLines:   lineBound + 1,
		ReadCeiling: fetchCeiling(byteBound),
		Since:       sinceFrom(args.GetSinceTime()),
	})
	if err != nil {
		basis.Outcome = wireOutcome(outcomeOf(err))
		return basis
	}

	bounded := applyBounds(parseLines(read), read.Truncated, lineBound, byteBound)

	basis.Outcome = relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_SUCCESS
	basis.Lines = bounded.lines
	basis.ReturnedLineCount = int32(len(bounded.lines))
	basis.ReturnedByteCount = bounded.returnedBytes
	basis.TruncatedByLineBound = bounded.byLines
	basis.TruncatedByByteBound = bounded.byBytes
	basis.WithheldByteCount = bounded.withheldBytes
	basis.Complete = !bounded.byLines && !bounded.byBytes
	basis.ReadAt = timestamppb.Now()
	return basis
}

// fetchCeiling is how many bytes this read may pull in, bounded absolutely so a pathological
// log cannot turn one job into the Relay's memory problem.
func fetchCeiling(byteBound int64) int64 {
	ceiling := byteBound * readOverdraft
	if ceiling > maxReadCeiling {
		ceiling = maxReadCeiling
	}
	return ceiling
}

func sinceFrom(since *timestamppb.Timestamp) time.Time {
	if since == nil {
		return time.Time{}
	}
	return since.AsTime()
}

// resolveContainer decides, from the pod alone, whether this read can proceed. The two
// failures it can report are deliberately different values: a name that is not on the pod
// is a planning mistake, and a container that has never died is a fact about the workload,
// and an investigator told the wrong one looks in the wrong place.
func resolveContainer(
	pod *corev1.Pod, container string, previous bool,
) (relayv1.KubernetesLogsOutcome, bool) {
	if !declaresContainer(pod, container) {
		return relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_CONTAINER_NOT_FOUND, false
	}
	if previous && !hasPreviousInstance(pod, container) {
		return relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_PREVIOUS_CONTAINER_NOT_FOUND, false
	}
	return relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_SUCCESS, true
}

// declaresContainer reports whether the pod has a container by this name. Init and
// ephemeral containers count: an init container that failed is a very common reason a pod
// never started, and excluding it would make the most useful read the one that is refused.
func declaresContainer(pod *corev1.Pod, name string) bool {
	for _, container := range pod.Spec.Containers {
		if container.Name == name {
			return true
		}
	}
	for _, container := range pod.Spec.InitContainers {
		if container.Name == name {
			return true
		}
	}
	for _, container := range pod.Spec.EphemeralContainers {
		if container.Name == name {
			return true
		}
	}
	return false
}

// hasPreviousInstance reports whether a terminated instance of this container exists to be
// read. The status's last termination is the authority: a restart count alone would say a
// container had restarted without saying the kubelet still holds its output.
func hasPreviousInstance(pod *corev1.Pod, name string) bool {
	for _, statuses := range [][]corev1.ContainerStatus{
		pod.Status.ContainerStatuses,
		pod.Status.InitContainerStatuses,
		pod.Status.EphemeralContainerStatuses,
	} {
		for _, status := range statuses {
			if status.Name == name {
				return status.LastTerminationState.Terminated != nil
			}
		}
	}
	return false
}

// line is one parsed log line, kept with its raw content length so the byte accounting
// describes what the container wrote rather than what capping left of it.
type line struct {
	at      time.Time
	content string
	bytes   int64
}

// parseLines splits the read into lines and separates each line's timestamp from its
// content.
//
// The trailing element needs care in both directions. Split always produces one after the
// final newline, and it is empty exactly when the read ended on a line boundary. A
// NON-empty trailing element from a truncated fetch is a line cut mid-sentence and is
// dropped, because reporting a fragment as something the container said puts half a
// sentence into evidence. A non-empty trailing element from a complete read is a real
// final line the container wrote without a newline, and dropping it would silently lose
// the last thing a dying process managed to say — which is the most valuable line there is.
func parseLines(read *kube.LogRead) []line {
	split := strings.Split(string(read.Raw), "\n")
	if last := len(split) - 1; last >= 0 && split[last] == "" {
		split = split[:last]
	} else if last >= 0 && read.Truncated {
		split = split[:last]
	}

	parsed := make([]line, 0, len(split))
	for _, entry := range split {
		entry = strings.TrimSuffix(entry, "\r")
		if entry == "" {
			continue
		}
		at, content := splitTimestamp(entry)
		parsed = append(parsed, line{at: at, content: content, bytes: int64(len(content))})
	}
	return parsed
}

// splitTimestamp separates the RFC3339 prefix the API adds from the content. A prefix that
// does not parse is not a prefix: the whole entry is content and the line carries no time,
// because substituting this Relay's clock would present a guess as provenance.
func splitTimestamp(entry string) (time.Time, string) {
	prefix, rest, found := strings.Cut(entry, " ")
	if !found {
		return time.Time{}, entry
	}
	at, err := time.Parse(time.RFC3339Nano, prefix)
	if err != nil {
		return time.Time{}, entry
	}
	return at, rest
}

// boundedLines is the result of applying both bounds, with the basis for reporting them.
type boundedLines struct {
	lines         []*relayv1.KubernetesLogLine
	returnedBytes int64
	withheldBytes int64
	byLines       bool
	byBytes       bool
}

// applyBounds keeps the NEWEST lines that fit both bounds and reports which bound bound.
//
// Newest rather than oldest, because a container's last words are what explain its death.
// Both bounds are applied independently: dropping to the line bound can leave a set still
// over the byte bound, and one line under the line bound can be over the byte bound on its
// own.
func applyBounds(parsed []line, fetchTruncated bool, lineBound, byteBound int64) boundedLines {
	bounded := boundedLines{
		// A fetch that stopped at its ceiling had more to give, so the read is byte-bound
		// whatever the counting below concludes.
		byBytes: fetchTruncated,
	}

	keep := parsed
	if int64(len(keep)) > lineBound {
		bounded.byLines = true
		dropped := keep[:int64(len(keep))-lineBound]
		keep = keep[int64(len(keep))-lineBound:]
		for _, entry := range dropped {
			bounded.withheldBytes += entry.bytes
		}
	}

	// Walk backwards from the newest, taking lines while they fit.
	var total int64
	first := len(keep)
	for index := len(keep) - 1; index >= 0; index-- {
		if total+keep[index].bytes > byteBound {
			bounded.byBytes = true
			break
		}
		total += keep[index].bytes
		first = index
	}
	for _, entry := range keep[:first] {
		bounded.withheldBytes += entry.bytes
	}

	kept := keep[first:]
	bounded.returnedBytes = total
	bounded.lines = make([]*relayv1.KubernetesLogLine, 0, len(kept))
	for _, entry := range kept {
		emitted := &relayv1.KubernetesLogLine{
			// Capped independently of the byte bound. The byte bound stops many lines from
			// being too much; this stops one line from being all of it.
			Content: capabilities.CapLogLine(entry.content),
		}
		if !entry.at.IsZero() {
			emitted.At = timestamppb.New(entry.at)
		}
		bounded.lines = append(bounded.lines, emitted)
	}
	return bounded
}

// validArgs re-validates on receipt. The container name is required: a pod's containers are
// not interchangeable, and defaulting to the first one reads whichever the spec happens to
// list first and reports it as the one that was asked for.
func validArgs(args *relayv1.KubernetesContainerLogsArgsV1) bool {
	return args != nil &&
		capabilities.ValidNamespace(args.GetNamespace()) &&
		capabilities.ValidObjectName(args.GetPodName()) &&
		capabilities.ValidContainerName(args.GetContainerName())
}

func outcomeOf(err error) kube.ReadOutcome {
	var readErr *kube.ReadError
	if errors.As(err, &readErr) {
		return readErr.Outcome
	}
	return kube.OutcomeUnreachable
}

func wireOutcome(outcome kube.ReadOutcome) relayv1.KubernetesLogsOutcome {
	switch outcome {
	case kube.OutcomeSuccess:
		return relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_SUCCESS
	case kube.OutcomeUnauthorized:
		return relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_UNAUTHORIZED
	case kube.OutcomePodNotFound, kube.OutcomeWorkloadNotFound, kube.OutcomeNamespaceNotFound:
		// Every not-found the port can report for a read addressed to one pod means the pod
		// is not there. It is a typed outcome and never absence evidence.
		return relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_POD_NOT_FOUND
	default:
		return relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_UNREACHABLE
	}
}
