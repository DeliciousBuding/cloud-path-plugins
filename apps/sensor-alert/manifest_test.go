package sensoralert

import (
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
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

const sensorAlertUIJSON = `{"apiVersion":1,"navigation":{"title":"传感器告警","icon":"bell-ring","order":40,"route":"sensor-alert","visibility":"always"},"pages":[{"id":"home","title":"传感器告警","description":"监测温度、光照、接触和振动，达到设定条件时发出声光提醒，并记录每次告警。","sections":[{"type":"status","title":"运行状态","description":"查看告警功能是否已经启用，以及设备和平台是否连接正常。","emptyText":"暂无运行状态信息。","source":"instance"},{"type":"metrics","title":"当前告警","description":"显示最近一次告警的状态、传感器、当前值和触发时间。","emptyText":"还没有告警记录。","source":"records","recordType":"alert","fields":[{"key":"state","label":"告警状态","values":{"armed":"已启用","triggered":"已触发","recovered":"已恢复","disarmed":"已停用"},"hideWhenEmpty":true},{"key":"sensor","label":"传感器","values":{"temperature":"温度","illuminance":"光照","contact":"接触","vibration":"振动"},"hideWhenEmpty":true},{"key":"value","label":"当前值","format":"number","hideWhenEmpty":true},{"key":"triggered_at","label":"触发时间","format":"time","hideWhenEmpty":true}]},{"type":"actions","title":"告警开关","description":"点击“布防告警”启用告警，点击“撤防告警”停用告警。","emptyText":"暂无可执行的告警操作。","source":"manual-jobs"},{"type":"records","title":"告警记录","description":"查看每次告警发生在哪个传感器、当时是什么状态、数值和触发阈值。","emptyText":"还没有告警记录。","source":"records","recordType":"alert","presentation":"timeline","fields":[{"key":"sensor","label":"传感器","values":{"temperature":"温度","illuminance":"光照","contact":"接触","vibration":"振动"},"hideWhenEmpty":true},{"key":"state","label":"告警状态","values":{"armed":"已启用","triggered":"已触发","recovered":"已恢复","disarmed":"已停用"},"hideWhenEmpty":true},{"key":"value","label":"当前值","format":"number","hideWhenEmpty":true},{"key":"threshold","label":"触发阈值","format":"number","hideWhenEmpty":true},{"key":"triggered_at","label":"触发时间","format":"time","hideWhenEmpty":true}]},{"type":"form","title":"告警设置","description":"设置触发告警的条件，以及触发后如何提醒。","emptyText":"暂无可配置项。","source":"config","fields":[{"key":"app_config.temperature_min","label":"最低温度（℃）","type":"number","description":"低于这个温度时触发告警。","default":18},{"key":"app_config.temperature_max","label":"最高温度（℃）","type":"number","description":"高于这个温度时触发告警。","default":28},{"key":"app_config.light_min","label":"最低光照","type":"number","description":"低于这个数值时触发告警；留空表示不检查。"},{"key":"app_config.light_max","label":"最高光照","type":"number","description":"高于这个数值时触发告警；留空表示不检查。"},{"key":"app_config.contact_enabled","label":"接触告警","type":"boolean","description":"打开后，门、盖或窗被打开时触发告警。","default":false},{"key":"app_config.vibration_enabled","label":"振动告警","type":"boolean","description":"打开后，检测到明显振动时触发告警。","default":false},{"key":"app_config.cooldown_s","label":"重复提醒间隔（秒）","type":"integer","description":"同一问题再次触发前至少等待多久；填 0 表示不限制。","minimum":0,"maximum":86400,"default":60},{"key":"app_config.silent","label":"静音提醒","type":"boolean","description":"打开后只亮指示灯，不发出提示音。","default":false}]}]}]}`

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
	if manifest.API != "plugins.cloudpath.dev/v1alpha1" || manifest.Kind != "Application" ||
		manifest.ID != ApplicationID() || manifest.Version != Version() || manifest.Protocol != 1 ||
		manifest.Entrypoint != "cloud-path-app-sensor-alert" || manifest.Compatibility.Core != ">=0.2.15 <0.3.0" {
		t.Fatalf("manifest identity drift: %+v", manifest)
	}
	if descriptor.ApplicationID != manifest.ID || descriptor.Version != manifest.Version || descriptor.DeclarativeOnly {
		t.Fatal("descriptor drift")
	}
	if len(manifest.Contributes.Applications) != 1 || manifest.Contributes.Applications[0].ID != "sensor-alert" || manifest.Contributes.Applications[0].Title == "" {
		t.Fatal("application contribution missing")
	}
	var gotUI, wantUI any
	if err := json.Unmarshal(manifest.Contributes.Applications[0].UI, &gotUI); err != nil {
		t.Fatalf("invalid UI contribution: %v", err)
	}
	if err := json.Unmarshal([]byte(sensorAlertUIJSON), &wantUI); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotUI, wantUI) {
		t.Fatalf("UI contribution drift: %#v", gotUI)
	}
	if len(manifest.Permissions) != 4 {
		t.Fatal("all permission families must be explicit")
	}
	for _, family := range []string{"hardware", "network", "filesystem", "secrets"} {
		if values, ok := manifest.Permissions[family]; !ok || len(values) != 0 {
			t.Fatalf("unexpected permission %s=%v", family, values)
		}
	}
	if !reflect.DeepEqual(manifest.Requirements, mirror.Requirements) || len(manifest.Requirements) != len(roles) || len(descriptor.Requirements) != len(roles) {
		t.Fatal("requirements mirror drift")
	}
	for i, role := range roles {
		want := manifestRequirement{ID: role, Capability: capabilityForRole(role), Cardinality: "zero-or-one"}
		if manifest.Requirements[i] != want {
			t.Fatalf("manifest requirement %d = %+v, want %+v", i, manifest.Requirements[i], want)
		}
		got := descriptor.Requirements[i]
		if got.ID != role || got.Capability != want.Capability || got.Cardinality != "zero-or-one" || got.MinItems != 0 {
			t.Fatalf("descriptor requirement %d = %+v", i, got)
		}
	}
	jobFlags := map[string]bool{jobArm: true, jobDisarm: true, jobStatus: true, jobCheckFreshness: false}
	if len(descriptor.Jobs) != len(jobFlags) {
		t.Fatalf("jobs = %+v", descriptor.Jobs)
	}
	for _, job := range descriptor.Jobs {
		wantManual, ok := jobFlags[job.ID]
		if !ok || job.ManualOnly != wantManual || job.InputSchemaJSON != emptyObjectSchema || strings.TrimSpace(job.Title) == "" {
			t.Fatalf("bad job descriptor: %+v", job)
		}
	}
}

