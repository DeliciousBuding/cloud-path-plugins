package environmentguard

import (
	"encoding/json"
	"testing"
)

func TestLightThresholdDisabledByDefaultAndNoScaleOrDirectionAssumptions(t *testing.T) {
	h := newHarness(t)
	h.observe(TemperatureRequirement, 24)
	for _, value := range []float64{0, 512, 1023, 4095, -5} {
		h.observe(IlluminanceRequirement, value)
		r := h.snapshot()
		assertStatus(t, r, "within_thresholds")
		assertValue(t, r.Illuminance, value)
		if r.Illuminance.Status != "unassessed" || r.Illuminance.ThresholdEnabled || r.Thresholds.LightThreshold != nil || r.Illuminance.UnitLabel != "原始/相对读数" {
			t.Fatalf("unproven light interpretation: %+v", r)
		}
	}
	if len(h.writer.records("alert")) != 0 {
		t.Fatal("default light readings produced physical-light alerts")
	}
	for _, raw := range []string{`{"light_threshold":null}`, `{"light_threshold":0,"light_alert_when":"below"}`, `{"light_threshold":300,"light_alert_when":"above","hysteresis":{"light":30}}`} {
		if _, err := UnmarshalConfig([]byte(raw)); err != nil {
			t.Fatalf("valid config %s: %v", raw, err)
		}
	}
	for _, raw := range []string{`{"light_alert_when":"dark"}`, `{"light_alert_when":null}`, `{"light_threshold":"300"}`, `{"light_threshold":1e999}`, `{"hysteresis":{"light":null}}`} {
		if _, err := UnmarshalConfig([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid light config %s", raw)
		}
	}
}

func TestLightBelowAndAboveNumericHysteresis(t *testing.T) {
	for _, tc := range []struct {
		direction, state     string
		enter, hold, recover float64
	}{{"below", "below_threshold", 299, 329.99, 330}, {"above", "above_threshold", 301, 270.01, 270}} {
		t.Run(tc.direction, func(t *testing.T) {
			h := newHarness(t)
			config := jsonText(map[string]any{"light_threshold": 300, "light_alert_when": tc.direction, "hysteresis": map[string]any{"light": 30}})
			configure(t, h.svc, h.id, config, 2)
			// Repeating a config with an equal pointer value/revision is valid.
			configure(t, h.svc, h.id, config, 2)
			h.observe(TemperatureRequirement, 24)
			h.observe(IlluminanceRequirement, tc.enter)
			if h.snapshot().Illuminance.Status != tc.state || len(h.writer.records("alert")) != 1 {
				t.Fatal("numeric threshold did not enter")
			}
			for range 20 {
				h.observe(IlluminanceRequirement, tc.hold)
			}
			if h.snapshot().Illuminance.Status != tc.state || len(h.writer.records("alert")) != 1 {
				t.Fatal("hysteresis jitter wrote history")
			}
			h.observe(IlluminanceRequirement, tc.recover)
			assertStatus(t, h.snapshot(), "within_thresholds")
			if len(h.writer.records("alert")) != 2 {
				t.Fatal("numeric recovery missing")
			}
			for _, record := range h.writer.records("alert") {
				var alert AlertRecord
				if err := json.Unmarshal([]byte(record.DataJSON), &alert); err != nil {
					t.Fatal(err)
				}
				if alert.Condition != "light-"+tc.direction || alert.Thresholds.LightAlertWhen != tc.direction {
					t.Fatalf("lost comparison direction: %+v", alert)
				}
			}
		})
	}
}

func TestDisablingLightThresholdIsNotMeasurementRecovery(t *testing.T) {
	h := newHarness(t)
	configure(t, h.svc, h.id, `{"light_threshold":300,"light_alert_when":"below"}`, 2)
	h.observe(TemperatureRequirement, 24)
	h.observe(IlluminanceRequirement, 100)
	if len(h.writer.records("alert")) != 1 {
		t.Fatal("no threshold entry")
	}
	configure(t, h.svc, h.id, `{"light_threshold":null}`, 3)
	assertStatus(t, h.snapshot(), "within_thresholds")
	if len(h.writer.records("alert")) != 1 || h.snapshot().Illuminance.Status != "unassessed" {
		t.Fatal("disabling threshold masqueraded as physical recovery")
	}
	configure(t, h.svc, h.id, `{"light_threshold":50,"light_alert_when":"above"}`, 4)
	if len(h.writer.records("alert")) != 2 || h.snapshot().Illuminance.Status != "above_threshold" {
		t.Fatal("new comparison direction was not evaluated")
	}
}
