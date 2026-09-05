package buttonindicator

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Machine identity values that are part of the public plugin contract and must
// not drift. pluginIDValue and pluginVersion come from service.go and are what
// Describe reports at runtime; the manifest files must carry exactly the same
// values.
const (
	manifestAPIVersion = "plugins.cloudpath.dev/v1alpha1"
	entrypointBinary   = "cloud-path-app-button-indicator"
	contributionID     = "button-indicator"
)

// repoFile reads one hand-authored repository file that lives next to the Go
// package.
func repoFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// TestManifestMatchesCode locks the plugin.yaml identity to the values the
// service reports at runtime: id, version, entrypoint, contribution id, and
// the capability requirements. Drift here means the published manifest no
// longer describes the binary.
func TestManifestMatchesCode(t *testing.T) {
	m := repoFile(t, "plugin.yaml")

	if !strings.Contains(m, "id: "+pluginIDValue) {
		t.Fatalf("plugin.yaml id does not match service id %q", pluginIDValue)
	}
	if !strings.Contains(m, "version: "+pluginVersion) {
		t.Fatalf("plugin.yaml version does not match service version %q", pluginVersion)
	}
	if !strings.Contains(m, "apiVersion: "+manifestAPIVersion) {
		t.Fatalf("plugin.yaml apiVersion does not match %q", manifestAPIVersion)
	}
	if !strings.Contains(m, "entrypoint: "+entrypointBinary) {
		t.Fatalf("plugin.yaml entrypoint does not match %q", entrypointBinary)
	}
	if !strings.Contains(m, "id: "+contributionID) {
		t.Fatalf("plugin.yaml contribution id %q missing", contributionID)
	}
	for _, cap := range []string{keyCap, ledCap, buzzerCp} {
		if !strings.Contains(m, cap) {
			t.Fatalf("plugin.yaml missing capability %q", cap)
		}
	}

	// requirements.yaml mirrors the machine manifest
	r := repoFile(t, "requirements.yaml")
	for _, cap := range []string{keyCap, ledCap, buzzerCp} {
		if !strings.Contains(r, cap) {
			t.Fatalf("requirements.yaml missing capability %q", cap)
		}
	}
}

// TestDescribeReportsRequirements locks the descriptor to the manifest
// requirement ids: Binder matching is by capability, but the app rejects any
// binding whose requirement id is not declared here.
func TestDescribeReportsRequirements(t *testing.T) {
	svc := New()
	desc, err := svc.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if desc.ApplicationID != pluginIDValue || desc.Version != pluginVersion {
		t.Fatalf("descriptor identity = %q/%q", desc.ApplicationID, desc.Version)
	}
	want := map[string]string{
		"button-input": keyCap,
		"indicator":    ledCap,
		"sound":        buzzerCp,
	}
	if len(desc.Requirements) != len(want) {
		t.Fatalf("requirements = %+v", desc.Requirements)
	}
	for _, r := range desc.Requirements {
		if want[r.ID] != r.Capability {
			t.Fatalf("requirement %q -> %q, want %q", r.ID, r.Capability, want[r.ID])
		}
	}
	jobIDs := map[string]bool{}
	for _, j := range desc.Jobs {
		jobIDs[j.ID] = true
	}
	if !jobIDs[jobBootstrap] || !jobIDs[jobHeartbeat] {
		t.Fatalf("descriptor jobs missing bootstrap/heartbeat: %+v", desc.Jobs)
	}
}
