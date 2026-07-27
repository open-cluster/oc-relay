package identity

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

// BootstrapTokenMetadataKey carries the single-use bootstrap token in Register call
// metadata over TLS — never in a message field, never in logs.
const BootstrapTokenMetadataKey = "opencluster-bootstrap-token"

// OrgIDMetadataKey carries the claimed organization id in Register call metadata.
// Identity scope rides in metadata alongside the token; the request message carries
// attestations only. The claim scopes the server-side token lookup and the per-org
// flood partition — it grants nothing by itself.
const OrgIDMetadataKey = "opencluster-org-id"

// EnrollmentParams is the non-secret content of the registration request.
type EnrollmentParams struct {
	// OrgID is the organization the operator created the bootstrap token under. The
	// control plane's response echoes the authoritative value; a mismatch refuses the
	// enrollment without persisting anything.
	OrgID              string
	ProtocolVersion    uint32
	RelayVersion       string
	ClusterFingerprint string
	Capabilities       []*relayv1.CapabilityDescriptor
}

// Enroll performs the one-time bootstrap: it presents the token in call metadata,
// persists the returned durable credential through the store, and only then returns
// it. Enroll retains no copy of the token; the caller should drop its own copy as soon
// as Enroll returns. A refused registration maps to ErrEnrollmentRefused and a failed
// persist maps to ErrStorageForbidden — in both cases nothing usable is kept, and no
// error carries secret material.
func Enroll(
	ctx context.Context,
	client relayv1.RelayRegistrationServiceClient,
	bootstrapToken string,
	params EnrollmentParams,
	store Store,
) (Credential, error) {
	ctx = metadata.AppendToOutgoingContext(ctx,
		BootstrapTokenMetadataKey, bootstrapToken,
		OrgIDMetadataKey, params.OrgID,
	)

	response, err := client.Register(ctx, &relayv1.RegisterRequest{
		ProtocolVersion:    params.ProtocolVersion,
		RelayVersion:       params.RelayVersion,
		ClusterFingerprint: params.ClusterFingerprint,
		Capabilities:       params.Capabilities,
	})
	if err != nil {
		// The contract pins FAILED_PRECONDITION as the single refusal status; the two
		// auth-flavored codes are accepted defensively so a middlebox or server quirk
		// can never turn a terminal refusal into a retry loop on a consumed token.
		if code := status.Code(err); code == codes.FailedPrecondition ||
			code == codes.Unauthenticated || code == codes.PermissionDenied {
			return Credential{}, fmt.Errorf("register: %w", ErrEnrollmentRefused)
		}
		return Credential{}, fmt.Errorf("register: %w", err)
	}

	// The response's org id is the organization the token was actually consumed under.
	// It can differ from the claim only if something between us and the store is wrong
	// — refuse and keep nothing rather than persist an identity we did not ask for.
	if response.GetOrgId() != params.OrgID {
		return Credential{}, fmt.Errorf(
			"register: control plane bound the credential to organization %q, claimed %q: %w",
			response.GetOrgId(), params.OrgID, ErrEnrollmentRefused)
	}

	credential := Credential{
		OrgID:           response.GetOrgId(),
		RegistrationID:  response.GetRegistrationId(),
		Secret:          response.GetCredential(),
		SPKIPins:        response.GetSpkiPins(),
		ProtocolVersion: response.GetProtocolVersion(),
	}
	if err := store.Save(ctx, credential); err != nil {
		// The unpersisted credential is discarded: surviving only in memory would mean a
		// silent re-bootstrap wall on the next restart. The wrap carries the cause but
		// the message can never include secret material of ours.
		return Credential{}, fmt.Errorf("%w: persisting durable credential: %w", ErrStorageForbidden, err)
	}

	return credential, nil
}

// LoadOnStart loads the durable credential on process start. A missing credential maps
// to ErrBootstrapRequired (first run or post-storage-loss); any other storage failure
// is returned wrapped, unchanged.
func LoadOnStart(ctx context.Context, store Store) (Credential, error) {
	credential, err := store.Load(ctx)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Credential{}, ErrBootstrapRequired
		}
		return Credential{}, fmt.Errorf("loading durable credential: %w", err)
	}
	return credential, nil
}
