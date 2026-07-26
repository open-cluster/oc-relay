// Package audit wraps capability execution with the Relay's local audit trail: one
// structured line per executed job recording what ran and how many bytes the result
// carries, so an operator can verify egress independently of the vendor. The line
// never includes argument or result payloads — it is an execution ledger, not a data
// copy.
package audit

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/OCluster/opencluster-relay/internal/session"

	relayv1 "github.com/OCluster/opencluster-relay/gen/go/opencluster/relay/v1"
)

// Executor decorates a session.Executor with execution measurement: it stamps the
// result's monotonic duration provenance and emits the local audit line.
type Executor struct {
	inner  session.Executor
	logger *slog.Logger
}

func NewExecutor(inner session.Executor, logger *slog.Logger) *Executor {
	return &Executor{inner: inner, logger: logger}
}

func (e *Executor) Execute(ctx context.Context, job *relayv1.JobAssignment) *relayv1.JobResult {
	start := time.Now()
	result := e.inner.Execute(ctx, job)
	duration := time.Since(start)

	if result != nil {
		result.ExecutionDurationMs = duration.Milliseconds()
	}

	e.logger.LogAttrs(ctx, slog.LevelInfo, "job executed",
		slog.String("job_id", job.GetJobId()),
		slog.String("capability",
			fmt.Sprintf("%s.v%d", job.GetCapabilityId(), job.GetCapabilityVersion())),
		slog.Uint64("lease_epoch", job.GetLeaseEpoch()),
		slog.String("outcome", outcomeLabel(result)),
		slog.Int("result_bytes", proto.Size(result)),
		slog.Int64("duration_ms", duration.Milliseconds()),
	)
	return result
}

// outcomeLabel reports "result" for a typed capability result and the failure kind's
// enum name otherwise — enough for the ledger without copying payload data.
func outcomeLabel(result *relayv1.JobResult) string {
	if result == nil {
		return "none"
	}
	if failure := result.GetFailure(); failure != nil {
		return failure.GetKind().String()
	}
	return "result"
}
