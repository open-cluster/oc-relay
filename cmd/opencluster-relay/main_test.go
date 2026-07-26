package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OCluster/opencluster-relay/internal/config"
)

func TestReadBootstrapToken_TrimsAndRefusesEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("  the-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	token, err := readBootstrapToken(path)
	if err != nil || token != "the-token" {
		t.Fatalf("token = %q, err = %v", token, err)
	}

	if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBootstrapToken(path); err == nil {
		t.Fatal("an empty token file must be refused")
	}
}

func TestTransportCredentials_PinPrecedence(t *testing.T) {
	validPin := strings.Repeat("A", 43) + "="
	buffer := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buffer, nil))
	cfg := config.Config{ControlPlaneAddress: "control-plane.example:443"}

	// Neither credential nor initial pins: WebPKI fallback, loudly.
	if _, err := transportCredentials(cfg, nil, logger); err != nil {
		t.Fatalf("WebPKI fallback must build: %v", err)
	}
	if !strings.Contains(buffer.String(), "no SPKI pins") {
		t.Fatal("the unpinned fallback must be logged")
	}

	// Credential pins present: pinned, no fallback warning.
	buffer.Reset()
	if _, err := transportCredentials(cfg, []string{validPin}, logger); err != nil {
		t.Fatalf("pinned credentials must build: %v", err)
	}
	if buffer.Len() != 0 {
		t.Fatalf("no warning expected when pinned: %s", buffer.String())
	}

	// Initial pins only (bootstrap): pinned as well.
	cfg.InitialSPKIPins = []string{validPin}
	if _, err := transportCredentials(cfg, nil, logger); err != nil {
		t.Fatalf("initial-pin credentials must build: %v", err)
	}
	if buffer.Len() != 0 {
		t.Fatalf("no warning expected with initial pins: %s", buffer.String())
	}
}
