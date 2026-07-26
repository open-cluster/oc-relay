package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"math/big"
	"net"
	"testing"
	"time"
)

// selfSignedServer is a throwaway TLS identity plus its SPKI pin.
type selfSignedServer struct {
	certificate tls.Certificate
	pin         string
}

func newSelfSignedServer(t *testing.T) selfSignedServer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "control-plane.test"},
		DNSNames:     []string{"control-plane.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
	return selfSignedServer{
		certificate: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		pin:         base64.StdEncoding.EncodeToString(digest[:]),
	}
}

// handshake runs one client handshake against a live TLS listener presenting the
// server identity, using the client tls.Config under test. It returns the client-side
// handshake error.
func handshake(t *testing.T, server selfSignedServer, clientConfig *tls.Config) error {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		tlsConn := tls.Server(conn, &tls.Config{
			Certificates: []tls.Certificate{server.certificate},
			MinVersion:   tls.VersionTLS12,
		})
		_ = tlsConn.Handshake() // the client-side verdict is what the test asserts
	}()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	clientErr := tls.Client(conn, clientConfig).Handshake()
	conn.Close()
	<-serverDone
	return clientErr
}

func TestPinnedTLSConfig_MatchingPinAcceptsHandshake(t *testing.T) {
	server := newSelfSignedServer(t)
	clientConfig, err := pinnedTLSConfig("control-plane.test", []string{server.pin})
	if err != nil {
		t.Fatalf("pinnedTLSConfig: %v", err)
	}

	if err := handshake(t, server, clientConfig); err != nil {
		t.Fatalf("a server presenting a pinned key must be accepted: %v", err)
	}
}

func TestPinnedTLSConfig_UnpinnedServerIsRefused(t *testing.T) {
	pinnedIdentity := newSelfSignedServer(t)
	impostor := newSelfSignedServer(t)
	clientConfig, err := pinnedTLSConfig("control-plane.test", []string{pinnedIdentity.pin})
	if err != nil {
		t.Fatalf("pinnedTLSConfig: %v", err)
	}

	if err := handshake(t, impostor, clientConfig); err == nil {
		t.Fatal("a server whose key matches no pin must be refused")
	}
}

func TestPinnedTLSConfig_SecondPinAlsoAccepts(t *testing.T) {
	server := newSelfSignedServer(t)
	other := newSelfSignedServer(t)
	clientConfig, err := pinnedTLSConfig("control-plane.test", []string{other.pin, server.pin})
	if err != nil {
		t.Fatalf("pinnedTLSConfig: %v", err)
	}

	if err := handshake(t, server, clientConfig); err != nil {
		t.Fatalf("rotation overlap: any pin in the set must accept: %v", err)
	}
}

func TestNewPinnedCredentials_RefusesUnusablePins(t *testing.T) {
	cases := [][]string{
		nil,
		{},
		{"not-base64!!"},
		{"c2hvcnQ="}, // decodes, but not 32 bytes
	}
	for _, pins := range cases {
		if _, err := NewPinnedCredentials("control-plane.test", pins); err == nil {
			t.Fatalf("pins %v must be refused at construction", pins)
		}
	}
}

func TestNewPinnedCredentials_BuildsWithValidPin(t *testing.T) {
	server := newSelfSignedServer(t)
	credentials, err := NewPinnedCredentials("control-plane.test", []string{server.pin})
	if err != nil {
		t.Fatalf("NewPinnedCredentials: %v", err)
	}
	if credentials.Info().SecurityProtocol != "tls" {
		t.Fatalf("expected TLS transport credentials, got %+v", credentials.Info())
	}
}

func TestPinnedTLSConfig_ErrorNamesNoKeyMaterial(t *testing.T) {
	pinnedIdentity := newSelfSignedServer(t)
	impostor := newSelfSignedServer(t)
	clientConfig, err := pinnedTLSConfig("control-plane.test", []string{pinnedIdentity.pin})
	if err != nil {
		t.Fatal(err)
	}

	handshakeErr := handshake(t, impostor, clientConfig)
	if handshakeErr == nil {
		t.Fatal("expected refusal")
	}
	var pinErr error = handshakeErr
	for {
		if unwrapped := errors.Unwrap(pinErr); unwrapped != nil {
			pinErr = unwrapped
			continue
		}
		break
	}
	// The refusal explains itself without echoing the presented certificate's key.
	if text := handshakeErr.Error(); len(text) > 300 {
		t.Fatalf("refusal message suspiciously large (embedding material?): %q", text)
	}
}
