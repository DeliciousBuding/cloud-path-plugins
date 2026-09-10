package scheduledcompartment

import (
	"context"
	"encoding/json"
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
		`core: ">=0.2.29 <0.3.0"`,
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

func TestManifestUIContribution(t *testing.T) {
	m := strings.ReplaceAll(repoFile(t, "plugin.yaml"), "\r\n", "\n")
	for _, want := range []string{
		"      ui:\n        apiVersion: 1",
		"        navigation:\n          title: 药盒提醒\n          icon: pill\n          order: 30\n          route: pillbox\n          visibility: instance-enabled",
		"        pages:\n          - id: home\n            title: 药盒提醒",
		"              - type: status\n                source: instance",
		"              - type: metrics\n                source: records\n                recordType: window",
		"              - type: actions\n                source: manual-jobs",
		"              - type: records\n                source: records\n                recordType: window\n                presentation: timeline",
		"              - type: schedule\n                source: jobs",
		"              - type: form\n                source: config",
		"                fields:",
		"                  - key: app_config.compartments",
		"                    type: array",
		"                    minItems: 1",
		"                    itemFields:",
		"                      - key: id",
		"                      - key: name",
		"                  - key: app_config.schedule",
		"                        label: 药格 ID",
		"                        description: 必须引用上方 compartments[].id 中已配置的值，例如 medicine。",
		"                      - key: start",
		"                        label: 开始时间",
		"                        placeholder: \"08:00\"",
		"                      - key: end",
		"                        label: 结束时间",
		"                        placeholder: \"08:30\"",
		"                  - key: app_config.timezone",
		"                  - key: app_config.reminder.freq",
		"                  - key: app_config.reminder.duration",
	} {
		if !strings.Contains(m, want) {
			t.Fatalf("plugin.yaml missing UI contract %q", want)
		}
	}
	scheduleStart := strings.Index(m, "              - type: schedule")
	formStart := -1
	if scheduleStart >= 0 {
		formStart = strings.Index(m[scheduleStart:], "              - type: form")
	}
	if scheduleStart < 0 || formStart < 0 {
		t.Fatal("schedule/form section boundary missing")
	}
	scheduleBlock := m[scheduleStart : scheduleStart+formStart]
	if !strings.Contains(scheduleBlock, "\n                source: jobs") || strings.Contains(scheduleBlock, "\n                recordType:") {
		t.Fatal("schedule section must use source: jobs and no recordType")
	}
	if strings.Contains(m, "type: custom") {
		t.Fatal("scheduled-compartment must use declarative sections, not arbitrary custom UI")
	}
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
	if len(desc.Jobs) != 3 {
		t.Fatalf("expected three jobs, got %+v", desc.Jobs)
	}
	expected := map[string]bool{jobWindowCheck: false, jobStartReminder: true, jobConfirmWindow: true}
	for _, job := range desc.Jobs {
		manual, ok := expected[job.ID]
		if !ok || job.ManualOnly != manual || strings.TrimSpace(job.Title) == "" {
			t.Fatalf("invalid job descriptor: %+v", job)
		}
		delete(expected, job.ID)
		var schema map[string]any
		if err := json.Unmarshal([]byte(job.InputSchemaJSON), &schema); err != nil || schema["type"] != "object" {
			t.Fatalf("invalid job schema: %+v, err=%v", job, err)
		}
	}
	if len(expected) != 0 {
		t.Fatalf("missing jobs: %v", expected)
	}
	if !strings.Contains(repoFile(t, "go.mod"), "github.com/DeliciousBuding/cloud-path v0.2.15") {
		t.Fatal("SDK dependency must match the minimum Core version")
	}
}

// TestManifestExplainsDisplayCodesAndWindowRecords locks the usability contract
// the audit asked for: the digit-display states get a Chinese, read-only
// section, the window records expose their copyable id, and the per-minute
// check task stops pretending to be the daily schedule.
func TestManifestExplainsDisplayCodesAndWindowRecords(t *testing.T) {
	m := strings.ReplaceAll(repoFile(t, "plugin.yaml"), "\r\n", "\n")
	for _, want := range []string{
		"              - type: metrics\n                source: records\n                recordType: display",
		"                      reminder: 提醒中（待确认）",
		"                      missed: 已超时未确认",
		"                      idle: 显示时钟",
		"                  - key: id\n                    label: 记录编号 / 窗口编号",
		"                title: 窗口检查任务",
		"                emptyText: 当前没有正在运行的检查任务。",
	} {
		if !strings.Contains(m, want) {
			t.Fatalf("plugin.yaml missing usability contract %q", want)
		}
	}
	for _, dishonest := range []string{"自动提醒计划", "还没有设置自动提醒计划"} {
		if strings.Contains(m, dishonest) {
			t.Fatalf("plugin.yaml still labels the minute-check task as %q", dishonest)
		}
	}
}

// TestManualJobSchemasMakeWindowIDOptionalAndGuideButtonFirst pins the dashboard
// contract: start-reminder no longer forces operators to invent an id, while
// confirm-window keeps the exact id and points at the physical key first.
func TestManualJobSchemasMakeWindowIDOptionalAndGuideButtonFirst(t *testing.T) {
	desc, err := New().Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	type property struct{ Title, Description string }
	schemas := map[string]struct {
		Required   []string            `json:"required"`
		Properties map[string]property `json:"properties"`
	}{}
	for _, job := range desc.Jobs {
		var schema struct {
			Required   []string            `json:"required"`
			Properties map[string]property `json:"properties"`
		}
		if err := json.Unmarshal([]byte(job.InputSchemaJSON), &schema); err != nil {
			t.Fatalf("%s schema: %v", job.ID, err)
		}
		schemas[job.ID] = schema
	}

	start := schemas[jobStartReminder]
	for _, required := range start.Required {
		if required == "window_id" {
			t.Fatal("start-reminder still requires callers to invent window_id")
		}
	}
	if !strings.Contains(start.Properties["window_id"].Title, "可选") ||
		!strings.Contains(start.Properties["window_id"].Description, "服务端") {
		t.Fatalf("start-reminder window_id help does not explain server generation: %+v", start.Properties["window_id"])
	}

	confirm := schemas[jobConfirmWindow]
	if !strings.Contains(confirm.Properties["window_id"].Description, "直接按盒子上的确认按键") {
		t.Fatalf("confirm-window does not point at the physical key first: %q", confirm.Properties["window_id"].Description)
	}
	for _, required := range confirm.Required {
		if required == "window_id" {
			return
		}
	}
	t.Fatal("confirm-window must keep requiring the exact window_id")
}

// TestReadmeExplainsDigitCodesAndManualFallback keeps the two audit-facing
// explanations (digit codes, button-first confirmation) in the README.
func TestReadmeExplainsDigitCodesAndManualFallback(t *testing.T) {
	readme := strings.ReplaceAll(repoFile(t, "README.md"), "\r\n", "\n")
	for _, want := range []string{
		"00000001", "00000002", "提醒中（待确认）", "已超时未确认", "显示时钟",
		"window_id 可省略：服务端会生成唯一编号", "直接按盒子上的确认按键",
	} {
		if !strings.Contains(readme, want) {
			t.Fatalf("README missing usability explanation %q", want)
		}
	}
}
