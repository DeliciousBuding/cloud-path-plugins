package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestEncodeV1Tone(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"normal", `{"frequency_hz":440,"duration_ms":250}`, "CMD:tone-1:beep:freq=440,dur=25\r\n"},
		{"minimum", `{"frequency_hz":1,"duration_ms":10}`, "CMD:tone-1:beep:freq=1,dur=1\r\n"},
		{"maximum", `{"frequency_hz":4000,"duration_ms":1200}`, "CMD:tone-1:beep:freq=4000,dur=120\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := encodeV1Command("tone-1", actionTone, tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("frame = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEncodeV1ToneRejectsInvalidArgs(t *testing.T) {
	cases := []struct {
		name string
		args string
	}{
		{"missing frequency", `{"duration_ms":100}`},
		{"missing duration", `{"frequency_hz":440}`},
		{"null frequency", `{"frequency_hz":null,"duration_ms":100}`},
		{"frequency zero", `{"frequency_hz":0,"duration_ms":100}`},
		{"frequency above max", `{"frequency_hz":4001,"duration_ms":100}`},
		{"frequency not integer", `{"frequency_hz":440.5,"duration_ms":100}`},
		{"frequency string", `{"frequency_hz":"440","duration_ms":100}`},
		{"duration below min", `{"frequency_hz":440,"duration_ms":9}`},
		{"duration above max", `{"frequency_hz":440,"duration_ms":1210}`},
		{"duration not multiple of ten", `{"frequency_hz":440,"duration_ms":125}`},
		{"duration not integer", `{"frequency_hz":440,"duration_ms":100.5}`},
		{"extra field", `{"frequency_hz":440,"duration_ms":100,"freq":4}`},
		{"array root", `[440,100]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame, err := encodeV1Command("tone-1", actionTone, tc.args)
			if err == nil || len(frame) != 0 {
				t.Fatalf("invalid tone produced frame %q err=%v", frame, err)
			}
		})
	}
}

func TestLegacyToneIsExplicitlyRejected(t *testing.T) {
	args := `{"frequency_hz":440,"duration_ms":250}`
	frame, err := encodeCommand(actionTone, args)
	if err == nil || len(frame) != 0 {
		t.Fatalf("legacy tone produced frame %q err=%v", frame, err)
	}
	if !strings.Contains(err.Error(), "Protocol v1") {
		t.Fatalf("legacy tone rejection must identify Protocol v1, got %v", err)
	}

	if _, err := (&device{}).sendCommand(context.Background(), "tone-1", actionTone, args); err == nil {
		t.Fatal("legacy device accepted tone")
	}
}

func TestToneActionIsDeclared(t *testing.T) {
	if !containsAction(supportedActions, actionTone) {
		t.Fatal("supportedActions does not include tone")
	}
	if !slowActions[actionTone] {
		t.Fatal("slowActions does not include tone")
	}

	var found bool
	for _, capability := range capabilityDescriptors() {
		if capability.ID != capBuzzer {
			continue
		}
		for _, action := range capability.Actions {
			if action.Name != actionTone {
				continue
			}
			found = true
			if strings.TrimSpace(action.Title) == "" || strings.TrimSpace(action.Description) == "" {
				t.Fatal("tone descriptor is missing presentation metadata")
			}
			var schema map[string]any
			if err := json.Unmarshal([]byte(action.InputSchemaJSON), &schema); err != nil {
				t.Fatalf("tone input schema is invalid JSON: %v", err)
			}
			if schema["additionalProperties"] != false {
				t.Fatal("tone input schema must reject additional properties")
			}
			properties, ok := schema["properties"].(map[string]any)
			if !ok {
				t.Fatal("tone input schema has no properties")
			}
			frequency, ok := properties["frequency_hz"].(map[string]any)
			if !ok || frequency["minimum"] != float64(1) || frequency["maximum"] != float64(4000) {
				t.Fatalf("frequency_hz schema = %#v", properties["frequency_hz"])
			}
			duration, ok := properties["duration_ms"].(map[string]any)
			if !ok || duration["minimum"] != float64(10) || duration["maximum"] != float64(1200) || duration["multipleOf"] != float64(10) {
				t.Fatalf("duration_ms schema = %#v", properties["duration_ms"])
			}
		}
	}
	if !found {
		t.Fatal("buzzer capability does not declare tone")
	}
}

func containsAction(actions []string, want string) bool {
	for _, action := range actions {
		if action == want {
			return true
		}
	}
	return false
}
