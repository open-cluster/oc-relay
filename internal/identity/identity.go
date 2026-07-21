// Package identity owns the Relay's durable credential lifecycle: one-time bootstrap
// enrollment against the control plane and credential loading on every later start.
// Secret material never appears in logs, errors, metrics, or printed values — the
// Credential type redacts itself — and persistence goes through the consumer-owned
// Store interface so deployments choose their own backing (a pre-created Kubernetes
// Secret in-cluster, a file beside a self-hosted control plane, memory in tests).
package identity

import (
	"context"
	"errors"
	"fmt"
)

// Credential is the Relay's durable identity, received exactly once at enrollment.
// The identity is the pair (OrgID, RegistrationID); Secret authenticates it.
type Credential struct {
	OrgID           string
	RegistrationID  string
	Secret          string
	SPKIPins        []string
	ProtocolVersion uint32
}

// String redacts the secret; the fmt package uses it for %v, %+v, and %s.
func (c Credential) String() string {
	return fmt.Sprintf("identity.Credential{org=%s registration=%s secret=[REDACTED] pins=%d}",
		c.OrgID, c.RegistrationID, len(c.SPKIPins))
}

// GoString redacts the secret for %#v as well.
func (c Credential) GoString() string { return c.String() }

// Store persists the durable credential. Implementations must return ErrNotFound from
// Load when no credential has ever been saved.
type Store interface {
	Load(ctx context.Context) (Credential, error)
	Save(ctx context.Context, credential Credential) error
}

// ErrNotFound is returned by Store.Load when no credential is persisted.
var ErrNotFound = errors.New("no credential stored")

// Degraded states. Each is terminal for the current process: the operator (or the
// installer) must act, and the Relay must not guess its way past them.
var (
	// ErrBootstrapRequired: no durable credential exists — first-run enrollment with a
	// fresh bootstrap token is required.
	ErrBootstrapRequired = errors.New("no durable credential: bootstrap enrollment required")

	// ErrStorageForbidden: the credential store refused a write — the credential from a
	// successful enrollment could not be persisted and is deliberately discarded, so a
	// fresh bootstrap token is required after the storage permission is fixed.
	ErrStorageForbidden = errors.New("credential storage forbidden")

	// ErrEnrollmentRefused: the control plane refused the bootstrap (invalid, expired,
	// consumed, or revoked) — a fresh bootstrap token is required.
	ErrEnrollmentRefused = errors.New("enrollment refused by the control plane: a fresh bootstrap token is required")
)
