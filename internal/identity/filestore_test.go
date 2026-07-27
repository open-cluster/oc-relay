package identity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestFileStore_SaveThenLoadRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.json")
	store := NewFileStore(path)
	saved := Credential{
		OrgID:           "org-1",
		RegistrationID:  "reg-1",
		Secret:          "secret-canary",
		SPKIPins:        []string{"pin-a", "pin-b"},
		ProtocolVersion: 1,
	}

	if err := store.Save(context.Background(), saved); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.OrgID != "org-1" || loaded.RegistrationID != "reg-1" ||
		loaded.Secret != "secret-canary" || loaded.ProtocolVersion != 1 ||
		len(loaded.SPKIPins) != 2 || loaded.SPKIPins[0] != "pin-a" || loaded.SPKIPins[1] != "pin-b" {
		t.Fatalf("roundtrip mismatch: %+v", loaded)
	}
}

func TestFileStore_LoadWithoutFileIsErrNotFound(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), "absent.json"))

	_, err := store.Load(context.Background())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestFileStore_CorruptFileIsNotErrNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.json")
	if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewFileStore(path)

	_, err := store.Load(context.Background())
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("a corrupt credential must surface as a storage fault, not first-run: %v", err)
	}
}

func TestFileStore_SaveOverwritesExistingCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.json")
	store := NewFileStore(path)
	ctx := context.Background()

	if err := store.Save(ctx, Credential{OrgID: "org-1", RegistrationID: "reg-1", Secret: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, Credential{OrgID: "org-1", RegistrationID: "reg-1", Secret: "second"}); err != nil {
		t.Fatalf("overwrite must succeed (rotation writes the successor): %v", err)
	}
	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Secret != "second" {
		t.Fatalf("latest credential must win: %+v", loaded)
	}
}

func TestFileStore_FileIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not enforced on Windows")
	}
	path := filepath.Join(t.TempDir(), "credential.json")
	store := NewFileStore(path)

	if err := store.Save(context.Background(), Credential{OrgID: "o", RegistrationID: "r", Secret: "s"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("credential file must be owner-only, got %o", perm)
	}
}
