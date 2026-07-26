// Package gates holds the repository's build-failing structural guarantees: the
// no-command/no-plugin import rules, the read-only Kubernetes port boundary, and the
// capability schema shape. They are ordinary tests over the real import graph, AST,
// and generated descriptors, so a //nolint comment or a lint-config edit cannot
// switch them off.
package gates

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	// Blank import so the generated descriptors register themselves in
	// protoregistry.GlobalFiles, which the schema-shape gate iterates.
	_ "github.com/OCluster/opencluster-relay/gen/go/opencluster/relay/v1"
)

const modulePath = "github.com/OCluster/opencluster-relay"

// bannedImports can appear nowhere in the repository, tests included. Each entry is a
// capability the Relay must not merely avoid but be UNABLE to acquire: process
// spawning, dynamic code loading, raw syscall surfaces, and the write/exec-capable
// corners of client-go.
var bannedImports = []string{
	"os/exec",
	"plugin",
	"golang.org/x/sys/unix",
	"k8s.io/client-go/tools/remotecommand",
	"k8s.io/client-go/tools/portforward",
	"k8s.io/client-go/transport/spdy",
}

func loadPackages(t *testing.T) []*packages.Package {
	t.Helper()
	loaded, err := packages.Load(&packages.Config{
		Mode:  packages.NeedName | packages.NeedImports | packages.NeedFiles,
		Dir:   moduleRoot(t),
		Tests: true,
	}, "./...")
	if err != nil {
		t.Fatalf("loading packages: %v", err)
	}
	if len(loaded) == 0 {
		t.Fatal("no packages loaded")
	}
	return loaded
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestNoForbiddenImportsAnywhere(t *testing.T) {
	for _, pkg := range loadPackages(t) {
		for imported := range pkg.Imports {
			for _, banned := range bannedImports {
				if imported == banned {
					t.Errorf("%s imports banned package %s", pkg.PkgPath, banned)
				}
			}
		}
	}
}

func TestCapabilityPackagesUseOnlyTheReadOnlyPort(t *testing.T) {
	for _, pkg := range loadPackages(t) {
		if !strings.HasPrefix(pkg.PkgPath, modulePath+"/internal/capabilities/") {
			continue
		}
		for imported := range pkg.Imports {
			if strings.HasPrefix(imported, "k8s.io/client-go/") {
				t.Errorf("capability package %s reaches client-go (%s) directly; "+
					"all cluster reads go through internal/kube", pkg.PkgPath, imported)
			}
		}
	}
}

func TestOnlyThePortAndCompositionRootTouchTheClientset(t *testing.T) {
	// The kube port owns cluster access; the two composition roots (the relay binary
	// and the differential-parity harness) construct the client and hand it to the port.
	allowed := map[string]bool{
		modulePath + "/internal/kube":         true,
		modulePath + "/cmd/opencluster-relay": true,
		modulePath + "/test/parity":           true,
	}
	for _, pkg := range loadPackages(t) {
		base := strings.TrimSuffix(strings.TrimSuffix(pkg.PkgPath, "_test"), ".test")
		if allowed[base] {
			continue
		}
		for imported := range pkg.Imports {
			// Any client-go subpackage is a cluster-access surface — a typed clientset,
			// a rest config, a kubeconfig loader, or a lower-level typed client. Match the
			// whole prefix so a future direct import of k8s.io/client-go/kubernetes/typed/...
			// cannot reach live reads outside the port and composition roots.
			if strings.HasPrefix(imported, "k8s.io/client-go/") {
				t.Errorf("%s imports %s; only the kube port and the composition root "+
					"may construct or hold cluster clients", pkg.PkgPath, imported)
			}
		}
	}
}

// bannedSelectors maps an import PATH to the symbols on it that spawn processes or load
// code dynamically. Keyed by canonical path, never by local identifier, so an import
// alias cannot hide a call: `os.StartProcess` and `import osx "os"; osx.StartProcess`
// resolve to the same "os.StartProcess" violation. os and syscall cannot be banned at
// the import-path level (they carry legitimate file-I/O and signal-constant use), so
// this alias-resolving selector scan is their only defense and must not be evadable.
var bannedSelectors = map[string]map[string]bool{
	"os": {"StartProcess": true},
	"syscall": {
		"Exec": true, "ForkExec": true, "StartProcess": true, "CreateProcess": true,
		// Raw syscall entry points: a hand-written execve syscall number through these
		// spawns a process below the named wrappers. Nothing idiomatic needs them here
		// (x/sys/unix is already import-banned), so banning them closes that vector.
		"Syscall": true, "Syscall6": true, "RawSyscall": true, "RawSyscall6": true,
	},
	"plugin":  {"Open": true},
	"os/exec": {"Command": true, "CommandContext": true, "LookPath": true},
}

// importAliases maps each of a file's local import names to its canonical import path.
// A dot import of a restricted package is reported separately (it turns a banned call
// into a bare identifier that a selector scan cannot see); blank imports are ignored
// (nothing is callable through `_`).
func importAliases(file *ast.File) (aliases map[string]string, dotImportedRestricted []string) {
	aliases = make(map[string]string)
	for _, spec := range file.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		local := path[strings.LastIndex(path, "/")+1:]
		if spec.Name != nil {
			switch spec.Name.Name {
			case "_":
				continue
			case ".":
				if _, restricted := bannedSelectors[path]; restricted {
					dotImportedRestricted = append(dotImportedRestricted, path)
				}
				continue
			default:
				local = spec.Name.Name
			}
		}
		aliases[local] = path
	}
	return aliases, dotImportedRestricted
}

