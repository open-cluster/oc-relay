package inventory

import (
	"strings"
	"testing"
	"time"
)

func TestLocal_NoFileMeansEnabledWithTheBuiltInFloor(t *testing.T) {
	local, err := LoadLocal("")
	if err != nil {
		t.Fatalf("no file is a supported state: %v", err)
	}
	if !local.Enabled || local.Namespaces != nil || local.MinimumInterval != defaultMinimumInterval {
		t.Fatalf("defaults must be enabled, unnarrowed, floored, got %+v", local)
	}
}

func TestLocal_AnUnknownKeyIsRefused(t *testing.T) {
	_, err := ParseLocal([]byte("inventory:\n  version: 1\n  intervall: 5m\n"), "inv.yaml")
	if err == nil {
		t.Fatal("a misspelled constraint must be loud at startup, not silently absent")
	}
}

func TestLocal_AnUnrecognisedVersionIsRefused(t *testing.T) {
	_, err := ParseLocal([]byte("inventory:\n  version: 2\n"), "inv.yaml")
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("an unknown format version must be refused by name, got %v", err)
	}
}

func TestLocal_ExplicitDisableIsHonoured(t *testing.T) {
	local, err := ParseLocal([]byte("inventory:\n  version: 1\n  enabled: false\n"), "inv.yaml")
	if err != nil {
		t.Fatalf("a valid file must parse: %v", err)
	}
	if local.Enabled {
		t.Fatal("enabled: false means what it says")
	}
}

func TestLocal_ABadNamespaceIsRefused(t *testing.T) {
	_, err := ParseLocal([]byte("inventory:\n  version: 1\n  namespaces: [\"Not_A_Namespace\"]\n"), "inv.yaml")
	if err == nil {
		t.Fatal("a namespace that cannot exist would silently watch nothing")
	}
}

func TestLocal_TheFloorIsParsedAndAppliesToWhatTheServerMayRequest(t *testing.T) {
	local, err := ParseLocal([]byte("inventory:\n  version: 1\n  minimum_interval: 2m\n"), "inv.yaml")
	if err != nil {
		t.Fatalf("a valid file must parse: %v", err)
	}
	if local.MinimumInterval != 2*time.Minute {
		t.Fatalf("expected a two-minute floor, got %v", local.MinimumInterval)
	}
}

func TestLocal_AnEmptyFileIsRefusedAsATruncatedWrite(t *testing.T) {
	if _, err := ParseLocal(nil, "inv.yaml"); err == nil {
		t.Fatal("an empty file is indistinguishable from a truncated write and must refuse")
	}
}

func TestLocal_ASecondDocumentIsRefused(t *testing.T) {
	raw := []byte("inventory:\n  version: 1\n---\ninventory:\n  version: 1\n")
	if _, err := ParseLocal(raw, "inv.yaml"); err == nil {
		t.Fatal("the ignored second document is exactly the one an operator would swear they configured")
	}
}
