// Package config loads and validates the Relay's operator-supplied process
// configuration from the environment. Configuration is non-secret with one
// deliberate exception: secrets (the bootstrap token) are referenced by file path,
// never carried in an environment value, so they cannot leak through process
// listings or diagnostic dumps of the environment.
package config

import (
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Bounds on operator-tunable values. The pod cap ceiling matches the capability
// schema's hard maximum — a local cap can lower the effective bound, never raise it.
const (
	maxLocalPods         = 50
	maxConcurrentJobsCap = 64
)

// Config is the validated process configuration.
type Config struct {
	// ControlPlaneAddress is the gRPC dial target, host:port.
	ControlPlaneAddress string
	// OrgID scopes bootstrap enrollment; after enrollment the durable credential's
	// org binding is authoritative.
	OrgID string
	// CredentialFile is where the durable credential lives (file custody).
	CredentialFile string
	// BootstrapTokenFile, when set, points at a single-use bootstrap token consumed on
	// first run. Empty means enrollment is not attempted this start.
	BootstrapTokenFile string
	// KubeconfigPath selects the explicit-kubeconfig harness path; empty means
	// in-cluster ServiceAccount configuration — the production default.
	KubeconfigPath string
	// InitialSPKIPins closes the bootstrap trust window: when present, even the first
	// Register call verifies the control plane against these pins.
	InitialSPKIPins []string

	LocalMaxPods      int64
	MaxConcurrentJobs uint32
	HeartbeatInterval time.Duration
	ResendInterval    time.Duration
}

// Load reads configuration through lookup (os.LookupEnv in production) and validates
// every value. It fails on the first problem, naming the offending variable — but
// never echoing secret material, which no environment variable may carry.
func Load(lookup func(string) (string, bool)) (Config, error) {
	cfg := Config{
		LocalMaxPods:      maxLocalPods,
		MaxConcurrentJobs: 4,
		HeartbeatInterval: 15 * time.Second,
		ResendInterval:    10 * time.Second,
	}

	var err error
	if cfg.ControlPlaneAddress, err = required(lookup, "RELAY_CONTROL_PLANE_ADDRESS"); err != nil {
		return Config{}, err
	}
	if hostPortErr := validateHostPort(cfg.ControlPlaneAddress); hostPortErr != nil {
		return Config{}, fmt.Errorf("RELAY_CONTROL_PLANE_ADDRESS must be host:port: %w", hostPortErr)
	}
	if cfg.OrgID, err = required(lookup, "RELAY_ORG_ID"); err != nil {
		return Config{}, err
	}
	if cfg.CredentialFile, err = required(lookup, "RELAY_CREDENTIAL_FILE"); err != nil {
		return Config{}, err
	}

	cfg.BootstrapTokenFile, _ = lookup("RELAY_BOOTSTRAP_TOKEN_FILE")
	cfg.KubeconfigPath, _ = lookup("RELAY_KUBECONFIG")

	if value, ok := lookup("RELAY_LOCAL_MAX_PODS"); ok {
		pods, parseErr := strconv.ParseInt(value, 10, 64)
		if parseErr != nil || pods < 1 || pods > maxLocalPods {
			return Config{}, fmt.Errorf("RELAY_LOCAL_MAX_PODS must be an integer in [1, %d]", maxLocalPods)
		}
		cfg.LocalMaxPods = pods
	}
	if value, ok := lookup("RELAY_MAX_CONCURRENT_JOBS"); ok {
		jobs, parseErr := strconv.ParseUint(value, 10, 32)
		if parseErr != nil || jobs < 1 || jobs > maxConcurrentJobsCap {
			return Config{}, fmt.Errorf("RELAY_MAX_CONCURRENT_JOBS must be an integer in [1, %d]", maxConcurrentJobsCap)
		}
		cfg.MaxConcurrentJobs = uint32(jobs)
	}
	if cfg.HeartbeatInterval, err = interval(lookup, "RELAY_HEARTBEAT_INTERVAL", cfg.HeartbeatInterval); err != nil {
		return Config{}, err
	}
	if cfg.ResendInterval, err = interval(lookup, "RELAY_RESEND_INTERVAL", cfg.ResendInterval); err != nil {
		return Config{}, err
	}
	if cfg.InitialSPKIPins, err = pins(lookup, "RELAY_INITIAL_SPKI_PINS"); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validateHostPort accepts only a bare dial address: a non-empty host (no scheme, no
// path) and a numeric port. net.SplitHostPort alone would accept "https://host" by
// splitting on the scheme's colon.
func validateHostPort(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if host == "" || strings.Contains(host, "/") {
		return fmt.Errorf("invalid host %q", host)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65_535 {
		return fmt.Errorf("invalid port %q", port)
	}
	return nil
}

func required(lookup func(string) (string, bool), key string) (string, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return strings.TrimSpace(value), nil
}

func interval(lookup func(string) (string, bool), key string, fallback time.Duration) (time.Duration, error) {
	value, ok := lookup(key)
	if !ok {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return parsed, nil
}

// pins parses a comma-separated SPKI pin list. Each pin must be a standard-base64
// SHA-256 digest — 32 bytes — so a truncated or mistyped pin fails at startup, not as
// an unexplainable handshake refusal later.
func pins(lookup func(string) (string, bool), key string) ([]string, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var parsed []string
	for _, entry := range strings.Split(value, ",") {
		pin := strings.TrimSpace(entry)
		if pin == "" {
			continue
		}
		digest, err := base64.StdEncoding.DecodeString(pin)
		if err != nil || len(digest) != 32 {
			return nil, fmt.Errorf("%s: each pin must be a base64-encoded SHA-256 digest", key)
		}
		parsed = append(parsed, pin)
	}
	if len(parsed) == 0 {
		return nil, fmt.Errorf("%s: no usable pins", key)
	}
	return parsed, nil
}
