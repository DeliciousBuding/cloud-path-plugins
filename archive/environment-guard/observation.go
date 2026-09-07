package environmentguard

import (
	"bytes"
	"encoding/json"
	"strconv"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
	"github.com/DeliciousBuding/cloud-path/sdk/go/model"
)

const (
	TemperatureRequirement = "temperature"
	IlluminanceRequirement = "illuminance"
	TemperatureCapability  = "cloudpath.dev/capability/temperature@1"
	IlluminanceCapability  = "cloudpath.dev/capability/illuminance@1"
	// Core v0.2.15 fans a public model.Observation into CapabilityEvent under
	// this exact event type. This literal is a wire contract, not a new SDK API.
	PropertyObservedEvent = "cloudpath.dev/event/property-observed@1"
)

var roles = [...]string{TemperatureRequirement, IlluminanceRequirement}

func capability(role string) string {
	switch role {
	case TemperatureRequirement:
		return TemperatureCapability
	case IlluminanceRequirement:
		return IlluminanceCapability
	}
	return ""
}

type reading struct {
	value           *float64
	unit            string
	quality         string
	reason          string
	observedAt      time.Time
	receivedAt      time.Time
	arrivalAt       time.Time
	freshnessAt     time.Time
	timestampSource string
	sequence        int64
}

type sensorState struct {
	reading         *reading
	lastSequence    int64
	lastObservedAt  time.Time
	lastReceivedAt  time.Time
	lastEnvelopeSeq uint64
	learnedUnit     string
	hasUnit         bool
	zone            string // last fresh classification; never cleared by bad/stale data
}

