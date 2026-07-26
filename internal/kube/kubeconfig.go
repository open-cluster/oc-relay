package kube

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Kubeconfig load failures classify into exactly four operator-facing states. The
// classification is the whole diagnostic: loader errors never quote the file, because a
// kubeconfig's content is credential material.
var (
	ErrKubeconfigMissing      = errors.New("kubeconfig file missing")
	ErrKubeconfigInaccessible = errors.New("kubeconfig file inaccessible")
	ErrKubeconfigMalformed    = errors.New("kubeconfig malformed")
	ErrKubeconfigUnsupported  = errors.New("kubeconfig uses an unsupported construct")
)

// LoadKubeconfig loads the explicit kubeconfig for the self-hosted/test harness path.
// In-cluster ServiceAccount configuration is the production default; this loader exists
// only for deployments beside a self-hosted control plane and for the parity harness.
// The path comes from operator configuration at process start — job arguments can never
// carry a path (the capability schemas have no path fields), so no request can steer
// which file is read.
//
// The file must be fully self-contained: inline certificate/token data only. Constructs
// that would execute code (exec credential plugins, legacy auth-provider plugins), read
// further files (certificate/key/token/CA path references), weaken transport security
// (plaintext servers, insecure-skip-tls-verify, proxies), or escalate identity
// (impersonation, basic auth) are refused outright — a credential loader that can be
// steered into running a binary or walking the filesystem is an execution surface, not
// configuration.
func LoadKubeconfig(path string) (*rest.Config, error) {
	kubeconfig, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return nil, classifyLoadError(err)
	}

	if err := refuseUnsupportedConstructs(kubeconfig); err != nil {
		return nil, err
	}

	if !hasUsableCurrentContext(kubeconfig) {
		return nil, fmt.Errorf("%w: no usable current context", ErrKubeconfigMalformed)
	}

	config, err := clientcmd.NewNonInteractiveClientConfig(
		*kubeconfig, kubeconfig.CurrentContext, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		// The builder's message can echo entry names; classify without quoting it.
		return nil, fmt.Errorf("%w: client configuration could not be built", ErrKubeconfigMalformed)
	}

	// Belt-and-braces: the construct checks above make these unreachable, but a loader
	// whose output could still carry a plugin reference would defeat the point of them.
	if config.ExecProvider != nil || config.AuthProvider != nil {
		return nil, fmt.Errorf("%w: a credential plugin survived construct filtering", ErrKubeconfigUnsupported)
	}
	return config, nil
}

func classifyLoadError(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ErrKubeconfigMissing
	case errors.Is(err, fs.ErrPermission):
		return ErrKubeconfigInaccessible
	default:
		// Parse errors can quote file content; the classification alone is the diagnostic.
		return ErrKubeconfigMalformed
	}
}

// refuseUnsupportedConstructs vets every user and cluster entry in the file, not just
// the selected context — an operator file carrying a dangerous entry anywhere is refused
// whole, so a later context switch cannot quietly activate it. Refusals name only the
// construct kind, never entry names or values.
func refuseUnsupportedConstructs(kubeconfig *clientcmdapi.Config) error {
	for _, user := range kubeconfig.AuthInfos {
		switch {
		case user.Exec != nil:
			return fmt.Errorf("%w: a user entry uses an exec credential plugin", ErrKubeconfigUnsupported)
		case user.AuthProvider != nil:
			return fmt.Errorf("%w: a user entry uses a legacy auth-provider plugin", ErrKubeconfigUnsupported)
		case user.TokenFile != "":
			return fmt.Errorf("%w: a user entry references a token file; inline data is required", ErrKubeconfigUnsupported)
		case user.ClientCertificate != "" || user.ClientKey != "":
			return fmt.Errorf("%w: a user entry references certificate or key files; inline data is required", ErrKubeconfigUnsupported)
		case user.Username != "" || user.Password != "":
			return fmt.Errorf("%w: a user entry uses basic authentication", ErrKubeconfigUnsupported)
		case user.Impersonate != "" || user.ImpersonateUID != "" ||
			len(user.ImpersonateGroups) > 0 || len(user.ImpersonateUserExtra) > 0:
			return fmt.Errorf("%w: a user entry uses impersonation", ErrKubeconfigUnsupported)
		}
	}

	for _, cluster := range kubeconfig.Clusters {
		switch {
		case cluster.CertificateAuthority != "":
			return fmt.Errorf("%w: a cluster entry references a certificate-authority file; inline data is required", ErrKubeconfigUnsupported)
		case cluster.ProxyURL != "":
			return fmt.Errorf("%w: a cluster entry routes through a proxy", ErrKubeconfigUnsupported)
		case cluster.InsecureSkipTLSVerify:
			return fmt.Errorf("%w: a cluster entry disables TLS verification", ErrKubeconfigUnsupported)
		case !strings.HasPrefix(cluster.Server, "https://"):
			return fmt.Errorf("%w: a cluster entry uses a non-TLS server address", ErrKubeconfigUnsupported)
		}
	}
	return nil
}

func hasUsableCurrentContext(kubeconfig *clientcmdapi.Config) bool {
	if kubeconfig.CurrentContext == "" {
		return false
	}
	context, ok := kubeconfig.Contexts[kubeconfig.CurrentContext]
	if !ok {
		return false
	}
	_, hasCluster := kubeconfig.Clusters[context.Cluster]
	_, hasUser := kubeconfig.AuthInfos[context.AuthInfo]
	return hasCluster && hasUser
}
