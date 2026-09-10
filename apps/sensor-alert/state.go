// SPDX-License-Identifier: Apache-2.0

package sensoralert

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
)

const (
	alertRecordType = "alert"
	alertRecordID   = "current"

	stateArmed     = "armed"
	stateTriggered = "triggered"
	stateRecovered = "recovered"
	stateDisarmed  = "disarmed"

	actionTone = "tone"
	actionLED  = "led"

	freshnessWindow = 2 * time.Minute
)

// AlertRecord is the durable, device-independent application record. A single
// record represents the current/last alert for one plugin instance.
type AlertRecord struct {
	Sensor      string     `json:"sensor"`
	State       string     `json:"state"`
	Value       *float64   `json:"value"`
	Threshold   *float64   `json:"threshold"`
	TriggeredAt *time.Time `json:"triggered_at"`
	RecoveredAt *time.Time `json:"recovered_at"`
	Severity    string     `json:"severity"`
	Summary     string     `json:"summary"`
}

type activeAlert struct {
	Key       string
	Sensor    string
	Value     float64
	Threshold *float64
	Severity  string
	Summary   string
}

type commandResult struct {
	RequestID string `json:"request_id"`
	EntityID  string `json:"entity_id"`
	Action    string `json:"action"`
	State     string `json:"state"`
	ErrorCode string `json:"error_code,omitempty"`
}

type instanceState struct {
	mu sync.Mutex

	id            string
	configured    bool
	config        Config
	configRev     uint32
	bindings      map[string]string
	bindingsValid bool

	armed bool
	state string
	// armRecordPending records that auto-arm set the armed state while no
	// effect stream was attached yet, so the arm domain record still has to be
	// flushed on the first event.
	armRecordPending bool

	sensors       map[string]*sensorState
	active        map[string]activeAlert
	lastTriggered map[string]time.Time
	alert         AlertRecord

	route     application.ApplicationEffectWriter
	effectSeq uint64

	lastEnvelopeSeq uint64
	commandSeq      uint64
	pending         map[string]commandResult
	lastCommand     *commandResult

	jobResults map[string]string
	jobOrder   []string
}

func newInstance(id string) *instanceState {
	return &instanceState{
		id:            id,
		config:        DefaultConfig(),
		bindings:      map[string]string{},
		state:         stateDisarmed,
		sensors:       map[string]*sensorState{},
		active:        map[string]activeAlert{},
		lastTriggered: map[string]time.Time{},
		pending:       map[string]commandResult{},
		jobResults:    map[string]string{},
	}
}

func (st *instanceState) sensor(role string) *sensorState {
	ss := st.sensors[role]
	if ss == nil {
		ss = &sensorState{}
		st.sensors[role] = ss
	}
	return ss
}

func (st *instanceState) binding(role string) string { return st.bindings[role] }

func (st *instanceState) isBound(role string) bool {
	return st.bindingsValid && st.bindings[role] != ""
}

func (st *instanceState) alertRecord() AlertRecord { return st.alert }

func (st *instanceState) setState(state string, now time.Time, active *activeAlert) {
	st.state = state
	switch state {
	case stateTriggered:
		if active == nil {
			return
		}
		value := active.Value
		st.alert = AlertRecord{
			Sensor: active.Sensor, State: stateTriggered, Value: &value, Threshold: active.Threshold,
			TriggeredAt: timePtr(now), Severity: active.Severity, Summary: active.Summary,
		}
	case stateRecovered:
		recoveredAt := now
		st.alert.State = stateRecovered
		st.alert.RecoveredAt = &recoveredAt
		if st.alert.Severity == "" {
			st.alert.Severity = "warning"
		}
		st.alert.Summary = recoverySummary(st.alert.Sensor, st.alert.Value)
	case stateArmed:
		st.alert.State = stateArmed
		st.alert.TriggeredAt = nil
		st.alert.RecoveredAt = nil
		st.alert.Severity = "info"
		st.alert.Summary = "sensor alert armed"
	case stateDisarmed:
		st.alert.State = stateDisarmed
		st.alert.Severity = "info"
		st.alert.Summary = "sensor alert disarmed"
	}
}

func timePtr(value time.Time) *time.Time { return &value }

func recoverySummary(sensor string, value *float64) string {
	if sensor == "" {
		return "sensor alert recovered"
	}
	return fmt.Sprintf("%s recovered (value=%s)", sensor, describeValue(value))
}

func severityFor(sensor string) string {
	switch sensor {
	case ContactRequirement, VibrationRequirement:
		return "critical"
	default:
		return "warning"
	}
}

func triggerSummary(sensor string, value float64, threshold *float64, condition string) string {
	if threshold != nil {
		return fmt.Sprintf("%s alert triggered (value=%g, threshold=%g, condition=%s)", sensor, value, *threshold, condition)
	}
	return fmt.Sprintf("%s alert triggered (value=%g, condition=%s)", sensor, value, condition)
}

func jsonText(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("sensor-alert invariant: non-JSON state: %v", err))
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
