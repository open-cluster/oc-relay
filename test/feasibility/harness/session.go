package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	relayv1 "github.com/OCluster/opencluster-relay/gen/go/opencluster/relay/v1"
)

// session wraps one Connect stream with the single-sender discipline the
// architecture mandates: only the owning goroutine created by pump/probeAlive
// writes; this harness keeps all writes on one goroutine per phase by construction.
type session struct {
	stream           grpc.BidiStreamingClient[relayv1.RelayToControl, relayv1.ControlToRelay]
	sessionID        string
	establishLatency time.Duration
}

type pumpStats struct {
	assignments int
	acks        int
	rtts        []time.Duration
}

func openSession(
	ctx context.Context,
	conn *grpc.ClientConn,
	inFlight []*relayv1.InFlightJob,
) (*session, error) {
	md := metadata.Pairs("x-relay-credential", "feasibility-credential")
	start := time.Now()
	stream, err := relayv1.NewRelaySessionServiceClient(conn).
		Connect(metadata.NewOutgoingContext(ctx, md))
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}

	hello := &relayv1.RelayToControl{
		Message: &relayv1.RelayToControl_Hello{
			Hello: &relayv1.Hello{
				ProtocolVersion: 1,
				RelayVersion:    "0.0.0-feasibility",
				Capabilities: []*relayv1.CapabilityDescriptor{
					{CapabilityId: "kubernetes.workload.runtime", CapabilityVersion: 1},
				},
				ClusterFingerprint: "feas-fingerprint",
				LocalPolicyHash:    "feas-policy",
				MaxConcurrentJobs:  4,
				InFlight:           inFlight,
			},
		},
	}
	if err := stream.Send(hello); err != nil {
		return nil, fmt.Errorf("send hello: %w", err)
	}

	first, err := stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("recv session_accepted: %w", err)
	}
	accepted := first.GetSessionAccepted()
	if accepted == nil {
		return nil, fmt.Errorf("expected session_accepted, got %T", first.GetMessage())
	}

	return &session{
		stream:           stream,
		sessionID:        accepted.SessionId,
		establishLatency: time.Since(start),
	}, nil
}

// pump answers every assignment with started+result and collects assignment→ack
// round-trip times until the deadline elapses.
func (s *session) pump(ctx context.Context, duration time.Duration) (*pumpStats, error) {
	deadline := time.Now().Add(duration)
	stats := &pumpStats{}
	sentAt := map[string]time.Time{}

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		inbound, err := s.stream.Recv()
		if err != nil {
			return stats, fmt.Errorf("recv: %w", err)
		}

		switch m := inbound.GetMessage().(type) {
		case *relayv1.ControlToRelay_JobAssignment:
			stats.assignments++
			job := m.JobAssignment
			if job.OrgId != "org-feas" || job.RegistrationId != "reg-feas" {
				return stats, errors.New("identity mismatch on assignment")
			}
			sentAt[job.JobId] = time.Now()
			result := &relayv1.RelayToControl{
				Message: &relayv1.RelayToControl_JobResult{
					JobResult: &relayv1.JobResult{
						JobId:      job.JobId,
						LeaseEpoch: job.LeaseEpoch,
						Outcome: &relayv1.JobResult_Result{
							Result: &relayv1.CapabilityResult{
								Result: &relayv1.CapabilityResult_KubernetesWorkloadRuntimeV1{
									KubernetesWorkloadRuntimeV1: &relayv1.KubernetesWorkloadRuntimeResultV1{
										Outcome:          relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_SUCCESS,
										ReturnedPodCount: 0,
										Complete:         true,
										AppliedMaxPods:   job.GetArguments().GetKubernetesWorkloadRuntimeV1().GetMaxPods(),
									},
								},
							},
						},
						ExecutionDurationMs: 1,
					},
				},
			}
			if err := s.stream.Send(result); err != nil {
				return stats, fmt.Errorf("send result: %w", err)
			}

		case *relayv1.ControlToRelay_ResultAck:
			stats.acks++
			if at, ok := sentAt[m.ResultAck.JobId]; ok {
				stats.rtts = append(stats.rtts, time.Since(at))
				delete(sentAt, m.ResultAck.JobId)
			}
		}
	}
	return stats, nil
}

// probeAlive proves the stream still works end-to-end by sending a result for a
// synthetic job and waiting for its ack.
func (s *session) probeAlive(ctx context.Context) error {
	probe := &relayv1.RelayToControl{
		Message: &relayv1.RelayToControl_JobResult{
			JobResult: &relayv1.JobResult{
				JobId:      "job-idle-probe",
				LeaseEpoch: 1,
				Outcome: &relayv1.JobResult_Failure{
					Failure: &relayv1.JobFailure{
						Kind: relayv1.JobFailure_KIND_CANCELLED,
					},
				},
			},
		},
	}
	if err := s.stream.Send(probe); err != nil {
		return fmt.Errorf("send probe: %w", err)
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		inbound, err := s.stream.Recv()
		if err != nil {
			return fmt.Errorf("recv probe ack: %w", err)
		}
		if ack := inbound.GetResultAck(); ack != nil && ack.JobId == "job-idle-probe" {
			return nil
		}
	}
}

func (p *pumpStats) print() {
	fmt.Printf("delivery: %d assignments, %d acks\n", p.assignments, p.acks)
	if len(p.rtts) > 0 {
		fmt.Printf("rtt (result→ack observed at assignment grain): p50=%s p95=%s max=%s over %d samples\n",
			percentile(p.rtts, 0.50).Round(time.Millisecond),
			percentile(p.rtts, 0.95).Round(time.Millisecond),
			percentile(p.rtts, 1.0).Round(time.Millisecond),
			len(p.rtts))
	}
}
