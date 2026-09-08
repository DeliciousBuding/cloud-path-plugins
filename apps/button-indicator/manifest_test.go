package buttonindicator

import (
	"context"
	"encoding/json"
	"fmt"
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

	if !strings.Contains(repoFile(t, "README.md"), "Version **"+pluginVersion+"**") {
		t.Fatal("README.md current version does not match the service")
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
		"button-input":      keyCap,
		"indicator":         ledCap,
		"sound":             buzzerCp,
		"acknowledge-input": keyCap,
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
	// Only bootstrap is auto-dispatched. Heartbeat remains cron-owned; the
	// other descriptors exist for the generic management-console operation UI.
	if len(desc.Jobs) != 3 || !jobIDs[jobBootstrap] || !jobIDs[jobRequest] || !jobIDs[jobAcknowledge] || jobIDs[jobHeartbeat] {
		t.Fatalf("unexpected descriptor jobs: %+v", desc.Jobs)
	}
	for _, job := range desc.Jobs {
		if job.ManualOnly != (job.ID != jobBootstrap) {
			t.Fatalf("unsafe automatic dispatch for job: %+v", job)
		}
		if !json.Valid([]byte(job.InputSchemaJSON)) || job.Title == "" {
			t.Fatalf("job missing usable schema/title: %+v", job)
		}
	}
}

// Compare all three declarations, including cardinalities/minItems. Checking
// only capability strings misses drift when two requirements share key@1.
func TestRequirementDeclarationsStayInSync(t *testing.T) {
	desc, err := New().Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, r := range desc.Requirements {
		want = append(want, "- id: "+r.ID, "capability: "+r.Capability, "cardinality: "+r.Cardinality)
		if r.MinItems != 0 {
			want = append(want, fmt.Sprintf("minItems: %d", r.MinItems))
		}
	}
	for _, name := range []string{"plugin.yaml", "requirements.yaml"} {
		var got []string
		inRequirements := false
		for _, line := range strings.Split(repoFile(t, name), "\n") {
			line = strings.TrimSuffix(line, "\r")
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if line == "requirements:" {
				inRequirements = true
				continue
			}
			if !inRequirements {
				continue
			}
			if !strings.HasPrefix(line, " ") {
				break
			}
			got = append(got, trimmed)
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("%s requirements differ from Describe:\ngot: %v\nwant: %v", name, got, want)
		}
	}
	if !strings.Contains(repoFile(t, "plugin.yaml"), `core: ">=0.2.15 <0.3.0"`) {
		t.Fatal("manual jobs must not install on a Core that auto-runs every job")
	}
	if !strings.Contains(repoFile(t, "go.mod"), "github.com/DeliciousBuding/cloud-path v0.2.15") {
		t.Fatal("public SDK dependency must provide ManualOnly")
	}
}

func TestServiceCallJobInputSchemas(t *testing.T) {
	desc, err := New().Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range desc.Jobs {
		if job.ID == jobBootstrap {
			continue
		}
		var schema struct {
			Type       string `json:"type"`
			Properties map[string]struct {
				Type      string          `json:"type"`
				Title     string          `json:"title"`
				Const     *bool           `json:"const"`
				MinLength int             `json:"minLength"`
				MaxLength int             `json:"maxLength"`
				Pattern   json.RawMessage `json:"pattern"`
			} `json:"properties"`
			Required             []string `json:"required"`
			AdditionalProperties *bool    `json:"additionalProperties"`
		}
		if err := json.Unmarshal([]byte(job.InputSchemaJSON), &schema); err != nil {
			t.Fatal(err)
		}
		if schema.Type != "object" || schema.AdditionalProperties == nil || *schema.AdditionalProperties || len(schema.Required) != 1 {
			t.Fatalf("job has an unbounded/non-object schema: %+v", job)
		}
		if job.ID == jobRequest {
			confirm, note := schema.Properties["confirm"], schema.Properties["note"]
			if len(schema.Properties) != 2 || schema.Required[0] != "confirm" || confirm.Type != "boolean" || confirm.Const == nil || !*confirm.Const || note.Type != "string" || note.MaxLength != 256 {
				t.Fatalf("request schema disagrees with validation: %+v", schema)
			}
		} else {
			id := schema.Properties["request_id"]
			if len(schema.Properties) != 1 || schema.Required[0] != "request_id" || id.Type != "string" || id.MinLength != 1 || id.MaxLength != 128 {
				t.Fatalf("acknowledge schema disagrees with validation: %+v", schema)
			}
		}
		for field, property := range schema.Properties {
			if property.Pattern != nil {
				t.Fatalf("%s.%s must omit pattern to retain the console text form", job.ID, field)
			}
			if property.Title == "" {
				t.Fatalf("%s.%s has no understandable form label", job.ID, field)
			}
		}
	}
}
