package environmentguard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	_ "time/tzdata" // IANA zones are available without a host zoneinfo installation.
	"unicode"
	"unicode/utf8"
)

// Config thresholds are in the incoming reading's units. Units are not converted.
// An explicit unit is both the fallback for missing metadata and an expected unit.
// With no explicit unit, a change in observed units requires reconfiguration.
type Config struct {
	Timezone        string     `json:"timezone"`
	TemperatureMin  float64    `json:"temperature_min"`
	TemperatureMax  float64    `json:"temperature_max"`
	LightThreshold  *float64   `json:"light_threshold"`
	LightAlertWhen  string     `json:"light_alert_when"`
	Hysteresis      Hysteresis `json:"hysteresis"`
	StaleAfterS     int        `json:"stale_after_s"`
	TemperatureUnit string     `json:"temperature_unit"`
	LightUnit       string     `json:"light_unit"`
}

type Hysteresis struct {
	Temperature float64 `json:"temperature"`
	Light       float64 `json:"light"`
}

func DefaultConfig() Config {
	return Config{
		Timezone: "UTC", TemperatureMin: 18, TemperatureMax: 28,
		LightAlertWhen: "below", Hysteresis: Hysteresis{Temperature: 1, Light: 5},
		StaleAfterS: 120,
	}
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func validUnit(unit string) bool {
	if !utf8.ValidString(unit) || utf8.RuneCountInString(unit) > 32 || strings.TrimSpace(unit) != unit {
		return false
	}
	for _, ch := range unit {
		if unicode.IsControl(ch) {
			return false
		}
	}
	return true
}

func (c Config) Validate() error {
	if c.Timezone == "" || c.Timezone == "Local" || len(c.Timezone) > 64 || strings.TrimSpace(c.Timezone) != c.Timezone {
		return fmt.Errorf("timezone must be UTC or an explicit IANA zone (not Local)")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("timezone: %w", err)
	}
	if !finite(c.TemperatureMin) || !finite(c.TemperatureMax) || c.TemperatureMin >= c.TemperatureMax || !finite(c.TemperatureMax-c.TemperatureMin) {
		return fmt.Errorf("temperature_min and temperature_max must be finite, with min < max")
	}
	if c.LightThreshold != nil && !finite(*c.LightThreshold) {
		return fmt.Errorf("light_threshold must be a finite number or null")
	}
	if c.LightAlertWhen != "below" && c.LightAlertWhen != "above" {
		return fmt.Errorf("light_alert_when must be below or above")
	}
	if !finite(c.Hysteresis.Temperature) || c.Hysteresis.Temperature < 0 || c.Hysteresis.Temperature >= (c.TemperatureMax-c.TemperatureMin)/2 {
		return fmt.Errorf("hysteresis.temperature must be >= 0 and less than half the temperature range")
	}
	if !finite(c.Hysteresis.Light) || c.Hysteresis.Light < 0 {
		return fmt.Errorf("hysteresis.light must be finite and >= 0")
	}
	if c.LightThreshold != nil && (!finite(*c.LightThreshold+c.Hysteresis.Light) || !finite(*c.LightThreshold-c.Hysteresis.Light)) {
		return fmt.Errorf("light recovery thresholds must be finite")
	}
	if c.StaleAfterS < 60 || c.StaleAfterS > 86400 {
		return fmt.Errorf("stale_after_s must be an integer in [60, 86400]")
	}
	if !validUnit(c.TemperatureUnit) || !validUnit(c.LightUnit) {
		return fmt.Errorf("units must be trimmed strings of at most 32 characters without control characters")
	}
	return nil
}

// objectFields rejects duplicate keys, null roots and trailing documents. Keeping
// one strict object decoder avoids silently defaulting a mistyped configuration.
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

func noNull(fields map[string]json.RawMessage) error {
	for key, raw := range fields {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("%s must not be null", key)
		}
	}
	return nil
}

// UnmarshalConfig applies defaults only to absent fields, never to null/invalid
// values. Empty input represents the host's omitted app_config.
func UnmarshalConfig(data []byte) (Config, error) {
	cfg := DefaultConfig()
	if len(bytes.TrimSpace(data)) == 0 {
		return cfg, nil
	}
	fields, err := objectFields(data)
	if err != nil {
		return cfg, err
	}
	nonNull := make(map[string]json.RawMessage, len(fields))
	for key, value := range fields {
		if key != "light_threshold" {
			nonNull[key] = value
		}
	}
	if err := noNull(nonNull); err != nil {
		return cfg, err
	}
	if raw, ok := fields["hysteresis"]; ok {
		nested, err := objectFields(raw)
		if err != nil {
			return cfg, fmt.Errorf("hysteresis: %w", err)
		}
		if err := noNull(nested); err != nil {
			return cfg, fmt.Errorf("hysteresis: %w", err)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, err
	}
	return cfg, cfg.Validate()
}

func sameThreshold(a, b *float64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func (c Config) equal(other Config) bool {
	a, b := c, other
	a.LightThreshold, b.LightThreshold = nil, nil
	return a == b && sameThreshold(c.LightThreshold, other.LightThreshold)
}
