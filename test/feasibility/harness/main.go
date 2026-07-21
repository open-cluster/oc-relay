// Transport feasibility harness: exercises the v1 session contract against a throwaway
// control-plane test server across edge topologies (direct TLS, h2c reverse proxy, HTTP
// CONNECT proxy, interruption, idle, certificate rotation, server restart).
//
// This is gate instrumentation, not the Relay: no durable identity, no capability
// execution, no local policy. It reuses the REAL generated contract types so the
// gate also proves cross-language contract fidelity.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"

	relayv1 "github.com/OCluster/opencluster-relay/gen/go/opencluster/relay/v1"
)

type config struct {
	scenario     string
	target       string
	useTLS       bool
	insecure     bool
	pins         []string
	proxy        string
	duration     time.Duration
	keepalive    time.Duration
	permitNoStrm bool
}

func main() {
	var cfg config
	var pins string
	flag.StringVar(&cfg.scenario, "scenario", "session", "session|idle|interrupt")
	flag.StringVar(&cfg.target, "target", "localhost:15443", "host:port")
	flag.BoolVar(&cfg.useTLS, "tls", true, "dial with TLS (false = h2c)")
	flag.BoolVar(&cfg.insecure, "insecure", false, "skip TLS verification (local-CA proxy scenario)")
	flag.StringVar(&pins, "pin", "", "comma-separated base64 SPKI pins (empty = no pinning)")
	flag.StringVar(&cfg.proxy, "proxy", "", "HTTP CONNECT proxy addr (sets HTTPS_PROXY)")
	flag.DurationVar(&cfg.duration, "duration", 15*time.Second, "scenario duration")
	flag.DurationVar(&cfg.keepalive, "keepalive", 60*time.Second, "client keepalive ping interval")
	flag.BoolVar(&cfg.permitNoStrm, "permit-without-stream", false, "send keepalive pings with no active RPC")
	flag.Parse()
	if pins != "" {
		cfg.pins = splitNonEmpty(pins)
	}

	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
}

func run(cfg config) error {
	if cfg.proxy != "" {
		// grpc-go performs HTTP CONNECT through the standard proxy environment.
		os.Setenv("HTTPS_PROXY", "http://"+cfg.proxy)
		defer os.Unsetenv("HTTPS_PROXY")
	}

	conn, err := dial(cfg)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	switch cfg.scenario {
	case "session":
		return runSession(cfg, conn)
	case "idle":
		return runIdle(cfg, conn)
	case "interrupt":
		return runInterrupt(cfg, conn)
	default:
		return fmt.Errorf("unknown scenario %q", cfg.scenario)
	}
}

func dial(cfg config) (*grpc.ClientConn, error) {
	opts := []grpc.DialOption{
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			// Tunable so the gate can measure the keepalive-vs-middlebox-idle
			// relationship that sets the production interval.
			Time:                cfg.keepalive,
			Timeout:             20 * time.Second,
			PermitWithoutStream: cfg.permitNoStrm,
		}),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(4*1024*1024),
			grpc.MaxCallSendMsgSize(4*1024*1024),
		),
	}
	if !cfg.useTLS {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
		return grpc.NewClient(cfg.target, opts...)
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.insecure || len(cfg.pins) > 0 {
		// Pinning replaces chain validation for self-signed harness certs; the
		// production Relay pins IN ADDITION to chain validation.
		tlsCfg.InsecureSkipVerify = true
	}
	if len(cfg.pins) > 0 {
		pinSet := map[string]bool{}
		for _, p := range cfg.pins {
			pinSet[p] = true
		}
		tlsCfg.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no peer certificate")
			}
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return fmt.Errorf("parse peer certificate: %w", err)
			}
			sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
			pin := base64.StdEncoding.EncodeToString(sum[:])
			if !pinSet[pin] {
				return fmt.Errorf("SPKI pin mismatch: peer %s not in pin set", pin)
			}
			return nil
		}
	}
	opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	return grpc.NewClient(cfg.target, opts...)
}

