// Command opencluster-relay is the Relay's composition root: it loads and validates
// configuration, initializes logging, loads (or bootstrap-enrolls) the durable
// identity, builds the read-only Kubernetes client, registers the compiled
// capabilities, and runs the control-plane session until the process is asked to
// stop. SIGTERM and SIGINT cancel the process context: the session supervisor exits
// cleanly and in-flight work is recovered by the control plane's leases — never
// silently completed on this side.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/open-cluster/oc-relay/internal/audit"
	"github.com/open-cluster/oc-relay/internal/capabilities"
	"github.com/open-cluster/oc-relay/internal/capabilities/kubernetes/events"
	"github.com/open-cluster/oc-relay/internal/capabilities/kubernetes/logs"
	"github.com/open-cluster/oc-relay/internal/capabilities/kubernetes/runtime"
	"github.com/open-cluster/oc-relay/internal/config"
	"github.com/open-cluster/oc-relay/internal/identity"
	"github.com/open-cluster/oc-relay/internal/kube"
	"github.com/open-cluster/oc-relay/internal/redaction"
	"github.com/open-cluster/oc-relay/internal/session"
	"github.com/open-cluster/oc-relay/internal/transport"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// version is stamped at release build time via -ldflags; "dev" otherwise.
var version = "dev"

const (
	protocolVersion = 1
	// startupTimeout bounds each startup read (fingerprint, enrollment) so a hung
	// endpoint degrades into a clear failure instead of a silent non-start.
	startupTimeout = 30 * time.Second
	// outboundBufferSize bounds the single-sender queue; producers block (observing
	// their contexts) when the control plane reads slowly.
	outboundBufferSize = 64
)

