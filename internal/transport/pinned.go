// Package transport builds the client TLS credentials for every control-plane
// connection. Trust is key-pinned: the control plane's SPKI pin set (delivered at
// enrollment, refreshed with rotations) is the trust anchor, so a mis-issued or
// stolen-CA certificate for the right hostname still cannot impersonate the control
// plane. Standard WebPKI verification exists only as the explicit bootstrap fallback
// for the very first Register call when no initial pin was configured.
package transport

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"

	"google.golang.org/grpc/credentials"
)

// NewPinnedCredentials returns gRPC transport credentials that accept a server only
// when the SHA-256 of its leaf certificate's SubjectPublicKeyInfo matches one of pins
// (standard base64). Multiple pins exist for rotation overlap: current plus successor.
func NewPinnedCredentials(serverName string, pins []string) (credentials.TransportCredentials, error) {
	config, err := pinnedTLSConfig(serverName, pins)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(config), nil
}

// NewSystemCredentials returns standard WebPKI-verified TLS credentials — the
// bootstrap-only fallback, used solely for the first Register call when the operator
// configured no initial pin. Every post-enrollment connection uses the pinned set.
func NewSystemCredentials(serverName string) credentials.TransportCredentials {
	return credentials.NewTLS(&tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12})
}

// pinnedTLSConfig builds the pin-enforcing client TLS configuration. Chain validation
// against a CA store is deliberately absent: the pin IS the trust decision, which is
// what lets a self-hosted control plane run on a self-signed certificate. The
// dial-time ServerName still rides in the handshake for SNI routing.
func pinnedTLSConfig(serverName string, pins []string) (*tls.Config, error) {
	digests, err := decodePins(pins)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
		// Standard verification is replaced, not skipped: VerifyPeerCertificate below
		// runs on every handshake and refuses any leaf whose key matches no pin.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("server presented no certificate")
			}
			leaf, parseErr := x509.ParseCertificate(rawCerts[0])
			if parseErr != nil {
				return errors.New("server leaf certificate unparseable")
			}
			presented := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
			for _, pinned := range digests {
				if subtle.ConstantTimeCompare(presented[:], pinned[:]) == 1 {
					return nil
				}
			}
			return errors.New("server public key matches no pinned key")
		},
	}, nil
}

func decodePins(pins []string) ([][sha256.Size]byte, error) {
	if len(pins) == 0 {
		return nil, errors.New("no pins: pinned credentials require at least one SPKI pin")
	}
	digests := make([][sha256.Size]byte, 0, len(pins))
	for _, pin := range pins {
		raw, err := base64.StdEncoding.DecodeString(pin)
		if err != nil || len(raw) != sha256.Size {
			return nil, fmt.Errorf("pin %d is not a base64-encoded SHA-256 digest", len(digests))
		}
		digests = append(digests, [sha256.Size]byte(raw))
	}
	return digests, nil
}
