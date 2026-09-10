package crd

import (
	"strings"
	"testing"
)

func TestEmbeddedDefinitionsParse(t *testing.T) {
	definitions, err := Definitions()
	if err != nil {
		t.Fatalf("Definitions: %v", err)
	}
	if len(definitions) == 0 {
		t.Fatal("the binary must carry its own CRDs; that is what makes installing them at start-up possible")
	}

	var found bool
	for _, definition := range definitions {
		if definition.GetKind() != "CustomResourceDefinition" {
			t.Fatalf("kind = %q", definition.GetKind())
		}
		if definition.GetName() == "sandmandevices.sandman.hauke.cloud" {
			found = true

			group, _, err := unstructuredString(definition.Object, "spec", "group")
			if err != nil {
				t.Fatal(err)
			}
			if group != "sandman.hauke.cloud" {
				t.Fatalf("group = %q", group)
			}
			scope, _, err := unstructuredString(definition.Object, "spec", "scope")
			if err != nil {
				t.Fatal(err)
			}
			if scope != "Cluster" {
				t.Fatalf("scope = %q, want Cluster: a device describes a machine behind a cluster-scoped Node", scope)
			}
		}
	}
	if !found {
		t.Fatal("the SandmanDevice definition is missing from the binary")
	}
}

// TestGeneratedCRDIsCurrent is a cheap guard against the embedded manifest
// drifting from the Go types: every field the API adds has to survive a round
// trip through the API server, and a stale CRD drops it silently.
func TestEmbeddedDefinitionCarriesTheSpecFields(t *testing.T) {
	definitions, err := Definitions()
	if err != nil {
		t.Fatalf("Definitions: %v", err)
	}

	raw, err := definitions[0].MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	manifest := string(raw)

	for _, field := range []string{
		"nodeRef", "wakeOnLAN", "macAddress", "secureOnSecretRef",
		"desiredPowerState", "shutdown", "drain", "timeouts", "paused",
		"phase", "nodeReady", "lastRequestID",
	} {
		if !strings.Contains(manifest, field) {
			t.Fatalf("the embedded CRD has no %q; run `make manifests`", field)
		}
	}
}

func unstructuredString(object map[string]any, path ...string) (value string, found bool, err error) {
	current := any(object)
	for _, key := range path {
		mapping, ok := current.(map[string]any)
		if !ok {
			return "", false, nil
		}
		current, ok = mapping[key]
		if !ok {
			return "", false, nil
		}
	}
	value, found = current.(string)
	return value, found, nil
}
