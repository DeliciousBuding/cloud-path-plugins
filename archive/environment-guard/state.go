package environmentguard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
)

const snapshotInterval = 30 * time.Second

// SensorReading keeps measurements distinct from evaluation/arrival timestamps.
// A retained stale numeric value is never marked usable.
type SensorReading struct {
	EntityID              string     `json:"entity_id"`
	Capability            string     `json:"capability"`
	Value                 *float64   `json:"value"`
	Unit                  string     `json:"unit"`
	UnitLabel             string     `json:"unit_label"`
	Status                string     `json:"status"`
	Usable                bool       `json:"usable"`
	ThresholdEnabled      bool       `json:"threshold_enabled"`
	Quality               string     `json:"quality"`
	SourceQuality         string     `json:"source_quality,omitempty"`
	Reason                string     `json:"reason,omitempty"`
	ObservedAt            *time.Time `json:"observed_at"`
	ReceivedAt            *time.Time `json:"received_at"`
	ApplicationReceivedAt *time.Time `json:"application_received_at"`
	FreshnessBasis        string     `json:"freshness_basis,omitempty"`
	Sequence              int64      `json:"sequence,omitempty"`
}

type Thresholds struct {
	TemperatureMin  float64    `json:"temperature_min"`
	TemperatureMax  float64    `json:"temperature_max"`
	LightThreshold  *float64   `json:"light_threshold"`
	LightAlertWhen  string     `json:"light_alert_when"`
	Hysteresis      Hysteresis `json:"hysteresis"`
	TemperatureUnit string     `json:"temperature_unit"`
	LightUnit       string     `json:"light_unit"`
}

type EnvironmentRecord struct {
	Title          string        `json:"title"`
	Summary        string        `json:"summary"`
	Status         string        `json:"status"`
	Quality        string        `json:"quality"`
	Configured     bool          `json:"configured"`
	BindingsValid  bool          `json:"bindings_valid"`
	Temperature    SensorReading `json:"temperature"`
	Illuminance    SensorReading `json:"illuminance"`
	Thresholds     Thresholds    `json:"thresholds"`
	ObservedAt     *time.Time    `json:"observed_at"`
	EvaluatedAt    time.Time     `json:"evaluated_at"`
	Timezone       string        `json:"timezone"`
	StaleAfterS    int           `json:"stale_after_s"`
	ConfigRevision uint32        `json:"config_revision"`
}

type AlertRecord struct {
	Title          string     `json:"title"`
	Summary        string     `json:"summary"`
	Status         string     `json:"status"` // entered/recovered is a transition, not a current alarm
	Condition      string     `json:"condition"`
	RequirementID  string     `json:"requirement_id"`
	EntityID       string     `json:"entity_id"`
	Value          *float64   `json:"value"`
	Unit           string     `json:"unit"`
	Quality        string     `json:"quality"`
	ObservedAt     *time.Time `json:"observed_at"`
	EvaluatedAt    time.Time  `json:"evaluated_at"`
	Thresholds     Thresholds `json:"thresholds"`
	ConfigRevision uint32     `json:"config_revision"`
}

func stamp(at time.Time, location *time.Location) *time.Time {
	if at.IsZero() {
		return nil
	}
	t := at.In(location)
	return &t
}