// bannedSelectorViolations reports every process-spawning or dynamic-loading call in a
// parsed file, resolving import aliases to their canonical paths first. Each violation
// is rendered "importpath.Symbol" (or a dot-import note) so the message is
// alias-independent.
func bannedSelectorViolations(file *ast.File) []string {
	aliases, dotImported := importAliases(file)
	var violations []string
	for _, path := range dotImported {
		violations = append(violations, "dot-import of restricted package "+path)
	}
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := selector.X.(*ast.Ident)
		if !ok {
			return true
		}
		if path := aliases[ident.Name]; path != "" && bannedSelectors[path][selector.Sel.Name] {
			violations = append(violations, path+"."+selector.Sel.Name)
		}
		return true
	})
	return violations
}

// clientsetHolderDirs are the only directories whose files may import client-go: the
// read-only port and the two composition roots (the relay binary and the parity
// harness). Keyed by on-disk directory, so — unlike the packages.Load gates — a real
// sibling directory named "kube_test" cannot inherit the port's exemption by import-path
// trimming, and a build-tag-hidden file cannot escape the check by being uncompiled.
var clientsetHolderDirs = map[string]bool{
	"internal/kube":         true,
	"cmd/opencluster-relay": true,
	"test/parity":           true,
}

// clientGoBoundaryViolation is the build-tag-immune backstop for the two port-boundary
// import gates: capability code must never reach client-go, and only the port and the
// composition roots may hold a cluster client. It parses every file regardless of build
// tags, so a file behind a CI-absent tag cannot silently break the invariant.
func clientGoBoundaryViolation(relative, imported string) string {
	if !strings.HasPrefix(imported, "k8s.io/client-go/") {
		return ""
	}
	slashed := filepath.ToSlash(relative)
	if strings.HasPrefix(slashed, "internal/capabilities/") {
		return "capability code imports client-go " + imported +
			"; all cluster reads go through internal/kube"
	}
	if !clientsetHolderDirs[filepath.ToSlash(filepath.Dir(slashed))] {
		return "imports client-go " + imported +
			" outside the read-only port and composition roots"
	}
	return ""
}