// options is what the command line says, as distinct from what the environment says. There is
// one of them, and it selects a local diagnostic mode rather than tuning the running Relay:
// process configuration stays in the environment, where it is one mechanism rather than two.
type options struct {
	// dryRunSample names a file of operator-supplied sample text to evaluate the redaction
	// policy against, "-" for standard input. It runs locally and exits: no control plane, no
	// cluster, no identity, no session.
	dryRunSample string
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	var chosen options
	flag.StringVar(&chosen.dryRunSample, "redaction-dry-run", "",
		"evaluate the redaction policy against sample text in this file (\"-\" for stdin), "+
			"report what would be masked, and exit")
	flag.Parse()

	if err := run(logger, chosen); err != nil {
		logger.Error("relay exiting", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(logger *slog.Logger, chosen options) error {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return err
	}

	// The redaction policy is loaded and reported before anything else happens, because a
	// fault in it changes what this process may do at all — and because an operator has to be
	// able to see what is actually in force rather than assume it.
	guard := redaction.Load(cfg.RedactionPolicyFile)
	guard.Describe(logger)

	// Evaluating a policy against sample text is a local operation and deliberately needs no
	// control plane, no cluster and no identity. A policy whose breadth is only discoverable
	// by losing an investigation is a policy nobody will tune, so this path exits before any
	// of that is built.
	if chosen.dryRunSample != "" {
		return dryRun(guard, chosen.dryRunSample, os.Stdout)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Identity first, from durable storage: loading the durable credential is a local
	// read with no cluster dependency, so a corrupt or missing credential fails fast on
	// its true (storage) root cause rather than being masked behind a later cluster-
	// reachability error, and the order matches the pinned composition sequence
	// (identity before the kube client). NOTE: this does NOT make startup resilient to a
	// transient Kubernetes API failure — the fingerprint read below is still required on
	// every start (the session Hello carries it) and still fatal; real resilience there
	// would need retry/backoff around that read, which is out of scope for this slice.
	store := identity.NewFileStore(cfg.CredentialFile)
	credential, err := identity.LoadOnStart(ctx, store)
	needEnroll := errors.Is(err, identity.ErrBootstrapRequired)
	if err != nil && !needEnroll {
		return err
	}

	// The read-only cluster client serves every capability and reads the cluster
	// fingerprint the session Hello carries (and enrollment attests).
	reader, err := newWorkloadReader(cfg)
	if err != nil {
		return err
	}
	fingerprint, err := clusterFingerprint(ctx, reader)
	if err != nil {
		return fmt.Errorf("reading cluster fingerprint: %w", err)
	}

	// Built before enrolment, because enrolment ATTESTS the same set the session advertises.
	// Two lists would be two lists to drift, and the drift is silent in the worst way: the
	// control plane would record at enrolment that this Relay serves one capability while the
	// session says it serves three, and an operator reading the roster would believe the
	// registration.
	registry := compiledCapabilities(reader, cfg)

	if needEnroll {
		if cfg.BootstrapTokenFile == "" {
			return fmt.Errorf(
				"%w and RELAY_BOOTSTRAP_TOKEN_FILE is not set", identity.ErrBootstrapRequired)
		}
		credential, err = enroll(ctx, cfg, fingerprint, registry.Descriptors(), store, logger)
		if err != nil {
			return err
		}
	}

	sessionCredentials, err := transportCredentials(cfg, credential.SPKIPins, logger)
	if err != nil {
		return err
	}
	connection, err := grpc.NewClient(cfg.ControlPlaneAddress,
		grpc.WithTransportCredentials(sessionCredentials))
	if err != nil {
		return fmt.Errorf("building control-plane client: %w", err)
	}
	defer func() {
		if closeErr := connection.Close(); closeErr != nil {
			logger.Warn("closing control-plane connection",
				slog.String("error", closeErr.Error()))
		}
	}()

	executor := guardedExecutor(registry, guard, logger)
	relaySession := session.New(session.Config{
		OrgID:              credential.OrgID,
		RegistrationID:     credential.RegistrationID,
		Capabilities:       registry.Descriptors(),
		MaxConcurrentJobs:  cfg.MaxConcurrentJobs,
		RelayVersion:       version,
		ClusterFingerprint: fingerprint,
		HeartbeatInterval:  cfg.HeartbeatInterval,
		ResendInterval:     cfg.ResendInterval,
		OutboundBuffer:     outboundBufferSize,
	}, executor)

	logger.Info("relay session starting",
		slog.String("registration_id", credential.RegistrationID),
		slog.String("relay_version", version))
	client := relayv1.NewRelaySessionServiceClient(connection)
	return relaySession.Run(ctx, session.NewGRPCConnector(client, credential))
}

// compiledCapabilities is the closed set this build can execute, and the same set it
// advertises. One registry serves both, so what the Relay refuses is exactly what it never
// claimed — there is no second list for the two to drift apart in.
//
// Every capability is handed the same customer-authored local policy. A namespace an
// operator excluded is excluded from every read, not from whichever ones remembered to
// check, which is the only form of that guarantee worth stating to them.
func compiledCapabilities(reader *kube.Reader, cfg config.Config) *capabilities.Registry {
	policy := capabilities.LocalPolicy{AllowedNamespaces: cfg.AllowedNamespaces}

	registry := capabilities.NewRegistry()
	registry.Register(runtime.Descriptor(), runtime.NewExecutor(reader, runtime.Options{
		Policy:       policy,
		LocalMaxPods: cfg.LocalMaxPods,
	}))
	registry.Register(events.Descriptor(), events.NewExecutor(reader, events.Options{
		Policy:           policy,
		LocalMaxEvents:   cfg.LocalMaxEvents,
		RetentionHorizon: cfg.EventRetention,
	}))
	registry.Register(logs.Descriptor(), logs.NewExecutor(reader, logs.Options{
		Policy:        policy,
		LocalMaxLines: cfg.LocalMaxLogLines,
		LocalMaxBytes: cfg.LocalMaxLogBytes,
	}))
	return registry
}

// guardedExecutor is the order the two decorators go in, and the order is the guarantee.
//
// Redaction sits INSIDE the audit decorator, so what audit measures and writes to the Relay's
// own log is the result AFTER masking. The Relay must not log what it just removed: diagnosing
// the Relay cannot be allowed to become the disclosure channel redaction exists to close, and
// an audit line reporting the unredacted size would leak the length of what was masked.
//
// It is a named function rather than one expression inline so that the ordering can be tested
// rather than only read. Getting these two the wrong way round is silent — everything still
// works, and the log quietly measures the wrong thing.
func guardedExecutor(
	inner session.Executor, guard *redaction.Guard, logger *slog.Logger,
) session.Executor {
	return audit.NewExecutor(guard.Enforce(inner), logger)
}

// dryRun reports what the effective policy would mask in operator-supplied sample text.
//
// A faulted policy refuses here exactly as it refuses a capability, rather than previewing the
// defaults. Showing an operator what the defaults would have done to text their own policy was
// meant to govern is the one answer that would actively mislead them.
func dryRun(guard *redaction.Guard, samplePath string, out io.Writer) error {
	if fault := guard.Fault(); fault != nil {
		return fault
	}

	var (
		sample []byte
		err    error
	)
	if samplePath == "-" {
		sample, err = io.ReadAll(os.Stdin)
	} else {
		sample, err = os.ReadFile(samplePath)
	}
	if err != nil {
		return fmt.Errorf("reading the sample text: %w", err)
	}

	preview := guard.Policy().DryRun(string(sample))
	_, err = io.WriteString(out, preview.Render(guard.Policy()))
	return err
}

// newWorkloadReader builds the read-only Kubernetes boundary. In-cluster
// ServiceAccount configuration is the production default; an explicit kubeconfig is
// the operator-configured harness path through the hardened loader.
func newWorkloadReader(cfg config.Config) (*kube.Reader, error) {
	restConfig, err := restConfigFor(cfg)
	if err != nil {
		return nil, err
	}
	restConfig.UserAgent = "opencluster-relay/" + version
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("building kubernetes client: %w", err)
	}
	return kube.NewReader(clientset), nil
}

func restConfigFor(cfg config.Config) (*rest.Config, error) {
	if cfg.KubeconfigPath != "" {
		restConfig, err := kube.LoadKubeconfig(cfg.KubeconfigPath)
		if err != nil {
			return nil, fmt.Errorf("loading kubeconfig: %w", err)
		}
		return restConfig, nil
	}
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster configuration unavailable "+
			"(set RELAY_KUBECONFIG for the self-hosted harness): %w", err)
	}
	return restConfig, nil
}

