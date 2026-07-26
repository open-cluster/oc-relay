package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// FileStore persists the durable credential as an owner-only JSON file — the custody
// backing for deployments beside a self-hosted control plane and for the test harness.
// In-cluster custody (a pre-created Kubernetes Secret) is a separate Store
// implementation that ships with the packaging slice.
type FileStore struct {
	path string
}

// NewFileStore stores the credential at path. The parent directory must exist; the
// installer owns directory creation and permissions.
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path}
}

// storedCredential is the serialized shape. It is versioned by field presence only —
// additive evolution, matching the credential's own compatibility rules.
type storedCredential struct {
	OrgID           string   `json:"org_id"`
	RegistrationID  string   `json:"registration_id"`
	Secret          string   `json:"secret"`
	SPKIPins        []string `json:"spki_pins,omitempty"`
	ProtocolVersion uint32   `json:"protocol_version"`
}

// Load reads the persisted credential. A missing file is ErrNotFound (first run); a
// present-but-unreadable or corrupt file is a storage fault — never ErrNotFound,
// because mapping it to first-run would silently demand a fresh bootstrap token while
// a credential still exists on disk.
func (s *FileStore) Load(_ context.Context) (Credential, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Credential{}, ErrNotFound
		}
		return Credential{}, fmt.Errorf("reading credential file: %w", err)
	}
	var stored storedCredential
	if err := json.Unmarshal(raw, &stored); err != nil {
		// The unmarshal error may quote file content (secret material): classify only.
		return Credential{}, errors.New("credential file corrupt")
	}
	return Credential(stored), nil
}

// Save writes the credential atomically: a same-directory temp file (owner-only) is
// renamed over the target, so a crash mid-write can never leave a truncated credential
// that would strand the Relay behind a re-bootstrap wall.
func (s *FileStore) Save(_ context.Context, credential Credential) error {
	payload, err := json.Marshal(storedCredential(credential))
	if err != nil {
		return fmt.Errorf("encoding credential: %w", err)
	}

	directory := filepath.Dir(s.path)
	temp, err := os.CreateTemp(directory, ".credential-*")
	if err != nil {
		return fmt.Errorf("creating credential temp file: %w", err)
	}
	tempPath := temp.Name()
	// Best-effort cleanup, a no-op after a successful rename. A failed remove must never
	// mask the write error the caller actually needs.
	defer func() { _ = os.Remove(tempPath) }()

	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("restricting credential file permissions: %w", err)
	}
	if _, err := temp.Write(payload); err != nil {
		_ = temp.Close()
		return fmt.Errorf("writing credential file: %w", err)
	}
	// Rename is atomic, but only over durable content: without the sync a crash after the
	// rename can still expose an empty file, which is exactly the re-bootstrap wall this
	// write order exists to prevent.
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("flushing credential file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("closing credential file: %w", err)
	}
	if err := os.Rename(tempPath, s.path); err != nil {
		return fmt.Errorf("committing credential file: %w", err)
	}
	return nil
}