// TestClientGoBoundaryViolation proves the build-tag-immune backstop flags exactly the
// files that break the port-boundary invariants and exempts exactly the holders —
// including a real sibling directory named kube_test, which the packages.Load gate's
// import-path trimming would wrongly exempt.
func TestClientGoBoundaryViolation(t *testing.T) {
	cases := []struct {
		name     string
		relative string
		imported string
		flagged  bool
	}{
		{"port holds the clientset", "internal/kube/clientgo.go", "k8s.io/client-go/kubernetes", false},
		{"relay composition root", "cmd/opencluster-relay/main.go", "k8s.io/client-go/rest", false},
		{"parity harness", "test/parity/main.go", "k8s.io/client-go/kubernetes", false},
		{"capability reaches client-go", "internal/capabilities/kubernetes/runtime/x.go", "k8s.io/client-go/kubernetes", true},
		{"session reaches client-go", "internal/session/x.go", "k8s.io/client-go/rest", true},
		{"sibling kube_test dir is not the port", "internal/kube_test/x.go", "k8s.io/client-go/kubernetes", true},
		{"non-client-go k8s import is fine", "internal/capabilities/kubernetes/runtime/x.go", "k8s.io/api/apps/v1", false},
		{"apimachinery is not client-go", "internal/kube/port.go", "k8s.io/apimachinery/pkg/api/errors", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clientGoBoundaryViolation(tc.relative, tc.imported) != ""
			if got != tc.flagged {
				t.Fatalf("clientGoBoundaryViolation(%q, %q) flagged=%v, want %v",
					tc.relative, tc.imported, got, tc.flagged)
			}
		})
	}
}

func TestNoProcessSpawningSymbolsAndNoCgo(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			name := entry.Name()
			if name == ".git" || name == ".claude" || name == ".agents" || name == ".idea" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			t.Errorf("parsing %s: %v", path, parseErr)
			return nil
		}
		relative, _ := filepath.Rel(root, path)

		for _, importSpec := range file.Imports {
			if importSpec.Path.Value == `"C"` {
				t.Errorf(`%s imports "C"; cgo is banned`, relative)
			}
			// Build-tag-immune import bans: packages.Load honors build constraints, so
			// a file tagged for another GOOS would evade the import-graph gates on a
			// single-platform CI. This textual pass sees every file unconditionally.
			imported := strings.Trim(importSpec.Path.Value, `"`)
			for _, banned := range bannedImports {
				if imported == banned {
					t.Errorf("%s imports banned package %s", relative, banned)
				}
			}
			if boundary := clientGoBoundaryViolation(relative, imported); boundary != "" {
				t.Errorf("%s: %s", relative, boundary)
			}
		}

		for _, violation := range bannedSelectorViolations(file) {
			t.Errorf("%s: %s is banned (process spawning / dynamic loading)", relative, violation)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking repository: %v", err)
	}
}

// TestBannedSelectorViolations_ResolvesImportAliases proves the selector scan cannot be
// evaded by aliasing a restricted package — the exact gap an identifier-only match left
// open. Synthetic sources are parsed in memory so the repo-walk gate never sees them.
func TestBannedSelectorViolations_ResolvesImportAliases(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "direct os.StartProcess",
			src:  "package p\nimport \"os\"\nfunc f() { os.StartProcess(\"\", nil, nil) }\n",
			want: []string{"os.StartProcess"},
		},
		{
			name: "aliased os",
			src:  "package p\nimport osx \"os\"\nfunc f() { osx.StartProcess(\"\", nil, nil) }\n",
			want: []string{"os.StartProcess"},
		},
		{
			name: "aliased syscall",
			src:  "package p\nimport sc \"syscall\"\nfunc f() { sc.ForkExec(\"\", nil, nil) }\n",
			want: []string{"syscall.ForkExec"},
		},
		{
			name: "raw syscall entry point",
			src:  "package p\nimport \"syscall\"\nfunc f() { syscall.RawSyscall(0, 0, 0, 0) }\n",
			want: []string{"syscall.RawSyscall"},
		},
		{
			name: "dot-imported restricted package",
			src:  "package p\nimport . \"os\"\nvar _ = StartProcess\n",
			want: []string{"dot-import of restricted package os"},
		},
		{
			name: "legitimate os use is not flagged",
			src:  "package p\nimport \"os\"\nvar _ = os.Getenv(\"X\")\n",
			want: nil,
		},
		{
			name: "aliased legitimate use is not flagged",
			src:  "package p\nimport foo \"os\"\nvar _ = foo.Getenv(\"X\")\n",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parsing synthetic source: %v", err)
			}
			got := bannedSelectorViolations(file)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("violations = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCapabilityAndPortCodeAvoidsUnsafe(t *testing.T) {
	root := moduleRoot(t)
	for _, directory := range []string{
		filepath.Join(root, "internal", "capabilities"),
		filepath.Join(root, "internal", "kube"),
	} {
		err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") {
				return walkErr
			}
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			content := string(raw)
			relative, _ := filepath.Rel(root, path)
			if strings.Contains(content, `"unsafe"`) {
				t.Errorf("%s imports unsafe; capability and port code must stay memory-safe", relative)
			}
			if strings.Contains(content, "go:linkname") {
				t.Errorf("%s uses go:linkname; private-symbol access is banned", relative)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", directory, err)
		}
	}
}

