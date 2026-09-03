package scheduledcompartment

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Machine identity values that are part of the public plugin contract and must
// not drift. pluginIDValue and pluginVersion come from service.go and are what
// Describe reports at runtime; the manifest files must carry exactly the same
// values.
const (
	manifestAPIVersion = "plugins.cloudpath.dev/v1alpha1"
	entrypointBinary   = "cloud-path-app-scheduled-compartment"
	contributionID     = "scheduled-compartment"
)

// repoFile reads one hand-authored repository file that lives next to the Go
// package. Tests run with the package directory as the working directory, so
// the names are stable regardless of where the repository is checked out.
func repoFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// TestManifestMachineIdentity locks the machine identity fields that the
// Registry, Host and CLI consume. id, version and entrypoint are identity, not
// display text: changing them changes install identity, so this test turns any
// accidental edit into a visible failure instead of a silent rename.
func TestManifestMachineIdentity(t *testing.T) {
	m := repoFile(t, "plugin.yaml")
	for _, want := range []string{
		"apiVersion: " + manifestAPIVersion,
		"kind: Application",
		"id: " + pluginIDValue,
		"version: " + pluginVersion,
		"protocol: 1",
		"entrypoint: " + entrypointBinary,
		"- id: " + contributionID,
	} {
		if !strings.Contains(m, want) {
			t.Fatalf("plugin.yaml is missing the exact line %q", want)
		}
	}
}

// manifestRequirement is the requirements block of plugin.yaml /
// requirements.yaml.
type manifestRequirement struct {
	ID          string
	Capability  string
	Cardinality string
	MinItems    string
}

// parseRequirementBlocks parses the hand-authored `requirements:` block of
// plugin.yaml and requirements.yaml. The files keep a fixed two-space YAML
// shape on purpose: this parser fails loudly when that shape changes, so
// manifest edits stay deliberate and reviewed.
func parseRequirementBlocks(t *testing.T, file, content string) []manifestRequirement {
	t.Helper()
	var (
		out     []manifestRequirement
		cur     manifestRequirement
		inBlock bool
		seen    bool
	)
	flush := func() {
		if cur.ID != "" || cur.Capability != "" || cur.Cardinality != "" || cur.MinItems != "" {
			out = append(out, cur)
		}
		cur = manifestRequirement{}
	}
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimRight(raw, "\r")
		if !inBlock {
			if strings.TrimSpace(line) == "requirements:" {
				inBlock = true
				seen = true
			}
			continue
		}
		if line != "" && !strings.HasPrefix(line, " ") {
			// The first unindented line after the block ends it.
			flush()
			inBlock = false
			continue
		}
		switch {
		case strings.HasPrefix(line, "  - id: "):
			flush()
			cur.ID = strings.TrimSpace(strings.TrimPrefix(line, "  - id: "))
		case strings.HasPrefix(line, "    capability: "):
			cur.Capability = strings.TrimSpace(strings.TrimPrefix(line, "    capability: "))
		case strings.HasPrefix(line, "    cardinality: "):
			cur.Cardinality = strings.TrimSpace(strings.TrimPrefix(line, "    cardinality: "))
		case strings.HasPrefix(line, "    minItems: "):
			cur.MinItems = strings.TrimSpace(strings.TrimPrefix(line, "    minItems: "))
		}
	}
	flush()
	if !seen {
		t.Fatalf("%s: missing requirements: block", file)
	}
	return out
}

// TestManifestRequirementsMirror pins the three source-of-truth copies
// together: plugin.yaml, requirements.yaml and the ApplicationDescriptor
// returned by Describe. A change in one without the others fails here instead
// of silently drifting at install time.
func TestManifestRequirementsMirror(t *testing.T) {
	manifest := parseRequirementBlocks(t, "plugin.yaml", repoFile(t, "plugin.yaml"))
	mirror := parseRequirementBlocks(t, "requirements.yaml", repoFile(t, "requirements.yaml"))

	if len(manifest) != 3 {
		t.Fatalf("plugin.yaml: %d requirements, want 3 (%+v)", len(manifest), manifest)
	}
	if len(mirror) != 3 {
		t.Fatalf("requirements.yaml: %d requirements, want 3 (%+v)", len(mirror), mirror)
	}
	for i := range manifest {
		if manifest[i] != mirror[i] {
			t.Fatalf("requirement %d drift:\n  plugin.yaml:       %+v\n  requirements.yaml: %+v",
				i, manifest[i], mirror[i])
		}
	}

	desc, err := New().Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if desc.ApplicationID != pluginIDValue || desc.Version != pluginVersion {
		t.Fatalf("descriptor identity = (%s, %s), want (%s, %s)",
			desc.ApplicationID, desc.Version, pluginIDValue, pluginVersion)
	}
	byID := make(map[string]manifestRequirement, len(manifest))
	for _, r := range manifest {
		byID[r.ID] = r
	}
	if len(desc.Requirements) != len(byID) {
		t.Fatalf("descriptor has %d requirements, manifest has %d", len(desc.Requirements), len(byID))
	}
	for _, r := range desc.Requirements {
		want, ok := byID[r.ID]
		if !ok {
			t.Fatalf("descriptor requirement %q is missing from plugin.yaml", r.ID)
		}
		if want.Capability != r.Capability || want.Cardinality != r.Cardinality {
			t.Fatalf("descriptor requirement %q = (%s, %s), manifest = (%s, %s)",
				r.ID, r.Capability, r.Cardinality, want.Capability, want.Cardinality)
		}
		wantMin := uint32(0)
		if want.MinItems != "" {
			n, err := strconv.ParseUint(want.MinItems, 10, 32)
			if err != nil {
				t.Fatalf("manifest requirement %q: minItems %q is not an integer", r.ID, want.MinItems)
			}
			wantMin = uint32(n)
		}
		if r.MinItems != wantMin {
			t.Fatalf("descriptor requirement %q MinItems = %d, manifest = %d", r.ID, r.MinItems, wantMin)
		}
	}
	if len(desc.Jobs) != 1 || desc.Jobs[0].ID != jobWindowCheck {
		t.Fatalf("descriptor jobs = %+v, want a single %q job", desc.Jobs, jobWindowCheck)
	}
}
