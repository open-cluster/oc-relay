package config

import (
	"testing"
	"time"

	"github.com/open-cluster/oc-relay/internal/capabilities/kubernetes/events"
	"github.com/open-cluster/oc-relay/internal/capabilities/kubernetes/logs"
	"github.com/open-cluster/oc-relay/internal/capabilities/kubernetes/runtime"
)

// The ceilings here are stated as literals so this package stays a leaf, which means they
// are a copy — and a copy of a bound is where a bound goes to drift. This pins each one
// against the capability constant it mirrors, so raising a schema maximum without raising
// the configuration's fails here rather than becoming a value an operator cannot set.
func TestVolumeCeilingsMatchTheCapabilitySchemaMaximums(t *testing.T) {
	for name, pair := range map[string]struct{ configured, compiled int64 }{
		"pods":      {maxLocalPods, runtime.MaxWorkloadPods},
		"events":    {maxLocalEvents, events.MaxEvents},
		"log lines": {maxLocalLogLines, logs.MaxLogLines},
		"log bytes": {maxLocalLogBytes, logs.MaxLogBytes},
	} {
		if pair.configured != pair.compiled {
			t.Errorf("the %s ceiling is %d here and %d in the capability; a local cap must "+
				"be settable up to exactly what the schema allows",
				name, pair.configured, pair.compiled)
		}
	}
}

// The default retention has to be the one the events capability assumes, or an operator who
// configured nothing gets a different answer depending on which half of the code decided.
func TestDefaultEventRetentionMatchesTheCapabilityDefault(t *testing.T) {
	cfg, err := Load(lookupFrom(minimalEnvironment()))
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if cfg.EventRetention != events.DefaultRetentionHorizon {
		t.Fatalf("default retention is %v here and %v in the capability",
			cfg.EventRetention, events.DefaultRetentionHorizon)
	}
}

func TestLoad_VolumeCapsAreBoundedAndRefuseWhatTheSchemaCannotServe(t *testing.T) {
	for key, ceiling := range map[string]int64{
		"RELAY_LOCAL_MAX_EVENTS":    maxLocalEvents,
		"RELAY_LOCAL_MAX_LOG_LINES": maxLocalLogLines,
		"RELAY_LOCAL_MAX_LOG_BYTES": maxLocalLogBytes,
	} {
		for _, value := range []string{"0", "-1", "not-a-number"} {
			environment := minimalEnvironment()
			environment[key] = value
			if _, err := Load(lookupFrom(environment)); err == nil {
				t.Errorf("%s=%q must be refused", key, value)
			}
		}
		// Above the schema maximum is refused rather than clamped: an operator who typed a
		// number the product cannot serve should be told, not quietly given a smaller one.
		environment := minimalEnvironment()
		environment[key] = itoa(ceiling + 1)
		if _, err := Load(lookupFrom(environment)); err == nil {
			t.Errorf("%s above the schema maximum must be refused", key)
		}
	}
}

func TestLoad_ACapBelowTheCapabilityFloorIsAcceptedBecauseNarrowingIsAlwaysAllowed(t *testing.T) {
	environment := minimalEnvironment()
	environment["RELAY_LOCAL_MAX_LOG_LINES"] = "1"

	cfg, err := Load(lookupFrom(environment))
	if err != nil {
		t.Fatalf("an operator may always ask for less: %v", err)
	}
	if cfg.LocalMaxLogLines != 1 {
		t.Fatalf("the configured cap must survive, got %d", cfg.LocalMaxLogLines)
	}
}

func TestLoad_NamespaceAllowlistIsParsedAndValidated(t *testing.T) {
	environment := minimalEnvironment()
	environment["RELAY_ALLOWED_NAMESPACES"] = " shop , payments "

	cfg, err := Load(lookupFrom(environment))
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if len(cfg.AllowedNamespaces) != 2 || !cfg.AllowedNamespaces["shop"] ||
		!cfg.AllowedNamespaces["payments"] {
		t.Fatalf("the allowlist must be the set the operator wrote, got %v", cfg.AllowedNamespaces)
	}
}

func TestLoad_AnUnsetAllowlistNarrowsNothingRatherThanEverything(t *testing.T) {
	cfg, err := Load(lookupFrom(minimalEnvironment()))
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if cfg.AllowedNamespaces != nil {
		t.Fatal("an operator who set no allowlist has not excluded every namespace; " +
			"reading it the other way would disable every capability on an empty variable")
	}
}

func TestLoad_AnAllowlistEntryThatIsNotANamespaceFailsAtStartup(t *testing.T) {
	for _, value := range []string{"Shop", "shop/prod", "a_b", ","} {
		environment := minimalEnvironment()
		environment["RELAY_ALLOWED_NAMESPACES"] = value
		if _, err := Load(lookupFrom(environment)); err == nil {
			t.Errorf("RELAY_ALLOWED_NAMESPACES=%q must fail at startup, not silently refuse "+
				"reads the operator believes they permitted", value)
		}
	}
}

func TestLoad_EventRetentionMustBeAPositiveDuration(t *testing.T) {
	for _, value := range []string{"0s", "-1h", "soon"} {
		environment := minimalEnvironment()
		environment["RELAY_EVENT_RETENTION"] = value
		if _, err := Load(lookupFrom(environment)); err == nil {
			t.Errorf("RELAY_EVENT_RETENTION=%q must be refused", value)
		}
	}

	environment := minimalEnvironment()
	environment["RELAY_EVENT_RETENTION"] = "3h"
	cfg, err := Load(lookupFrom(environment))
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if cfg.EventRetention != 3*time.Hour {
		t.Fatalf("the attested horizon must survive, got %v", cfg.EventRetention)
	}
}

func itoa(value int64) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
