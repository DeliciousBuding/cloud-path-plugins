// SPDX-License-Identifier: Apache-2.0

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

func stripUII18n(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			if key == "i18n" || key == "valuesI18n" {
				continue
			}
			out[key] = stripUII18n(child)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, child := range typed {
			out[i] = stripUII18n(child)
		}
		return out
	default:
		return value
	}
}

type manifestRequirement struct {
	ID          string `json:"id"`
	Capability  string `json:"capability"`
	Cardinality string `json:"cardinality"`
}

const environmentUIJSON = `{"apiVersion":1,"navigation":{"title":"环境监测","icon":"leaf","order":30,"route":"environment","visibility":"instance-enabled"},"pages":[{"id":"home","title":"环境监测","sections":[{"type":"status","source":"instance","title":"应用状态","description":"查看应用是否正常运行，以及最近一次状态同步。","emptyText":"暂时没有应用状态信息。"},{"type":"metrics","source":"records","recordType":"environment","title":"当前环境","description":"显示最近一次收到的温度、光照和环境状态。","emptyText":"还没有收到环境数据，请确认设备已连接并开始上报。","fields":[{"key":"temperature.value","label":"温度","unit":"temperature.unit","precision":1,"hideWhenEmpty":true},{"key":"illuminance.value","label":"光照","unit":"illuminance.unit","precision":0,"hideWhenEmpty":true},{"key":"status","label":"环境状态","values":{"within_thresholds":"正常","attention":"需要关注","stale":"数据已过期","unknown":"状态未知"}},{"key":"observed_at","label":"最近观测","format":"time","hideWhenEmpty":true}]},{"type":"actions","source":"manual-jobs","title":"手动操作","description":"需要时重新计算当前状态，不会重新读取传感器。","emptyText":"暂无可执行的操作。"},{"type":"records","source":"records","recordType":"alert","presentation":"timeline","title":"变化记录","description":"查看温度和光照进入、离开设定范围的最近记录。","emptyText":"还没有温度或光照变化记录。","fields":[{"key":"title","label":"变化"},{"key":"summary","label":"说明"},{"key":"status","label":"状态","values":{"entered":"进入设定范围","recovered":"已恢复正常"}},{"key":"condition","label":"类型","values":{"temperature-high":"温度偏高","temperature-low":"温度偏低","light-below":"光照低于设定值","light-above":"光照高于设定值"}},{"key":"value","label":"读数","unit":"unit","precision":1,"hideWhenEmpty":true}]},{"type":"form","source":"config","fields":[{"key":"app_config.timezone","label":"时区","type":"string","description":"用于显示观测时间。填写 UTC 或 Asia/Shanghai 这样的时区名称。","placeholder":"Asia/Shanghai","default":"UTC"},{"key":"app_config.temperature_min","label":"温度下限","type":"number","description":"低于这个温度时，环境状态会提示需要关注。数值使用传感器上报的单位。","default":18},{"key":"app_config.temperature_max","label":"温度上限","type":"number","description":"高于这个温度时，环境状态会提示需要关注。数值使用传感器上报的单位。","default":28},{"key":"app_config.light_threshold","label":"光照提醒值","type":"number","description":"需要光照提醒时填写。留空表示只显示读数，不判断是否超出范围。","placeholder":"例如 300"},{"key":"app_config.light_alert_when","label":"光照提醒方向","type":"select","description":"选择低于还是高于设定值时提醒。这里只比较读数，不代表环境一定变暗或变亮。","enum":["below","above"],"default":"below","values":{"below":"低于设定值时提醒","above":"高于设定值时提醒"}},{"key":"app_config.hysteresis.temperature","label":"温度恢复缓冲","type":"number","description":"温度回到正常范围前需要越过的缓冲值，避免边界附近反复提醒。","minimum":0,"default":1},{"key":"app_config.hysteresis.light","label":"光照恢复缓冲","type":"number","description":"光照回到正常范围前需要越过的缓冲值，使用与光照读数相同的单位。","minimum":0,"default":5},{"key":"app_config.stale_after_s","label":"多久没有新数据就标记为过期（秒）","type":"integer","description":"超过这段时间没有收到新读数，页面会显示数据已过期。","minimum":60,"maximum":86400,"default":120},{"key":"app_config.temperature_unit","label":"温度单位","type":"string","description":"传感器没有提供单位时使用，例如 C 或 °C。应用不会换算温度。","placeholder":"例如 C"},{"key":"app_config.light_unit","label":"光照单位","type":"string","description":"传感器没有提供单位时使用，例如 lux。应用不会换算光照。","placeholder":"例如 lux"}],"title":"提醒设置","description":"设置温度范围、光照提醒和读数过期时间。","emptyText":"暂无设置项。"}],"description":"查看温度和光照是否在设定范围内，并了解最近一次变化。"}]}`

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
	if manifest.API != "plugins.cloudpath.dev/v1alpha1" || manifest.Kind != "Application" || manifest.ID != ApplicationID() || manifest.Version != Version() || manifest.Protocol != 1 || manifest.Entrypoint != "cloud-path-app-environment-guard" || manifest.Compatibility.Core != ">=0.2.29 <0.3.0" {
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
	if !reflect.DeepEqual(stripUII18n(gotUI), stripUII18n(wantUI)) {
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
