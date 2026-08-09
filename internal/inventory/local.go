package inventory

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// localVersion is the only format version this build understands. Local configuration
// constrains what leaves the cluster and how often it is read, so an unrecognised
// version is refused rather than interpreted optimistically.
const localVersion = 1

// defaultMinimumInterval is the floor applied when the operator has not written one.
// The control plane REQUESTS an interval; this is the lowest any request can take
// effect at, so a server-side change can never increase load on a cluster whose
// operator wrote nothing.
const defaultMinimumInterval = 30 * time.Second

var namespaceLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Local is the operator's constraints on inventory synchronization. The control plane
// requests; this constrains. It can disable the path, narrow which namespaces are
// watched, and floor the interval — it cannot widen anything, because there is nothing
// here a wider value would be read from.
type Local struct {
	Enabled bool
	// Namespaces, when non-nil, is the complete set synchronization may watch. Nil
	// means the operator has not narrowed it here — the process-wide
	// RELAY_ALLOWED_NAMESPACES allowlist and the Relay's own RBAC still apply.
	Namespaces map[string]bool
	// MinimumInterval is the floor under any control-plane-requested interval.
	MinimumInterval time.Duration
}

// document is the file's shape, nested under an `inventory:` root per the recorded
// decision that `discovery:` would collide with the on-demand discovery capability an
// operator would reasonably expect it to configure. Decoding is STRICT: an unknown key
// fails the parse, so a misspelled constraint is loud at startup instead of silently
// absent.
type document struct {
	Inventory section `yaml:"inventory"`
}

type section struct {
	Version int `yaml:"version"`
	// Enabled is a pointer so an absent key means "enabled" while an explicit false
	// means what it says. A zero-value bool could not tell those apart.
	Enabled         *bool    `yaml:"enabled"`
	Namespaces      []string `yaml:"namespaces"`
	MinimumInterval string   `yaml:"minimum_interval"`
}

// DefaultLocal is what applies when the operator has written no file: enabled, not
// narrowed, floored at the built-in minimum. Synchronization is on by default because
// the ledger it feeds is the answer to the first question of every investigation, and
// the path carries identity and declared intent only — there is no content a cautious
// default would be protecting.
func DefaultLocal() Local {
	return Local{Enabled: true, MinimumInterval: defaultMinimumInterval}
}

// LoadLocal reads the operator's inventory configuration. An empty path means no file
// and the defaults apply; a path that is set and cannot be read is fatal, because an
// operator who named a file meant it to be enforced.
func LoadLocal(path string) (Local, error) {
	if path == "" {
		return DefaultLocal(), nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Local{}, fmt.Errorf("reading inventory configuration: %w", err)
	}
	return ParseLocal(raw, path)
}

// ParseLocal parses an inventory configuration document. Every refusal names the file
// and the fault; none falls back to the defaults.
func ParseLocal(raw []byte, source string) (Local, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)

	var parsed document
	if err := decoder.Decode(&parsed); err != nil {
		if errors.Is(err, io.EOF) {
			return Local{}, fmt.Errorf("%s is empty; a configuration file that exists must say "+
				"something, because an empty one is indistinguishable from a truncated write", source)
		}
		return Local{}, fmt.Errorf("%s does not parse: %s", source, firstLine(err.Error()))
	}
	if err := decoder.Decode(new(document)); !errors.Is(err, io.EOF) {
		return Local{}, fmt.Errorf("%s carries more than one document; the configuration is one "+
			"document", source)
	}

	if parsed.Inventory.Version != localVersion {
		return Local{}, fmt.Errorf("%s declares inventory format version %d; this build "+
			"understands version %d", source, parsed.Inventory.Version, localVersion)
	}

	local := DefaultLocal()
	if parsed.Inventory.Enabled != nil {
		local.Enabled = *parsed.Inventory.Enabled
	}

	if len(parsed.Inventory.Namespaces) > 0 {
		local.Namespaces = make(map[string]bool, len(parsed.Inventory.Namespaces))
		for _, entry := range parsed.Inventory.Namespaces {
			namespace := strings.TrimSpace(entry)
			if !namespaceLabel.MatchString(namespace) || len(namespace) > 63 {
				return Local{}, fmt.Errorf("%s: %q is not a Kubernetes namespace", source, namespace)
			}
			local.Namespaces[namespace] = true
		}
	}

	if parsed.Inventory.MinimumInterval != "" {
		interval, err := time.ParseDuration(parsed.Inventory.MinimumInterval)
		if err != nil || interval <= 0 {
			return Local{}, fmt.Errorf("%s: minimum_interval must be a positive duration", source)
		}
		local.MinimumInterval = interval
	}
	return local, nil
}

func firstLine(message string) string {
	if index := strings.IndexByte(message, '\n'); index >= 0 {
		return strings.TrimSpace(message[:index])
	}
	return strings.TrimSpace(message)
}
