package session

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/open-cluster/oc-relay/internal/identity"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"
)

type capturingSessionClient struct {
	gotMetadata metadata.MD
}

func (c *capturingSessionClient) Connect(
	ctx context.Context, _ ...grpc.CallOption,
) (grpc.BidiStreamingClient[relayv1.RelayToControl, relayv1.ControlToRelay], error) {
	c.gotMetadata, _ = metadata.FromOutgoingContext(ctx)
	return nil, context.Canceled // we only assert the metadata, not a live stream
}

// The connector presents the durable identity in call metadata (org id, registration id,
// credential) — never in a message field — and the credential never appears anywhere but
// the credential metadata key.
func TestGRPCConnector_PresentsIdentityInMetadata(t *testing.T) {
	client := &capturingSessionClient{}
	credential := identity.Credential{
		OrgID:          "org-a",
		RegistrationID: "42",
		Secret:         "durable-credential-VERY-secret",
	}
	connect := NewGRPCConnector(client, credential)

	_, _ = connect(context.Background())

	if got := client.gotMetadata.Get(OrgIDMetadataKey); len(got) != 1 || got[0] != "org-a" {
		t.Fatalf("org id must ride in metadata %q, got %v", OrgIDMetadataKey, got)
	}
	if got := client.gotMetadata.Get(RegistrationIDMetadataKey); len(got) != 1 || got[0] != "42" {
		t.Fatalf("registration id must ride in metadata %q, got %v", RegistrationIDMetadataKey, got)
	}
	if got := client.gotMetadata.Get(CredentialMetadataKey); len(got) != 1 ||
		got[0] != "durable-credential-VERY-secret" {
		t.Fatalf("credential must ride in metadata %q", CredentialMetadataKey)
	}
	// The credential must not leak into any other metadata key.
	for key, values := range client.gotMetadata {
		if key == CredentialMetadataKey {
			continue
		}
		for _, v := range values {
			if strings.Contains(v, "VERY-secret") {
				t.Fatalf("credential leaked into metadata key %q", key)
			}
		}
	}
}
