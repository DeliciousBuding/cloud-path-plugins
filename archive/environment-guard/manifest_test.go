package environmentguard

import (
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func readJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

type manifestRequirement struct {
	ID          string `json:"id"`
	Capability  string `json:"capability"`
	Cardinality string `json:"cardinality"`
}

const environmentUIJSON = `{"apiVersion":1,"navigation":{"title":"环境监测","icon":"leaf","order":30,"route":"environment","visibility":"instance-enabled"},"pages":[{"id":"home","title":"环境监测","sections":[{"type":"status","source":"instance"},{"type":"metrics","source":"records","recordType":"environment"},{"type":"actions","source":"manual-jobs"},{"type":"records","source":"records","recordType":"alert","presentation":"timeline"},{"type":"form","source":"config","fields":[{"key":"app_config.timezone","label":"时区","type":"string","description":"UTC 或明确的 IANA 时区名称；不要使用 Local。","placeholder":"Asia/Shanghai","default":"UTC"},{"key":"app_config.temperature_min","label":"温度下限","type":"number","description":"原始温度读数的下限，必须小于温度上限。","default":18},{"key":"app_config.temperature_max","label":"温度上限","type":"number","description":"原始温度读数的上限，必须大于温度下限。","default":28},{"key":"app_config.light_threshold","label":"光照阈值","type":"number","description":"留空表示关闭光照阈值，仅显示原始读数。","placeholder":"例如 300"},{"key":"app_config.light_alert_when","label":"光照告警方向","type":"select","description":"仅表示数值比较方向，不代表物理明暗已标定。","enum":["below","above"],"default":"below"},{"key":"app_config.hysteresis.temperature","label":"温度回差","type":"number","description":"恢复区间宽度；必须小于温度上下限差的一半。","minimum":0,"default":1},{"key":"app_config.hysteresis.light","label":"光照回差","type":"number","description":"与光照原始读数同单位。","minimum":0,"default":5},{"key":"app_config.stale_after_s","label":"过期时间（秒）","type":"integer","description":"超过该时间没有新观测时标记为过期。","minimum":60,"maximum":86400,"default":120},{"key":"app_config.temperature_unit","label":"温度单位","type":"string","description":"可选；缺失单位时的回退值，不做换算。","placeholder":"例如 C"},{"key":"app_config.light_unit","label":"光照单位","type":"string","description":"可选；缺失单位时的回退值，不做换算。","placeholder":"例如 lux"}]}]}]}`

func TestManifestDescriptorAndRequirementMirror(t *testing.T) {
	var manifest struct {
		API           string `json:"apiVersion"`
		Kind          string `json:"kind"`
		ID            string `json:"id"`
		Version       string `json:"version"`
		Protocol      int    `json:"protocol"`
		Entrypoint    string `json:"entrypoint"`
		Compatibility struct {
			Core string `json:"core"`
		} `json:"compatibility"`
		Permissions  map[string][]string   `json:"permissions"`
		Requirements []manifestRequirement `json:"requirements"`
		Contributes  struct {
			Applications []struct {
				ID    string          `json:"id"`
				Title string          `json:"title"`
				UI    json.RawMessage `json:"ui"`
			} `json:"applications"`
		} `json:"contributes"`
	}
	readJSON(t, "plugin.yaml", &manifest)
	var mirror struct {
		Requirements []manifestRequirement `json:"requirements"`
	}
	readJSON(t, "requirements.yaml", &mirror)
	descriptor, err := New().Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if manifest.API != "plugins.cloudpath.dev/v1alpha1" || manifest.Kind != "Application" || manifest.ID != ApplicationID() || manifest.Version != Version() || manifest.Protocol != 1 || manifest.Entrypoint != "cloud-path-app-environment-guard" || manifest.Compatibility.Core != ">=0.2.15 <0.3.0" {
		t.Fatalf("manifest identity drift: %+v", manifest)
	}
	if descriptor.ApplicationID != manifest.ID || descriptor.Version != manifest.Version || descriptor.DeclarativeOnly {
		t.Fatal("descriptor drift")
	}
	if len(manifest.Contributes.Applications) != 1 || manifest.Contributes.Applications[0].ID != "environment-guard" || manifest.Contributes.Applications[0].Title == "" {
		t.Fatal("contribution identity missing")
	}
	var gotUI, wantUI any
	if err := json.Unmarshal(manifest.Contributes.Applications[0].UI, &gotUI); err != nil {
		t.Fatalf("invalid UI contribution: %v", err)
	}
	if err := json.Unmarshal([]byte(environmentUIJSON), &wantUI); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotUI, wantUI) {
		t.Fatalf("UI contribution drift: %#v", gotUI)
	}
	if len(manifest.Permissions) != 4 {
		t.Fatal("all permission families must explicitly be empty")
	}
	for _, family := range []string{"hardware", "network", "filesystem", "secrets"} {
		if entries, ok := manifest.Permissions[family]; !ok || len(entries) != 0 {
			t.Fatalf("unexpected permission: %s %+v", family, entries)
		}
	}
	if !reflect.DeepEqual(manifest.Requirements, mirror.Requirements) || len(manifest.Requirements) != 2 || len(descriptor.Requirements) != 2 {
		t.Fatal("requirements mirror drift")
	}
	for i, role := range roles {
		wanted := manifestRequirement{ID: role, Capability: capability(role), Cardinality: "one"}
		if manifest.Requirements[i] != wanted {
			t.Fatal("manifest capability/cardinality drift")
		}
		r := descriptor.Requirements[i]
		if r.ID != role || r.Capability != wanted.Capability || r.Cardinality != "one" || r.MinItems != 0 {
			t.Fatal("descriptor requirements drift")
		}
	}
	if len(descriptor.Jobs) != 2 {
		t.Fatalf("jobs=%+v", descriptor.Jobs)
	}
	ids := map[string]bool{}
	for _, job := range descriptor.Jobs {
		ids[job.ID] = true
		if job.Title == "" || job.InputSchemaJSON != emptyObjectSchema {
			t.Fatalf("bad job schema: %+v", job)
		}
		fields, err := objectFields([]byte(job.InputSchemaJSON))
		if err != nil || len(fields) != 3 {
			t.Fatal("invalid JSON schema")
		}
		if job.ManualOnly != (job.ID == jobRefresh) {
			t.Fatalf("job manual-only semantics: %+v", job)
		}
		if job.ID == jobRefresh && !strings.Contains(jsonText(job), `"manual_only":true`) {
			t.Fatal("manual-only missing from public SDK wire JSON")
		}
	}
	if !ids[jobBootstrap] || !ids[jobRefresh] || ids[jobFreshness] {
		t.Fatal("durable check-freshness must not be an automatic descriptor job")
	}
}

