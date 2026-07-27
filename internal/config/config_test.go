package config

import (
	"strings"
	"testing"
	"time"
)

// lookupFrom builds the environment lookup the loader consumes.
func lookupFrom(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func minimalEnvironment() map[string]string {
	return map[string]string{
		"RELAY_CONTROL_PLANE_ADDRESS": "control-plane.example:443",
		"RELAY_ORG_ID":                "org-1",
		"RELAY_CREDENTIAL_FILE":       "/var/lib/opencluster-relay/credential.json",
	}
}

func TestLoad_MinimalEnvironmentAppliesDefaults(t *testing.T) {
	cfg, err := Load(lookupFrom(minimalEnvironment()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ControlPlaneAddress != "control-plane.example:443" || cfg.OrgID != "org-1" {
		t.Fatalf("required values not taken: %+v", cfg)
	}
	if cfg.KubeconfigPath != "" {
		t.Fatal("in-cluster configuration must be the default (no kubeconfig path)")
	}
	if cfg.LocalMaxPods != 50 {
		t.Fatalf("default local pod cap must be the schema maximum: %d", cfg.LocalMaxPods)
	}
	if cfg.MaxConcurrentJobs != 4 {
		t.Fatalf("default concurrency: %d", cfg.MaxConcurrentJobs)
	}
	if cfg.HeartbeatInterval != 15*time.Second || cfg.ResendInterval != 10*time.Second {
		t.Fatalf("default intervals: %+v", cfg)
	}
}

func TestLoad_MissingRequiredValuesAreNamed(t *testing.T) {
	for _, missing := range []string{"RELAY_CONTROL_PLANE_ADDRESS", "RELAY_ORG_ID", "RELAY_CREDENTIAL_FILE"} {
		environment := minimalEnvironment()
		delete(environment, missing)

		_, err := Load(lookupFrom(environment))
		if err == nil || !strings.Contains(err.Error(), missing) {
			t.Fatalf("missing %s must fail naming the variable, got %v", missing, err)
		}
	}
}

func TestLoad_AddressMustBeHostPort(t *testing.T) {
	environment := minimalEnvironment()
	environment["RELAY_CONTROL_PLANE_ADDRESS"] = "https://control-plane.example"

	if _, err := Load(lookupFrom(environment)); err == nil {
		t.Fatal("a URL is not a dial address; want an error")
	}
}

func TestLoad_LocalMaxPodsBounds(t *testing.T) {
	for value, wantErr := range map[string]bool{"5": false, "50": false, "0": true, "51": true, "-3": true, "x": true} {
		environment := minimalEnvironment()
		environment["RELAY_LOCAL_MAX_PODS"] = value

		_, err := Load(lookupFrom(environment))
		if gotErr := err != nil; gotErr != wantErr {
			t.Fatalf("RELAY_LOCAL_MAX_PODS=%s: err=%v want error=%v", value, err, wantErr)
		}
	}
}

func TestLoad_ConcurrencyBounds(t *testing.T) {
	for value, wantErr := range map[string]bool{"1": false, "64": false, "0": true, "65": true, "x": true} {
		environment := minimalEnvironment()
		environment["RELAY_MAX_CONCURRENT_JOBS"] = value

		_, err := Load(lookupFrom(environment))
		if gotErr := err != nil; gotErr != wantErr {
			t.Fatalf("RELAY_MAX_CONCURRENT_JOBS=%s: err=%v want error=%v", value, err, wantErr)
		}
	}
}

func TestLoad_IntervalsMustBePositiveDurations(t *testing.T) {
	for _, key := range []string{"RELAY_HEARTBEAT_INTERVAL", "RELAY_RESEND_INTERVAL"} {
		for value, wantErr := range map[string]bool{"5s": false, "0s": true, "-1s": true, "soon": true} {
			environment := minimalEnvironment()
			environment[key] = value

			_, err := Load(lookupFrom(environment))
			if gotErr := err != nil; gotErr != wantErr {
				t.Fatalf("%s=%s: err=%v want error=%v", key, value, err, wantErr)
			}
		}
	}
}

func TestLoad_InitialSPKIPinsMustBeBase64Sha256(t *testing.T) {
	valid := strings.Repeat("A", 43) + "=" // 32 zero bytes, standard base64

	environment := minimalEnvironment()
	environment["RELAY_INITIAL_SPKI_PINS"] = valid + " , " + valid
	cfg, err := Load(lookupFrom(environment))
	if err != nil {
		t.Fatalf("valid pins refused: %v", err)
	}
	if len(cfg.InitialSPKIPins) != 2 || cfg.InitialSPKIPins[0] != valid {
		t.Fatalf("pins not parsed: %+v", cfg.InitialSPKIPins)
	}

	for _, bad := range []string{"not-base64!!", "c2hvcnQ="} {
		environment["RELAY_INITIAL_SPKI_PINS"] = bad
		if _, err := Load(lookupFrom(environment)); err == nil {
			t.Fatalf("pin %q must be refused (not a base64 SHA-256)", bad)
		}
	}
}

func TestLoad_OptionalPathsAreTaken(t *testing.T) {
	environment := minimalEnvironment()
	environment["RELAY_KUBECONFIG"] = "/etc/opencluster-relay/kubeconfig"
	environment["RELAY_BOOTSTRAP_TOKEN_FILE"] = "/run/secrets/bootstrap-token"

	cfg, err := Load(lookupFrom(environment))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.KubeconfigPath != "/etc/opencluster-relay/kubeconfig" ||
		cfg.BootstrapTokenFile != "/run/secrets/bootstrap-token" {
		t.Fatalf("optional paths not taken: %+v", cfg)
	}
}
