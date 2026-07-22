package session

import (
	"context"

	"google.golang.org/grpc/metadata"

	"github.com/OCluster/opencluster-relay/internal/identity"

	relayv1 "github.com/OCluster/opencluster-relay/gen/go/opencluster/relay/v1"
)

// Session-establishment metadata keys. Identity scope and the credential ride in call
// metadata over TLS — never in a message field, never in logs. These match the control
// plane's session service exactly.
const (
	OrgIDMetadataKey          = "opencluster-org-id"
	RegistrationIDMetadataKey = "opencluster-registration-id"
	CredentialMetadataKey     = "opencluster-relay-credential"
)

// NewGRPCConnector builds a Connector that opens a Connect stream on the given client,
// presenting the durable identity in call metadata. SPKI pinning and TLS are configured
// on the underlying dial (the composition root's concern); this connector only attaches
// identity to each attempt.
func NewGRPCConnector(client relayv1.RelaySessionServiceClient, credential identity.Credential) Connector {
	return func(ctx context.Context) (bidiStream, error) {
		ctx = metadata.AppendToOutgoingContext(ctx,
			OrgIDMetadataKey, credential.OrgID,
			RegistrationIDMetadataKey, credential.RegistrationID,
			CredentialMetadataKey, credential.Secret,
		)
		stream, err := client.Connect(ctx)
		if err != nil {
			return nil, err
		}
		return stream, nil
	}
}