// runSession: register, connect, exchange assignments/results for the duration;
// report session-establishment time, delivery counts, and RTT percentiles. A
// working bidi stream is itself the HTTP/2-preservation proof: gRPC cannot ride a
// downgraded hop.
func runSession(cfg config, conn *grpc.ClientConn) error {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.duration+20*time.Second)
	defer cancel()

	regStart := time.Now()
	reg, err := register(ctx, conn)
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	fmt.Printf("register: ok in %s (relay=%s org=%s)\n",
		time.Since(regStart).Round(time.Millisecond), reg.RelayId, reg.OrgId)

	s, err := openSession(ctx, conn, nil)
	if err != nil {
		return err
	}
	fmt.Printf("session: accepted in %s (id=%s)\n",
		s.establishLatency.Round(time.Millisecond), s.sessionID)

	stats, err := s.pump(ctx, cfg.duration)
	if err != nil {
		return err
	}
	stats.print()
	if stats.assignments == 0 || stats.acks == 0 {
		return errors.New("no assignments or acks delivered")
	}
	return nil
}

// runIdle: establish the session, stay silent for the duration (server in quiet
// mode), then prove the stream is still alive by sending a result and receiving its
// ack. Detects idle-timeout kills by proxies/middleboxes.
func runIdle(cfg config, conn *grpc.ClientConn) error {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.duration+30*time.Second)
	defer cancel()

	s, err := openSession(ctx, conn, nil)
	if err != nil {
		return err
	}
	fmt.Printf("session: accepted (id=%s); idling %s...\n", s.sessionID, cfg.duration)
	time.Sleep(cfg.duration)

	aliveStart := time.Now()
	if err := s.probeAlive(ctx); err != nil {
		return fmt.Errorf("stream dead after idle window: %w", err)
	}
	fmt.Printf("idle: stream ALIVE after %s (probe RTT %s)\n",
		cfg.duration, time.Since(aliveStart).Round(time.Millisecond))
	return nil
}

// runInterrupt: run a session through the controllable CONNECT proxy, have the
// proxy drop every tunnel mid-stream, then reconnect and verify delivery resumes.
// Reports detection time and reconnect-to-first-delivery time, and carries an
// in-flight roster on the second hello (the production reconciliation shape).
func runInterrupt(cfg config, conn *grpc.ClientConn) error {
	if cfg.proxy == "" {
		return errors.New("interrupt scenario requires -proxy")
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.duration+60*time.Second)
	defer cancel()

	s, err := openSession(ctx, conn, nil)
	if err != nil {
		return err
	}
	fmt.Printf("session: accepted (id=%s); phase 1 running...\n", s.sessionID)

	_, pumpErr := s.pump(ctx, cfg.duration)
	if pumpErr == nil {
		return errors.New("expected the proxy to break the stream, but pump completed cleanly")
	}
	detected := time.Now()
	fmt.Printf("interrupt: stream failure detected (%v)\n", pumpErr)

	roster := []*relayv1.InFlightJob{{JobId: "job-inflight-sim", LeaseEpoch: 1, ElapsedMs: 1500}}
	var s2 *session
	for attempt := 1; ; attempt++ {
		s2, err = openSession(ctx, conn, roster)
		if err == nil {
			break
		}
		if attempt >= 20 {
			return fmt.Errorf("reconnect failed after %d attempts: %w", attempt, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Printf("reconnect: new session %s established %s after failure detection\n",
		s2.sessionID, time.Since(detected).Round(time.Millisecond))

	stats, err := s2.pump(ctx, 5*time.Second)
	if err != nil {
		return fmt.Errorf("post-reconnect pump: %w", err)
	}
	stats.print()
	if stats.assignments == 0 {
		return errors.New("no delivery after reconnect")
	}
	fmt.Println("interrupt: delivery RESUMED after reconnect")
	return nil
}

func register(ctx context.Context, conn *grpc.ClientConn) (*relayv1.RegisterResponse, error) {
	md := metadata.Pairs("x-bootstrap-token", "feasibility-token")
	return relayv1.NewRelayRegistrationServiceClient(conn).Register(
		metadata.NewOutgoingContext(ctx, md),
		&relayv1.RegisterRequest{
			ProtocolVersion:    1,
			RelayVersion:       "0.0.0-feasibility",
			ClusterFingerprint: "feas-fingerprint",
			Capabilities: []*relayv1.CapabilityDescriptor{
				{CapabilityId: "kubernetes.runtime", CapabilityVersion: 1},
			},
		})
}

func percentile(durations []time.Duration, p float64) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}

func splitNonEmpty(commaSeparated string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(commaSeparated); i++ {
		if i == len(commaSeparated) || commaSeparated[i] == ',' {
			if i > start {
				out = append(out, commaSeparated[start:i])
			}
			start = i + 1
		}
	}
	return out
}
