package sensoralert

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultConfigAndSchemaExample(t *testing.T) {
	data := []byte(`{
		"temperature_min":18,
		"temperature_max":28,
		"light_min":null,
		"light_max":null,
		"contact_enabled":false,
		"vibration_enabled":false,
		"cooldown_s":60,
		"silent":false,
		"alert_led_mask":255,
		"alert_tone":{"frequency_hz":1000,"duration_ms":200}
	}`)
	cfg, err := UnmarshalConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, DefaultConfig()) {
		t.Fatalf("example config differs from defaults: %+v", cfg)
	}
	if _, err := UnmarshalConfig([]byte(`{}`)); err != nil {
		t.Fatalf("empty config must use defaults: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name string
		json string
		want string
	}{
		{"temperature range", `{"temperature_min":30,"temperature_max":20}`, "temperature_min"},
		{"light range", `{"light_min":20,"light_max":10}`, "light_min"},
		{"cooldown", `{"cooldown_s":-1}`, "cooldown_s"},
		{"mask", `{"alert_led_mask":256}`, "alert_led_mask"},
		{"tone frequency", `{"alert_tone":{"frequency_hz":0,"duration_ms":200}}`, "frequency_hz"},
		{"tone duration", `{"alert_tone":{"frequency_hz":1000,"duration_ms":205}}`, "duration_ms"},
		{"unknown", `{"unknown":1}`, "unknown field"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := UnmarshalConfig([]byte(tc.json))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
	if cfg, err := UnmarshalConfig([]byte(`{"alert_tone":null}`)); err != nil || cfg.AlertTone != nil {
		t.Fatalf("explicit null tone: cfg=%+v err=%v", cfg, err)
	}
	if cfg, err := UnmarshalConfig([]byte(`{"light_min":1.5,"light_max":null}`)); err != nil || cfg.LightMin == nil || *cfg.LightMin != 1.5 || cfg.LightMax != nil {
		t.Fatalf("nullable light bounds: cfg=%+v err=%v", cfg, err)
	}
}

func TestConfigJSONRoundTrip(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ContactEnabled = true
	cfg.LightMin = floatPtr(10)
	cfg.LightMax = floatPtr(900)
	cfg.AlertTone = nil
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.sameAs(got) {
		t.Fatalf("round trip mismatch: %+v != %+v", cfg, got)
	}
}

func floatPtr(v float64) *float64 { return &v }