func TestConfigSchemaAndExampleMatchDefaults(t *testing.T) {
	var schema map[string]any
	readJSON(t, "config.schema.json", &schema)
	if schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatal("config schema must be a closed object")
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("config schema has no properties")
	}
	example, err := os.ReadFile("examples/app-config.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := UnmarshalConfig(example)
	if err != nil || !cfg.sameAs(DefaultConfig()) {
		t.Fatalf("example config = %+v err=%v", cfg, err)
	}
	var exampleMap map[string]any
	if err := json.Unmarshal(example, &exampleMap); err != nil {
		t.Fatal(err)
	}
	if len(properties) != len(exampleMap) {
		t.Fatalf("schema fields=%d example fields=%d", len(properties), len(exampleMap))
	}
	for key := range exampleMap {
		if _, ok := properties[key]; !ok {
			t.Fatalf("example field %q missing from schema", key)
		}
	}
}

func TestOnlyPublicCloudPathSDKImports(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".local", ".worktrees", "vendor":
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
		for _, spec := range file.Imports {
			name := strings.Trim(spec.Path.Value, `"`)
			if strings.Contains(name, "/internal/") || strings.HasSuffix(name, "/internal") {
				t.Fatalf("non-public import in %s: %s", path, name)
			}
			if strings.HasPrefix(name, "github.com/DeliciousBuding/cloud-path/") && !strings.HasPrefix(name, "github.com/DeliciousBuding/cloud-path/sdk/go/") {
				t.Fatalf("non-SDK core import in %s: %s", path, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
