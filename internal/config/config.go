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
	"regexp"
	"strconv"
	"strings"
	"time"
)

// dns1123Label validates a namespace in the read allowlist. Configuration validates it
// here so a typo fails at startup rather than as a capability that quietly refuses every
// read against a namespace the operator believes they permitted.
var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Bounds on operator-tunable values. Each volume ceiling matches the corresponding
// capability schema's hard maximum — a local cap can lower the effective bound, never
// raise it — and a test in this package pins each one against the capability constant it
// mirrors, so the two cannot drift apart silently.
const (
	maxLocalPods         = 50
	maxLocalEvents       = 200
	maxLocalLogLines     = 2000
	maxLocalLogBytes     = 256 * 1024
	maxConcurrentJobsCap = 64
	// maxAllowedNamespaces bounds the allowlist so a malformed environment cannot turn
	// startup into an unbounded allocation.
	maxAllowedNamespaces = 256
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

	// RedactionPolicyFile points at the customer-authored redaction policy: a nested YAML
	// document under a `redaction:` root that may only ADD to the built-in masking. Empty
	// means the operator has not written one and the built-in defaults are in force — which
	// is a safe install rather than an unprotected one.
	//
	// It is a path rather than a value for the same reason the bootstrap token is: the file
	// that governs what may leave this cluster must not be readable from a process listing.
	// It should be mounted read-only and owned by the account the Relay runs as, mode 0400 —
	// a world-readable policy tells any local process exactly which shapes are NOT masked.
	//
	// A path that is set and cannot be read is fatal to every capability that could emit free
	// text. It never falls back to the defaults: an operator who named a file meant it to be
	// enforced.
	RedactionPolicyFile string

	// InventoryConfigFile points at the operator's constraints on inventory
	// synchronization: a nested YAML document under an `inventory:` root that can
	// disable the path, narrow the watched namespaces, and floor the interval the
	// control plane may request. Empty means no file and the built-in defaults apply —
	// enabled, constrained only by RELAY_ALLOWED_NAMESPACES, floored at the built-in
	// minimum. A path that is set and cannot be read is fatal: an operator who named a
	// file meant it to be enforced.
	InventoryConfigFile string

	LocalMaxPods      int64
	MaxConcurrentJobs uint32
	HeartbeatInterval time.Duration
	ResendInterval    time.Duration

	// The customer-authored caps on what the evidence capabilities may return. Every one
	// of them may lower a bound the control plane asked for and none may raise one, so a
	// server-side change can never increase what leaves this cluster.
	LocalMaxEvents   int64
	LocalMaxLogLines int64
	LocalMaxLogBytes int64

	// AllowedNamespaces, when non-empty, is the complete set of namespaces any capability
	// may read. Empty means the operator has not narrowed it and the Relay's own RBAC is
	// the boundary — which is where it was before this existed, and is why empty is not
	// read as "none".
	AllowedNamespaces map[string]bool

	// EventRetention is how long the operator attests this cluster keeps Events. It is an
	// attestation rather than a permission: nobody but the operator knows their
	// apiserver's --event-ttl, and an empty event window read over a horizon nobody stated
	// cannot tell "nothing happened" from "already discarded".
	EventRetention time.Duration
}

// Load reads configuration through lookup (os.LookupEnv in production) and validates
// every value. It fails on the first problem, naming the offending variable — but
// never echoing secret material, which no environment variable may carry.
func Load(lookup func(string) (string, bool)) (Config, error) {
	cfg := Config{
		LocalMaxPods:      maxLocalPods,
		LocalMaxEvents:    maxLocalEvents,
		LocalMaxLogLines:  maxLocalLogLines,
		LocalMaxLogBytes:  maxLocalLogBytes,
		MaxConcurrentJobs: 4,
		HeartbeatInterval: 15 * time.Second,
		ResendInterval:    10 * time.Second,
		// One hour is the Kubernetes default event TTL. Assuming it is the conservative
		// direction: it can only cause a window to be reported as reaching past retention,
		// which costs a certified absence rather than fabricating one.
		EventRetention: time.Hour,
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
	cfg.RedactionPolicyFile, _ = lookup("RELAY_REDACTION_POLICY_FILE")
	cfg.InventoryConfigFile, _ = lookup("RELAY_INVENTORY_CONFIG_FILE")

	if cfg.LocalMaxPods, err = volumeCap(lookup, "RELAY_LOCAL_MAX_PODS", maxLocalPods, cfg.LocalMaxPods); err != nil {
		return Config{}, err
	}
	if cfg.LocalMaxEvents, err = volumeCap(
		lookup, "RELAY_LOCAL_MAX_EVENTS", maxLocalEvents, cfg.LocalMaxEvents); err != nil {
		return Config{}, err
	}
	if cfg.LocalMaxLogLines, err = volumeCap(
		lookup, "RELAY_LOCAL_MAX_LOG_LINES", maxLocalLogLines, cfg.LocalMaxLogLines); err != nil {
		return Config{}, err
	}
	if cfg.LocalMaxLogBytes, err = volumeCap(
		lookup, "RELAY_LOCAL_MAX_LOG_BYTES", maxLocalLogBytes, cfg.LocalMaxLogBytes); err != nil {
		return Config{}, err
	}
	if cfg.AllowedNamespaces, err = namespaces(lookup, "RELAY_ALLOWED_NAMESPACES"); err != nil {
		return Config{}, err
	}
	if cfg.EventRetention, err = interval(lookup, "RELAY_EVENT_RETENTION", cfg.EventRetention); err != nil {
		return Config{}, err
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

// volumeCap parses one operator-set ceiling on how much a capability may return. The upper
// bound is the capability schema's own maximum: a value above it is refused rather than
// clamped, because an operator who typed a number larger than the product allows has
// misunderstood what the setting does, and silently serving them a smaller one leaves them
// believing something untrue about their own cluster.
func volumeCap(
	lookup func(string) (string, bool), key string, ceiling, fallback int64,
) (int64, error) {
	value, ok := lookup(key)
	if !ok {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 1 || parsed > ceiling {
		return 0, fmt.Errorf("%s must be an integer in [1, %d]", key, ceiling)
	}
	return parsed, nil
}

// namespaces parses the read allowlist. An unset or blank variable returns nil, which the
// capabilities read as "the operator has not narrowed this" rather than as "allow none" —
// the opposite reading would silently disable every capability the moment someone added an
// empty variable to a chart.
func namespaces(lookup func(string) (string, bool), key string) (map[string]bool, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return nil, nil
	}
	allowed := make(map[string]bool)
	for _, entry := range strings.Split(value, ",") {
		namespace := strings.TrimSpace(entry)
		if namespace == "" {
			continue
		}
		// Checked before the entry is added, not after the whole list is built. A bound
		// enforced once the allocation has already happened is not a bound.
		if len(allowed) >= maxAllowedNamespaces {
			return nil, fmt.Errorf("%s: at most %d namespaces", key, maxAllowedNamespaces)
		}
		if !dns1123Label.MatchString(namespace) || len(namespace) > 63 {
			return nil, fmt.Errorf("%s: %q is not a Kubernetes namespace", key, namespace)
		}
		allowed[namespace] = true
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("%s: no usable namespaces", key)
	}
	return allowed, nil
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