// Schema-shape gate: capability and transport schemas must stay closed. No dynamic
// payload types (Any/Struct/Value), no maps, and no field whose name suggests a
// command, script, filesystem path, or URL — the constructs a generic-execution
// protocol would need and this protocol must never grow.
var bannedFieldTypes = map[protoreflect.FullName]bool{
	"google.protobuf.Any":       true,
	"google.protobuf.Struct":    true,
	"google.protobuf.Value":     true,
	"google.protobuf.ListValue": true,
}

var bannedFieldNameSegments = map[string]bool{
	"command": true, "script": true, "shell": true, "path": true,
	"url": true, "uri": true, "exec": true, "plugin": true,
}

func TestSchemasCarryNoDynamicOrCommandConstructs(t *testing.T) {
	// Iterate the registry rather than a hand-listed set, so a proto added later is
	// covered automatically — a new file cannot silently escape the schema-shape gate.
	seen := map[protoreflect.FullName]bool{}
	inspected := 0
	protoregistry.GlobalFiles.RangeFiles(func(descriptor protoreflect.FileDescriptor) bool {
		if !strings.HasPrefix(string(descriptor.Package()), "opencluster.relay") {
			return true
		}
		inspected++
		messages := descriptor.Messages()
		for i := 0; i < messages.Len(); i++ {
			inspectMessage(t, messages.Get(i), seen)
		}
		return true
	})
	// The registry is populated by importing the generated package; a zero count means
	// the blank import below was dropped and the gate would vacuously pass.
	if inspected == 0 {
		t.Fatal("no opencluster.relay proto files registered; the gate would be vacuous")
	}
}

func inspectMessage(t *testing.T, message protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool) {
	t.Helper()
	if seen[message.FullName()] {
		return
	}
	seen[message.FullName()] = true

	fields := message.Fields()
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		if field.IsMap() {
			t.Errorf("%s.%s is a map field; schemas are closed — model it as a message",
				message.FullName(), field.Name())
			continue
		}
		for _, segment := range strings.Split(string(field.Name()), "_") {
			if bannedFieldNameSegments[segment] {
				t.Errorf("%s.%s: field name segment %q is banned",
					message.FullName(), field.Name(), segment)
			}
		}
		if field.Kind() == protoreflect.MessageKind {
			nested := field.Message()
			if bannedFieldTypes[nested.FullName()] {
				t.Errorf("%s.%s uses dynamic payload type %s",
					message.FullName(), field.Name(), nested.FullName())
				continue
			}
			if strings.HasPrefix(string(nested.FullName()), "opencluster.relay.") {
				inspectMessage(t, nested, seen)
			}
		}
	}

	nestedMessages := message.Messages()
	for i := 0; i < nestedMessages.Len(); i++ {
		inspectMessage(t, nestedMessages.Get(i), seen)
	}
}
