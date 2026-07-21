package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	relayv1 "github.com/OCluster/opencluster-relay/gen/go/opencluster/relay/v1"
)

// The enrollment contract: the bootstrap token travels only in call metadata, the
// durable credential is persisted through the consumer-owned store before Enroll
// returns, restart loads the stored credential instead of bootstrapping again, every
// failure maps to a documented degraded state, and no code path ever exposes secret
// material through errors or printing.

const testToken = "bootstrap-token-EXTREMELY-secret"

func testResponse() *relayv1.RegisterResponse {
	return &relayv1.RegisterResponse{
		OrgId:           "org-a",
		RegistrationId:  "42",
		Credential:      "durable-credential-VERY-secret",
		SpkiPins:        []string{"pin-active", "pin-backup"},
		ProtocolVersion: 1,
	}
}

func testParams() EnrollmentParams {
	return EnrollmentParams{
		OrgID:              "org-a",
		ProtocolVersion:    1,
		RelayVersion:       "0.1.0",
		ClusterFingerprint: "ns-uid-123",
		Capabilities: []*relayv1.CapabilityDescriptor{
			{CapabilityId: "kubernetes.workload.runtime", CapabilityVersion: 1},
		},
	}
}

func TestEnroll_PersistsCredentialAndSendsTokenOnlyInMetadata(t *testing.T) {
	client := &fakeRegistrationClient{resp: testResponse()}
	store := &memoryStore{}

	credential, err := Enroll(context.Background(), client, testToken, testParams(), store)
	if err != nil {
		t.Fatalf("enroll failed: %v", err)
	}

	if got := client.gotMetadata.Get(BootstrapTokenMetadataKey); len(got) != 1 || got[0] != testToken {
		t.Fatalf("bootstrap token must travel in metadata %q, got %v", BootstrapTokenMetadataKey, got)
	}
	if got := client.gotMetadata.Get(OrgIDMetadataKey); len(got) != 1 || got[0] != "org-a" {
		t.Fatalf("claimed org id must travel in metadata %q, got %v", OrgIDMetadataKey, got)
	}
	if client.gotRequest.GetClusterFingerprint() != "ns-uid-123" ||
		len(client.gotRequest.GetCapabilities()) != 1 ||
		client.gotRequest.GetProtocolVersion() != 1 {
		t.Fatalf("register request not built from params: %v", client.gotRequest)
	}

	if credential.Secret != "durable-credential-VERY-secret" || credential.OrgID != "org-a" ||
		credential.RegistrationID != "42" ||
		len(credential.SPKIPins) != 2 || credential.ProtocolVersion != 1 {
		t.Fatalf("credential not mapped from response: %+v", credential.RegistrationID)
	}
	if store.saved == nil || store.saved.Secret != credential.Secret {
		t.Fatalf("credential must be persisted through the store before Enroll returns")
	}
}

func TestEnroll_RefusalIsEnrollmentRefused_WithoutTokenInError(t *testing.T) {
	client := &fakeRegistrationClient{err: status.Error(codes.PermissionDenied, "refused")}
	store := &memoryStore{}

	_, err := Enroll(context.Background(), client, testToken, testParams(), store)
	if !errors.Is(err, ErrEnrollmentRefused) {
		t.Fatalf("refusal must map to ErrEnrollmentRefused, got %v", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error must not contain the bootstrap token")
	}
	if store.saved != nil {
		t.Fatalf("nothing may be persisted on refusal")
	}
}

func TestEnroll_SaveFailureIsStorageForbidden_WithoutSecretInError(t *testing.T) {
	client := &fakeRegistrationClient{resp: testResponse()}
	store := &memoryStore{saveErr: errors.New("secrets are read-only in this namespace")}

	_, err := Enroll(context.Background(), client, testToken, testParams(), store)
	if !errors.Is(err, ErrStorageForbidden) {
		t.Fatalf("save failure must map to ErrStorageForbidden, got %v", err)
	}
	if strings.Contains(err.Error(), "durable-credential-VERY-secret") ||
		strings.Contains(err.Error(), testToken) {
		t.Fatalf("error must not contain secret material: %v", err)
	}
}

func TestEnroll_OrgEchoMismatchRefusedWithoutPersisting(t *testing.T) {
	response := testResponse()
	response.OrgId = "org-OTHER"
	client := &fakeRegistrationClient{resp: response}
	store := &memoryStore{}

	_, err := Enroll(context.Background(), client, testToken, testParams(), store)
	if !errors.Is(err, ErrEnrollmentRefused) {
		t.Fatalf("org echo mismatch must refuse enrollment, got %v", err)
	}
	if strings.Contains(err.Error(), "durable-credential-VERY-secret") {
		t.Fatalf("error must not contain the credential: %v", err)
	}
	if store.saved != nil {
		t.Fatalf("a mismatched credential must never be persisted")
	}
}

func TestLoadOnStart_MissingCredentialIsBootstrapRequired(t *testing.T) {
	if _, err := LoadOnStart(context.Background(), &memoryStore{}); !errors.Is(err, ErrBootstrapRequired) {
		t.Fatalf("missing credential must map to ErrBootstrapRequired, got %v", err)
	}
}

func TestLoadOnStart_ReturnsTheSavedCredential_NoReBootstrap(t *testing.T) {
	client := &fakeRegistrationClient{resp: testResponse()}
	store := &memoryStore{}
	enrolled, err := Enroll(context.Background(), client, testToken, testParams(), store)
	if err != nil {
		t.Fatalf("enroll failed: %v", err)
	}

	loaded, err := LoadOnStart(context.Background(), store)
	if err != nil {
		t.Fatalf("restart must load the durable credential, got %v", err)
	}
	if loaded.Secret != enrolled.Secret || loaded.RegistrationID != enrolled.RegistrationID {
		t.Fatalf("loaded credential differs from the enrolled one")
	}
	if client.calls != 1 {
		t.Fatalf("restart must not re-register (register calls = %d)", client.calls)
	}
}

func TestCredentialPrinting_IsRedacted(t *testing.T) {
	credential := Credential{OrgID: "org-a", RegistrationID: "42",
		Secret: "durable-credential-VERY-secret"}
	for _, rendered := range []string{
		fmt.Sprintf("%v", credential),
		fmt.Sprintf("%+v", credential),
		fmt.Sprintf("%#v", credential),
		fmt.Sprint(credential),
	} {
		if strings.Contains(rendered, "VERY-secret") {
			t.Fatalf("printed credential leaks the secret: %s", rendered)
		}
		if !strings.Contains(rendered, "REDACTED") {
			t.Fatalf("printed credential must be visibly redacted: %s", rendered)
		}
	}
}

type fakeRegistrationClient struct {
	resp        *relayv1.RegisterResponse
	err         error
	calls       int
	gotMetadata metadata.MD
	gotRequest  *relayv1.RegisterRequest
}

func (f *fakeRegistrationClient) Register(
	ctx context.Context, in *relayv1.RegisterRequest, _ ...grpc.CallOption,
) (*relayv1.RegisterResponse, error) {
	f.calls++
	f.gotMetadata, _ = metadata.FromOutgoingContext(ctx)
	f.gotRequest = in
	return f.resp, f.err
}

type memoryStore struct {
	saved   *Credential
	saveErr error
}

func (m *memoryStore) Load(context.Context) (Credential, error) {
	if m.saved == nil {
		return Credential{}, ErrNotFound
	}
	return *m.saved, nil
}

func (m *memoryStore) Save(_ context.Context, credential Credential) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	m.saved = &credential
	return nil
}