// accept checks identity before updating ordering watermarks. Wrong-role/entity/
// capability/property events cannot poison an instance or consume its sequence.
// New invalid data on the correct binding invalidates the current reading rather
// than leaving an earlier comfortable value looking current.
func (ss *sensorState) accept(ev *application.CapabilityEvent, envelopeSeq uint64, role, entity string, now time.Time, cfg Config) bool {
	if ev == nil || ev.EventType != PropertyObservedEvent || ev.RequirementID != role || ev.EntityID != entity || entity == "" {
		return false
	}
	fields, err := objectFields([]byte(ev.PayloadJSON))
	if err != nil {
		return ss.invalid("invalid_payload", envelopeSeq, now)
	}
	var capID, property, payloadEntity string
	if json.Unmarshal(fields["capability"], &capID) != nil || json.Unmarshal(fields["property"], &property) != nil {
		return ss.invalid("invalid_payload", envelopeSeq, now)
	}
	if capID != capability(role) || property != "value" {
		return false
	}
	if raw, ok := fields["entity_id"]; ok {
		if json.Unmarshal(raw, &payloadEntity) != nil {
			return ss.invalid("invalid_payload", envelopeSeq, now)
		}
		if payloadEntity != "" && payloadEntity != entity {
			return false
		}
	}
	if envelopeSeq != 0 && envelopeSeq <= ss.lastEnvelopeSeq {
		return false
	}
	var obs model.Observation
	dec := json.NewDecoder(bytes.NewReader([]byte(ev.PayloadJSON)))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(&obs); err != nil {
		return ss.invalid("invalid_payload", envelopeSeq, now)
	}
	if obs.Sequence == 0 && ss.lastSequence > 0 {
		return ss.invalid("missing_sequence", envelopeSeq, now)
	}
	if obs.Sequence < 0 {
		return ss.invalid("invalid_sequence", envelopeSeq, now)
	}
	if obs.Sequence > 0 && obs.Sequence <= ss.lastSequence {
		return false
	}
	if !obs.ObservedAt.IsZero() && !ss.lastObservedAt.IsZero() && obs.ObservedAt.Before(ss.lastObservedAt) {
		return false
	}
	if !obs.ReceivedAt.IsZero() && !ss.lastReceivedAt.IsZero() && obs.ReceivedAt.Before(ss.lastReceivedAt) {
		return false
	}
	// Without a monotonic sequence, equal explicit timestamps are replays. A
	// timestamp-less event has only its actual arrival to establish freshness.
	if obs.Sequence == 0 && !obs.ObservedAt.IsZero() && obs.ObservedAt.Equal(ss.lastObservedAt) {
		return false
	}
	if obs.Sequence == 0 && obs.ObservedAt.IsZero() && !obs.ReceivedAt.IsZero() && obs.ReceivedAt.Equal(ss.lastReceivedAt) {
		return false
	}
	if obs.ObservedAt.After(now.Add(5*time.Second)) || obs.ReceivedAt.After(now.Add(5*time.Second)) {
		return ss.invalid("future_timestamp", envelopeSeq, now)
	}
	r := &reading{unit: obs.Unit, quality: string(obs.Quality), observedAt: obs.ObservedAt, receivedAt: obs.ReceivedAt, arrivalAt: now, sequence: obs.Sequence}
	if r.quality == "" {
		r.quality = "unspecified"
	}
	r.freshnessAt, r.timestampSource = now, "application_received_at"
	if !obs.ReceivedAt.IsZero() {
		r.freshnessAt, r.timestampSource = obs.ReceivedAt, "received_at"
	}
	// A recent receipt never launders an old measurement into a fresh one.
	if !obs.ObservedAt.IsZero() && (r.timestampSource == "application_received_at" || obs.ObservedAt.Before(r.freshnessAt)) {
		r.freshnessAt, r.timestampSource = obs.ObservedAt, "observed_at"
	}
	if r.freshnessAt.After(now) {
		r.freshnessAt = now
	}
	if err := obs.Validate(); err != nil {
		r.reason = "invalid_observation"
	}
	if err := noNull(fields); err != nil {
		r.reason = "invalid_observation"
	}
	if n, ok := obs.Value.(json.Number); ok {
		v, err := strconv.ParseFloat(string(n), 64)
		if err == nil && finite(v) {
			r.value = &v
		}
	}
	if r.value == nil {
		r.reason = "invalid_value"
	}
	if !validUnit(r.unit) {
		r.unit, r.reason = "", "invalid_unit"
	}
	expected := cfg.TemperatureUnit
	if role == IlluminanceRequirement {
		expected = cfg.LightUnit
	}
	if expected != "" {
		if r.unit != "" && r.unit != expected {
			r.reason = "unit_mismatch"
		}
		if r.unit == "" {
			r.unit = expected
		}
	} else if ss.hasUnit && r.unit != ss.learnedUnit {
		r.reason = "unit_changed"
	}
	switch obs.Quality {
	case model.QualityBad, model.QualityUnavailable, model.QualityUncertain:
		r.reason = string(obs.Quality)
	}
	if r.reason == "" && !ss.hasUnit {
		ss.learnedUnit, ss.hasUnit = r.unit, true
	}
	ss.reading = r
	if obs.Sequence > 0 {
		ss.lastSequence = obs.Sequence
	}
	if !obs.ObservedAt.IsZero() {
		ss.lastObservedAt = obs.ObservedAt
	}
	if !obs.ReceivedAt.IsZero() {
		ss.lastReceivedAt = obs.ReceivedAt
	}
	if envelopeSeq != 0 {
		ss.lastEnvelopeSeq = envelopeSeq
	}
	return true
}

func (ss *sensorState) invalid(reason string, envelopeSeq uint64, now time.Time) bool {
	if envelopeSeq != 0 && envelopeSeq <= ss.lastEnvelopeSeq {
		return false
	}
	ss.reading = &reading{reason: reason, quality: "invalid", arrivalAt: now, freshnessAt: now, timestampSource: "application_received_at"}
	if envelopeSeq != 0 {
		ss.lastEnvelopeSeq = envelopeSeq
	}
	return true
}
