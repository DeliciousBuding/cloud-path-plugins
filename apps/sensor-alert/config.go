package sensoralert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
)

// ToneConfig is the generic tone command payload sent to a buzzer@1 entity.
// The ranges mirror the tone primitive contract: 1..4000 Hz and 10..1200 ms
// in 10 ms steps.
type ToneConfig struct {
	FrequencyHz int `json:"frequency_hz"`
	DurationMs  int `json:"duration_ms"`
}

// Config is the bounded, device-independent configuration for one instance.
// Requirements are optional: a sensor is active only when its binding exists
// and its configured criterion is enabled.
type Config struct {
	TemperatureMin   float64     `json:"temperature_min"`
	TemperatureMax   float64     `json:"temperature_max"`
	LightMin         *float64    `json:"light_min"`
	LightMax         *float64    `json:"light_max"`
	ContactEnabled   bool        `json:"contact_enabled"`
	VibrationEnabled bool        `json:"vibration_enabled"`
	AutoArm          bool        `json:"auto_arm"`
	CooldownS        int         `json:"cooldown_s"`
	Silent           bool        `json:"silent"`
	AlertLEDMask     int         `json:"alert_led_mask"`
	AlertTone        *ToneConfig `json:"alert_tone"`
}

func DefaultConfig() Config {
	return Config{
		TemperatureMin: 18,
		TemperatureMax: 28,
		AutoArm:        true,
		CooldownS:      60,
		AlertLEDMask:   255,
		AlertTone:      &ToneConfig{FrequencyHz: 1000, DurationMs: 200},
	}
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func (c Config) Validate() error {
	if !finite(c.TemperatureMin) || !finite(c.TemperatureMax) || c.TemperatureMin >= c.TemperatureMax {
		return fmt.Errorf("temperature_min and temperature_max must be finite with min < max")
	}
	if c.LightMin != nil && !finite(*c.LightMin) {
		return fmt.Errorf("light_min must be finite or null")
	}
	if c.LightMax != nil && !finite(*c.LightMax) {
		return fmt.Errorf("light_max must be finite or null")
	}
	if c.LightMin != nil && c.LightMax != nil && *c.LightMin >= *c.LightMax {
		return fmt.Errorf("light_min must be less than light_max when both are set")
	}
	if c.CooldownS < 0 || c.CooldownS > 86400 {
		return fmt.Errorf("cooldown_s must be an integer in [0, 86400]")
	}
	if c.AlertLEDMask < 0 || c.AlertLEDMask > 255 {
		return fmt.Errorf("alert_led_mask must be an integer in [0, 255]")
	}
	if c.AlertTone != nil {
		if c.AlertTone.FrequencyHz < 1 || c.AlertTone.FrequencyHz > 4000 {
			return fmt.Errorf("alert_tone.frequency_hz must be an integer in [1, 4000]")
		}
		if c.AlertTone.DurationMs < 10 || c.AlertTone.DurationMs > 1200 || c.AlertTone.DurationMs%10 != 0 {
			return fmt.Errorf("alert_tone.duration_ms must be in [10, 1200] and a multiple of 10")
		}
	}
	return nil
}

func (c Config) equal(other Config) bool {
	return reflect.DeepEqual(c, other)
}

// objectFields rejects duplicate keys, null roots and trailing documents.
func objectFields(data []byte) (map[string]json.RawMessage, error) {
	if len(data) > 8192 {
		return nil, fmt.Errorf("JSON object exceeds 8192 bytes")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("expected a JSON object")
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		token, err = dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("expected an object key")
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate field %q", key)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		fields[key] = raw
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing JSON")
	}
	return fields, nil
}

func noNull(fields map[string]json.RawMessage, allowed ...string) error {
	allowedSet := map[string]bool{}
	for _, key := range allowed {
		allowedSet[key] = true
	}
	for key, raw := range fields {
		if allowedSet[key] {
			continue
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("%s must not be null", key)
		}
	}
	return nil
}

// UnmarshalConfig applies defaults only to absent fields. Empty input is the
// host's omitted app_config. Explicit null is accepted only for nullable
// fields (light_min, light_max and alert_tone).
func UnmarshalConfig(data []byte) (Config, error) {
	cfg := DefaultConfig()
	if len(bytes.TrimSpace(data)) == 0 {
		return cfg, nil
	}
	fields, err := objectFields(data)
	if err != nil {
		return cfg, err
	}
	if err := noNull(fields, "light_min", "light_max", "alert_tone"); err != nil {
		return cfg, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return cfg, fmt.Errorf("unexpected trailing JSON")
	}
	return cfg, cfg.Validate()
}

func sameFloat(a, b *float64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func (c Config) sensorEnabled(role string) bool {
	switch role {
	case TemperatureRequirement:
		return true
	case IlluminanceRequirement:
		return c.LightMin != nil || c.LightMax != nil
	case ContactRequirement:
		return c.ContactEnabled
	case VibrationRequirement:
		return c.VibrationEnabled
	default:
		return false
	}
}

func (c Config) sameAs(other Config) bool {
	return c.TemperatureMin == other.TemperatureMin &&
		c.TemperatureMax == other.TemperatureMax &&
		sameFloat(c.LightMin, other.LightMin) && sameFloat(c.LightMax, other.LightMax) &&
		c.ContactEnabled == other.ContactEnabled &&
		c.VibrationEnabled == other.VibrationEnabled &&
		c.AutoArm == other.AutoArm &&
		c.CooldownS == other.CooldownS && c.Silent == other.Silent &&
		c.AlertLEDMask == other.AlertLEDMask &&
		((c.AlertTone == nil && other.AlertTone == nil) ||
			(c.AlertTone != nil && other.AlertTone != nil && *c.AlertTone == *other.AlertTone))
}

func validateEmptyObject(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	fields, err := objectFields([]byte(raw))
	if err != nil {
		return err
	}
	if len(fields) != 0 {
		return fmt.Errorf("job arguments must be an empty JSON object")
	}
	return nil
}
