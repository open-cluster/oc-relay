package kube

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeKubeconfig writes content to a fresh temp file and returns its path.
func writeKubeconfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing kubeconfig fixture: %v", err)
	}
	return path
}

// assertNoLeak fails if the error surfaces any of the given file-content canaries.
// Loader errors classify; they never quote the kubeconfig.
func assertNoLeak(t *testing.T, err error, canaries ...string) {
	t.Helper()
	if err == nil {
		return
	}
	text := err.Error()
	for _, canary := range canaries {
		if strings.Contains(text, canary) {
			t.Fatalf("error leaks kubeconfig content %q: %v", canary, err)
		}
	}
}

const validInlineKubeconfig = `
apiVersion: v1
kind: Config
clusters:
- name: harness
  cluster:
    server: https://127.0.0.1:6443
    certificate-authority-data: Y2EtZGF0YQ==
users:
- name: harness
  user:
    token: harness-token-canary
contexts:
- name: harness
  context:
    cluster: harness
    user: harness
current-context: harness
`

func TestLoadKubeconfig_ValidInlineConfigLoads(t *testing.T) {
	path := writeKubeconfig(t, validInlineKubeconfig)

	config, err := LoadKubeconfig(path)
	if err != nil {
		t.Fatalf("LoadKubeconfig: %v", err)
	}
	if config.Host != "https://127.0.0.1:6443" {
		t.Fatalf("host not taken from the selected cluster: %q", config.Host)
	}
	if config.BearerToken != "harness-token-canary" {
		t.Fatal("inline token must be usable for the harness")
	}
}

func TestLoadKubeconfig_MissingFileIsClassified(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := LoadKubeconfig(path)
	if !errors.Is(err, ErrKubeconfigMissing) {
		t.Fatalf("want ErrKubeconfigMissing, got %v", err)
	}
}

func TestLoadKubeconfig_MalformedFileIsClassifiedWithoutQuotingContent(t *testing.T) {
	path := writeKubeconfig(t, "not-yaml: [unclosed\nsecret-canary-value")

	_, err := LoadKubeconfig(path)
	if !errors.Is(err, ErrKubeconfigMalformed) {
		t.Fatalf("want ErrKubeconfigMalformed, got %v", err)
	}
	assertNoLeak(t, err, "secret-canary-value", "not-yaml")
}

func TestLoadKubeconfig_RefusedConstructs(t *testing.T) {
	cases := []struct {
		name     string
		fragment string // spliced into the user or cluster entry
		inUser   bool
		canaries []string
	}{
		{
			name: "exec credential plugin",
			fragment: `    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: /usr/local/bin/plugin-canary
      interactiveMode: Never`,
			inUser:   true,
			canaries: []string{"plugin-canary"},
		},
		{
			name: "legacy auth-provider plugin",
			fragment: `    auth-provider:
      name: oidc
      config:
        id-token: idtoken-canary`,
			inUser:   true,
			canaries: []string{"idtoken-canary", "oidc"},
		},
		{
			name:     "token file reference",
			fragment: `    tokenFile: /var/run/secrets/token-canary`,
			inUser:   true,
			canaries: []string{"token-canary"},
		},
		{
			name: "client certificate file reference",
			fragment: `    client-certificate: /etc/certs/cert-canary.pem
    client-key: /etc/certs/key-canary.pem`,
			inUser:   true,
			canaries: []string{"cert-canary", "key-canary"},
		},
		{
			name: "basic auth",
			fragment: `    username: user-canary
    password: password-canary`,
			inUser:   true,
			canaries: []string{"user-canary", "password-canary"},
		},
		{
			name:     "impersonation",
			fragment: `    as: admin-canary`,
			inUser:   true,
			canaries: []string{"admin-canary"},
		},
		{
			name:     "certificate authority file reference",
			fragment: `    certificate-authority: /etc/certs/ca-canary.pem`,
			inUser:   false,
			canaries: []string{"ca-canary"},
		},
		{
			name:     "proxy url",
			fragment: `    proxy-url: http://proxy-canary:3128`,
			inUser:   false,
			canaries: []string{"proxy-canary"},
		},
		{
			name:     "insecure skip tls verify",
			fragment: `    insecure-skip-tls-verify: true`,
			inUser:   false,
			canaries: nil,
		},
		{
			name:     "plaintext server",
			fragment: ``, // the server URL itself is the violation
			inUser:   false,
			canaries: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := "https://127.0.0.1:6443"
			if tc.name == "plaintext server" {
				server = "http://127.0.0.1:8080"
			}
			userExtra, clusterExtra := "", ""
			if tc.inUser {
				userExtra = "\n" + tc.fragment
			} else if tc.fragment != "" {
				clusterExtra = "\n" + tc.fragment
			}
			content := `
apiVersion: v1
kind: Config
clusters:
- name: harness
  cluster:
    server: ` + server + clusterExtra + `
users:
- name: harness
  user:
    token: harness-token` + userExtra + `
contexts:
- name: harness
  context:
    cluster: harness
    user: harness
current-context: harness
`
			path := writeKubeconfig(t, content)

			_, err := LoadKubeconfig(path)
			if !errors.Is(err, ErrKubeconfigUnsupported) {
				t.Fatalf("want ErrKubeconfigUnsupported, got %v", err)
			}
			assertNoLeak(t, err, append(tc.canaries, "harness-token")...)
		})
	}
}

func TestLoadKubeconfig_NoCurrentContextIsMalformed(t *testing.T) {
	content := `
apiVersion: v1
kind: Config
clusters:
- name: harness
  cluster:
    server: https://127.0.0.1:6443
users:
- name: harness
  user:
    token: harness-token
contexts:
- name: harness
  context:
    cluster: harness
    user: harness
`
	path := writeKubeconfig(t, content)

	_, err := LoadKubeconfig(path)
	if !errors.Is(err, ErrKubeconfigMalformed) {
		t.Fatalf("want ErrKubeconfigMalformed for a config with no usable context, got %v", err)
	}
	assertNoLeak(t, err, "harness-token")
}