func (st *instanceState) sensorView(role string, now time.Time, loc *time.Location) SensorReading {
	ss := st.sensors[role]
	view := SensorReading{EntityID: st.bindings[role], Capability: capability(role), Status: "unknown", Quality: "unavailable", Reason: "no_observation"}
	view.ThresholdEnabled = true
	view.Unit = st.config.TemperatureUnit
	if role == IlluminanceRequirement {
		view.Unit = st.config.LightUnit
		view.ThresholdEnabled = st.config.LightThreshold != nil
	}
	if ss.reading != nil {
		r := ss.reading
		view.Value, view.Unit, view.SourceQuality = r.value, r.unit, r.quality
		view.ObservedAt, view.ReceivedAt = stamp(r.observedAt, loc), stamp(r.receivedAt, loc)
		view.ApplicationReceivedAt, view.Sequence = stamp(r.arrivalAt, loc), r.sequence
		view.FreshnessBasis, view.Quality, view.Reason = r.timestampSource, r.quality, r.reason
		switch {
		case r.reason != "":
			if r.reason != "bad" && r.reason != "unavailable" && r.reason != "uncertain" {
				view.Quality = "invalid"
			}
		case now.Sub(r.freshnessAt) >= time.Duration(st.config.StaleAfterS)*time.Second:
			view.Status, view.Quality, view.Reason = "stale", "stale", "observation_expired"
		default:
			view.Status, view.Usable = classify(role, *r.value, ss.zone, st.config), true
		}
	}
	if !st.configured {
		view.Status, view.Usable, view.Quality, view.Reason = "unknown", false, "unavailable", "not_configured"
	}
	if !st.bindingsValid {
		view.Status, view.Usable, view.Quality, view.Reason = "unknown", false, "unavailable", "not_bound"
	}
	view.UnitLabel = view.Unit
	if view.UnitLabel == "" {
		view.UnitLabel = "未声明单位"
		if role == IlluminanceRequirement {
			view.UnitLabel = "原始/相对读数"
		}
	}
	return view
}

func classify(role string, value float64, previous string, cfg Config) string {
	if role == TemperatureRequirement {
		if previous == "warm" && value > cfg.TemperatureMax-cfg.Hysteresis.Temperature {
			return "warm"
		}
		if previous == "cold" && value < cfg.TemperatureMin+cfg.Hysteresis.Temperature {
			return "cold"
		}
		if value > cfg.TemperatureMax {
			return "warm"
		}
		if value < cfg.TemperatureMin {
			return "cold"
		}
	} else {
		if cfg.LightThreshold == nil {
			return "unassessed"
		}
		threshold := *cfg.LightThreshold
		if cfg.LightAlertWhen == "below" {
			if previous == "below_threshold" && value < threshold+cfg.Hysteresis.Light {
				return "below_threshold"
			}
			if value < threshold {
				return "below_threshold"
			}
		} else {
			if previous == "above_threshold" && value > threshold-cfg.Hysteresis.Light {
				return "above_threshold"
			}
			if value > threshold {
				return "above_threshold"
			}
		}
	}
	return "normal"
}

func stateTitle(state string) string {
	switch state {
	case "normal":
		return "设定范围内"
	case "warm":
		return "偏暖"
	case "cold":
		return "偏冷"
	case "below_threshold":
		return "低于设定阈值"
	case "above_threshold":
		return "高于设定阈值"
	case "unassessed":
		return "未启用光照阈值"
	case "stale":
		return "读数过期"
	}
	return "状态未知"
}

func sensorSummary(role string, v SensorReading) string {
	label := "温度"
	if role == IlluminanceRequirement {
		label = "光照"
	}
	value := "尚无有效数字"
	if v.Value != nil {
		value = fmt.Sprintf("%g %s", *v.Value, v.UnitLabel)
	}
	return fmt.Sprintf("%s %s（%s）", label, value, stateTitle(v.Status))
}

// snapshot is pure: reads (including HTTP and Health) never emit records, reset
// timestamps, acquire a measurement, or advance threshold latches.
func (st *instanceState) snapshot(now time.Time) EnvironmentRecord {
	loc, _ := time.LoadLocation(st.config.Timezone) // config validated; initial state has defaults
	if loc == nil {
		loc = time.UTC
	}
	record := EnvironmentRecord{
		Title: "工位环境", Status: "within_thresholds", Quality: "good",
		Configured: st.configured, BindingsValid: st.bindingsValid,
		EvaluatedAt: now.In(loc), Timezone: st.config.Timezone,
		StaleAfterS: st.config.StaleAfterS, ConfigRevision: st.configRev,
	}
	record.Temperature = st.sensorView(TemperatureRequirement, now, loc)
	record.Illuminance = st.sensorView(IlluminanceRequirement, now, loc)
	record.Thresholds = Thresholds{
		TemperatureMin: st.config.TemperatureMin, TemperatureMax: st.config.TemperatureMax,
		LightThreshold: st.config.LightThreshold, LightAlertWhen: st.config.LightAlertWhen, Hysteresis: st.config.Hysteresis,
		TemperatureUnit: record.Temperature.UnitLabel, LightUnit: record.Illuminance.UnitLabel,
	}
	hasUnknown, hasStale, hasAlert := false, false, false
	for _, view := range []SensorReading{record.Temperature, record.Illuminance} {
		hasUnknown = hasUnknown || view.Status == "unknown"
		hasStale = hasStale || view.Status == "stale"
		hasAlert = hasAlert || (view.Usable && condition("", view.Status) != "")
		if view.Quality != "good" {
			record.Quality = view.Quality
		}
		if view.ObservedAt != nil && (record.ObservedAt == nil || view.ObservedAt.After(*record.ObservedAt)) {
			record.ObservedAt = view.ObservedAt
		}
	}
	switch {
	case hasUnknown:
		record.Status = "unknown"
	case hasStale:
		record.Status, record.Quality = "stale", "stale"
	case hasAlert:
		record.Status = "attention"
	}
	if record.Temperature.Quality != record.Illuminance.Quality {
		record.Quality = "mixed"
	}
	record.Summary = strings.Join([]string{sensorSummary(TemperatureRequirement, record.Temperature), sensorSummary(IlluminanceRequirement, record.Illuminance)}, "；")
	return record
}