func TestConfigSchemaAndInstallExampleMatchDefaults(t *testing.T) {
	data, err := os.ReadFile("examples/app-config.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := UnmarshalConfig(data)
	if err != nil || !cfg.equal(DefaultConfig()) {
		t.Fatalf("example = %+v %v", cfg, err)
	}
	var schema map[string]any
	readJSON(t, "config.schema.json", &schema)
	if schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatal("configuration schema is not closed")
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("no config properties")
	}
	var defaults map[string]any
	if err := json.Unmarshal([]byte(jsonText(DefaultConfig())), &defaults); err != nil {
		t.Fatal(err)
	}
	if len(properties) != len(defaults) {
		t.Fatal("config fields diverged from schema")
	}
	for name, value := range defaults {
		property, ok := properties[name].(map[string]any)
		if !ok {
			t.Fatalf("missing schema for %s", name)
		}
		if name == "hysteresis" {
			nested, ok := property["properties"].(map[string]any)
			if !ok || property["additionalProperties"] != false {
				t.Fatal("bad hysteresis schema")
			}
			for sub, want := range value.(map[string]any) {
				if nested[sub].(map[string]any)["default"] != want {
					t.Fatal("hysteresis default drift")
				}
			}
		} else if !reflect.DeepEqual(property["default"], value) {
			t.Fatalf("default mismatch for %s", name)
		}
	}
}

func TestOnlyPublicSDKAndNoApplicationIOImports(t *testing.T) {
	// Runtime imports are checked structurally, not by searching business words
	// in comments. Main's os/signal usage is only the host-controlled RPC launcher.
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == ".local" || entry.Name() == ".worktrees" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			name, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			const core = "github.com/DeliciousBuding/cloud-path/"
			if strings.Contains(name, "/internal/") || strings.HasSuffix(name, "/internal") || (strings.HasPrefix(name, core) && !strings.HasPrefix(name, core+"sdk/go/")) {
				t.Errorf("non-public import in %s: %s", path, name)
			}
			if filepath.Dir(path) == "." && !strings.HasSuffix(path, "_test.go") {
				switch name {
				case "os", "io/fs", "path/filepath", "net", "net/http", "os/exec":
					t.Errorf("application logic must not perform IO: %s imports %s", path, name)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	module, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(module), "replace ") || !strings.Contains(string(module), "require github.com/DeliciousBuding/cloud-path v0.2.15") {
		t.Fatal("public SDK pin/replace policy violated")
	}
}
