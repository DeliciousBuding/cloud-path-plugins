// SPDX-License-Identifier: Apache-2.0

package sensoralert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/DeliciousBuding/cloud-path/sdk/go/cloudpath/v1/application"
	"github.com/DeliciousBuding/cloud-path/sdk/go/model"
)

const (
	TemperatureRequirement = "temperature"
	IlluminanceRequirement = "illuminance"
	ContactRequirement     = "contact"
	VibrationRequirement   = "vibration"
	SoundRequirement       = "alert-sound"
	LightRequirement       = "alert-light"

	TemperatureCapability = "cloudpath.dev/capability/temperature@1"
	IlluminanceCapability = "cloudpath.dev/capability/illuminance@1"
	HallCapability        = "cloudpath.dev/capability/hall@1"
	VibrationCapability   = "cloudpath.dev/capability/vibration@1"
	BuzzerCapability      = "cloudpath.dev/capability/buzzer@1"
	LEDCapability         = "cloudpath.dev/capability/led@1"

	PropertyObservedEvent = "cloudpath.dev/event/property-observed@1"
	HallCloseEvent        = HallCapability + "/close"
	HallAwayEvent         = HallCapability + "/away"
	VibrationQuakeEvent   = VibrationCapability + "/quake"
)

var roles = [...]string{
	TemperatureRequirement,
	IlluminanceRequirement,
	ContactRequirement,
	VibrationRequirement,
	SoundRequirement,
	LightRequirement,
}

func capabilityForRole(role string) string {
	switch role {
	case TemperatureRequirement:
		return TemperatureCapability
	case IlluminanceRequirement:
		return IlluminanceCapability
	case ContactRequirement:
		return HallCapability
	case VibrationRequirement:
		return VibrationCapability
	case SoundRequirement:
		return BuzzerCapability
	case LightRequirement:
		return LEDCapability
	default:
		return ""
	}
}

func propertyForRole(role string) string {
	switch role {
	case TemperatureRequirement, IlluminanceRequirement:
		return "value"
	case ContactRequirement, VibrationRequirement:
		return "state"
	default:
		return ""
	}
}

type parsedObservation struct {
	value      float64
	quality    string
	usable     bool
	observedAt time.Time
	sequence   int64
}

type sensorState struct {
	lastSequence   int64
	lastObservedAt time.Time
	lastSeenAt     time.Time
	lastValue      float64
	hasValue       bool
	lastQuality    string
}

func (ss *sensorState) accept(obs parsedObservation, now time.Time) bool {
	if obs.sequence > 0 && obs.sequence <= ss.lastSequence {
		return false
	}
	if !obs.observedAt.IsZero() && !ss.lastObservedAt.IsZero() && obs.observedAt.Before(ss.lastObservedAt) {
		return false
	}
	if obs.sequence > 0 {
		ss.lastSequence = obs.sequence
	}
	if !obs.observedAt.IsZero() {
		ss.lastObservedAt = obs.observedAt
	}
	ss.lastSeenAt = now
	ss.lastValue = obs.value
	ss.hasValue = true
	ss.lastQuality = obs.quality
	return true
}

func parseObservation(ev *application.CapabilityEvent, role, entity string) (parsedObservation, bool) {
	if ev == nil || ev.EventType != PropertyObservedEvent || ev.RequirementID != role || ev.EntityID != entity || entity == "" {
		return parsedObservation{}, false
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(ev.PayloadJSON)))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	var obs model.Observation
	if err := dec.Decode(&obs); err != nil {
		return parsedObservation{}, false
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return parsedObservation{}, false
	}
	if obs.EntityID != "" && obs.EntityID != entity {
		return parsedObservation{}, false
	}
	if obs.Capability != capabilityForRole(role) || obs.Property != propertyForRole(role) {
		return parsedObservation{}, false
	}
	if err := obs.Validate(); err != nil {
		return parsedObservation{}, false
	}
	value, ok := numberValue(obs.Value)
	if !ok {
		return parsedObservation{}, false
	}
	quality := string(obs.Quality)
	if quality == "" {
		quality = "unspecified"
	}
	usable := obs.Quality == "" || obs.Quality == model.QualityGood
	return parsedObservation{
		value: value, quality: quality, usable: usable,
		observedAt: obs.ObservedAt, sequence: obs.Sequence,
	}, true
}

func numberValue(value any) (float64, bool) {
	switch v := value.(type) {
	case json.Number:
		n, err := strconv.ParseFloat(string(v), 64)
		return n, err == nil && finite(n)
	case float64:
		return v, finite(v)
	case float32:
		n := float64(v)
		return n, finite(n)
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case int32:
		return float64(v), true
	case uint:
		return float64(v), true
	case uint64:
		return float64(v), true
	case uint32:
		return float64(v), true
	default:
		return 0, false
	}
}

func eventTypeForRole(role string) string {
	switch role {
	case ContactRequirement:
		return HallCapability + "/close"
	case VibrationRequirement:
		return VibrationCapability + "/quake"
	default:
		return ""
	}
}

func eventIsTrigger(role, eventType string) bool {
	switch role {
	case ContactRequirement:
		return eventType == HallCloseEvent
	case VibrationRequirement:
		return eventType == VibrationQuakeEvent
	default:
		return false
	}
}

func eventIsRecovery(role, eventType string) bool {
	switch role {
	case ContactRequirement:
		return eventType == HallAwayEvent
	case VibrationRequirement:
		return eventType == VibrationCapability+"/calm"
	default:
		return false
	}
}

func eventValue(trigger bool) float64 {
	if trigger {
		return 1
	}
	return 0
}

func describeValue(value *float64) string {
	if value == nil {
		return "unknown"
	}
	return fmt.Sprintf("%g", *value)
}