func condition(role, zone string) string {
	switch zone {
	case "warm":
		return "temperature-high"
	case "cold":
		return "temperature-low"
	case "below_threshold":
		return "light-below"
	case "above_threshold":
		return "light-above"
	}
	return ""
}

// Each condition has two fixed latest-transition slots. This bounds durable
// alerts and pending retries to eight (temperature high/low; light below/above).
func (st *instanceState) collectTransitions(record EnvironmentRecord) {
	for _, role := range roles {
		view := record.Temperature
		if role == IlluminanceRequirement {
			view = record.Illuminance
		}
		if !view.Usable {
			continue
		} // unavailable/stale is NOT a recovery
		ss := st.sensors[role]
		oldCondition, nextCondition := condition(role, ss.zone), condition(role, view.Status)
		if oldCondition != nextCondition {
			if oldCondition != "" {
				st.queueAlert(oldCondition, "recovered", role, view, record)
			}
			if nextCondition != "" {
				st.queueAlert(nextCondition, "entered", role, view, record)
			}
		}
		ss.zone = view.Status
	}
}

func (st *instanceState) queueAlert(conditionID, transition, role string, view SensorReading, record EnvironmentRecord) {
	label := map[string]string{"temperature-high": "温度偏暖", "temperature-low": "温度偏冷", "light-below": "光照读数低于设定阈值", "light-above": "光照读数高于设定阈值"}[conditionID]
	verb := "进入阈值"
	if transition == "recovered" {
		verb = "恢复至阈值内"
	}
	data := AlertRecord{
		Title: label + "：" + verb, Summary: sensorSummary(role, view) + "；这是最近一次阈值变化记录，当前状态请看工位环境。",
		Status: transition, Condition: conditionID, RequirementID: role, EntityID: view.EntityID,
		Value: view.Value, Unit: view.UnitLabel, Quality: view.Quality, ObservedAt: view.ObservedAt,
		EvaluatedAt: record.EvaluatedAt, Thresholds: record.Thresholds, ConfigRevision: st.configRev,
	}
	st.pendingAlerts[conditionID+"-"+transition] = domainRecord("alert", conditionID+"-"+transition, data)
}

func jsonText(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("environment-guard invariant: non-JSON state: %v", err))
	}
	return string(data)
}

func digest(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

func domainRecord(kind, id string, value any) *application.UpsertDomainRecord {
	data := jsonText(value)
	return &application.UpsertDomainRecord{RecordType: kind, RecordID: id, DataJSON: data, Version: digest(data)}
}

func publicationKeys(record EnvironmentRecord) (state, content string) {
	// Numeric/time changes are throttled; per-sensor status, binding, units,
	// quality, thresholds and config changes must be published immediately.
	key := record
	key.EvaluatedAt, key.ObservedAt = time.Time{}, nil
	content = digest(jsonText(key))
	key.Summary = ""
	for _, view := range []*SensorReading{&key.Temperature, &key.Illuminance} {
		view.Value, view.ObservedAt, view.ReceivedAt, view.ApplicationReceivedAt = nil, nil, nil, nil
		view.Sequence = 0
	}
	state = digest(jsonText(key))
	return state, content
}