func clusterFingerprint(ctx context.Context, reader *kube.Reader) (string, error) {
	readCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	return reader.ClusterFingerprint(readCtx)
}

// A persisted credential always wins over a leftover bootstrap token file: enrollment
// is attempted only when LoadOnStart reports ErrBootstrapRequired, so an already-enrolled
// Relay never re-enrolls (see run).
func enroll(
	ctx context.Context, cfg config.Config, fingerprint string,
	capabilities []*relayv1.CapabilityDescriptor, store identity.Store, logger *slog.Logger,
) (identity.Credential, error) {
	token, err := readBootstrapToken(cfg.BootstrapTokenFile)
	if err != nil {
		return identity.Credential{}, err
	}

	bootstrapCredentials, err := transportCredentials(cfg, nil, logger)
	if err != nil {
		return identity.Credential{}, err
	}
	connection, err := grpc.NewClient(cfg.ControlPlaneAddress,
		grpc.WithTransportCredentials(bootstrapCredentials))
	if err != nil {
		return identity.Credential{}, fmt.Errorf("building registration client: %w", err)
	}
	defer connection.Close()

	enrollCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	credential, err := identity.Enroll(enrollCtx,
		relayv1.NewRelayRegistrationServiceClient(connection), token,
		identity.EnrollmentParams{
			OrgID:              cfg.OrgID,
			ProtocolVersion:    protocolVersion,
			RelayVersion:       version,
			ClusterFingerprint: fingerprint,
			Capabilities:       capabilities,
		}, store)
	if err != nil {
		return identity.Credential{}, err
	}
	logger.Info("bootstrap enrollment complete",
		slog.String("registration_id", credential.RegistrationID))
	return credential, nil
}

// readBootstrapToken reads the single-use token from the operator-supplied file. The
// token never transits an environment variable, an argument, or a log line.
func readBootstrapToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading bootstrap token file: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", errors.New("bootstrap token file is empty")
	}
	return token, nil
}

// transportCredentials picks the connection trust anchor: the credential's pin set
// when present, the operator's initial pins otherwise, and — only when neither exists
// — standard WebPKI verification, which is the documented bootstrap fallback for a
// control plane on a publicly-issued certificate.
func transportCredentials(
	cfg config.Config, credentialPins []string, logger *slog.Logger,
) (credentials.TransportCredentials, error) {
	serverName, _, err := net.SplitHostPort(cfg.ControlPlaneAddress)
	if err != nil {
		return nil, fmt.Errorf("deriving TLS server name: %w", err)
	}
	pins := credentialPins
	if len(pins) == 0 {
		pins = cfg.InitialSPKIPins
	}
	if len(pins) == 0 {
		// A control plane on a publicly-issued certificate with no configured pin set is
		// a supported deployment, so this is Warn (observable), not Error (which would page
		// on a correct, intentional posture). Pinning cannot be silently lost when the
		// operator asked for it: configured initial pins take precedence above, so this
		// path is reached only when NO pin material exists anywhere.
		logger.Warn("no SPKI pins configured; the control-plane link uses standard WebPKI "+
			"verification, not key pinning",
			slog.String("server", serverName))
		return transport.NewSystemCredentials(serverName), nil
	}
	return transport.NewPinnedCredentials(serverName, pins)
}
