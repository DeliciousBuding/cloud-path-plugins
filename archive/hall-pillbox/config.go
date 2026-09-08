package hallpillbox

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	_ "time/tzdata"
)

const (
	maxLocalIDBytes      = 128
	maxDisplayArgsBytes  = 2048
	maxManualMinutes     = 120
	displayReminderState = "reminder"
	displayMissedState   = "missed"
	displayIdleState     = "idle"
)

// Config is the bounded instance configuration for the Hall Pillbox
// application. The application is device-agnostic: it only refers to entity
// ids supplied through capability bindings and never to a Driver id, port or
// vendor-specific field.
type Config struct {
	Timezone    string         `json:"timezone"`
	Compartment string         `json:"compartment"`
	Schedule    []WindowSpec   `json:"schedule"`
	Reminder    *Reminder      `json:"reminder"`
	Display     *DisplayPolicy `json:"display,omitempty"`
}

// Reminder is the buzzer payload used when a window opens.
type Reminder struct {
	Freq     int `json:"freq"`
	Duration int `json:"duration"`
}

// WindowSpec is one daily schedule window in the configured timezone.
// Compartment is optional and exists only to keep the structure extensible;
// for this version it must be empty or equal Config.Compartment.
type WindowSpec struct {
	ID          string `json:"id"`
	Compartment string `json:"compartment,omitempty"`
	Start       string `json:"start"`
	End         string `json:"end"`
}

// DisplayPolicy contains capability-owned display payloads. The application
// validates bounded JSON objects but never generates glyphs or vendor codes.
type DisplayPolicy struct {
	ReminderArgs json.RawMessage `json:"reminder_args"`
	MissedArgs   json.RawMessage `json:"missed_args,omitempty"`
	IdleArgs     json.RawMessage `json:"idle_args"`
}

func (c Config) Validate() error {
	var errs []string

	if strings.TrimSpace(c.Timezone) == "" {
		errs = append(errs, "timezone is required")
	} else if _, err := time.LoadLocation(c.Timezone); err != nil {
		errs = append(errs, fmt.Sprintf("timezone %q is not a valid IANA name", c.Timezone))
	}

	if id := c.Compartment; id == "" || id != strings.TrimSpace(id) || len(id) > maxLocalIDBytes {
		errs = append(errs, "compartment must be 1-128 UTF-8 bytes with no surrounding whitespace")
	}

	if len(c.Schedule) == 0 {
		errs = append(errs, "schedule must contain at least one window")
	}
	seen := map[string]bool{}
	for i, w := range c.Schedule {
		id := strings.TrimSpace(w.ID)
		if id == "" || id != w.ID || len(id) > maxLocalIDBytes {
			errs = append(errs, fmt.Sprintf("schedule[%d].id must be 1-128 UTF-8 bytes with no surrounding whitespace", i))
		} else if seen[id] {
			errs = append(errs, fmt.Sprintf("duplicate schedule id %q", id))
		}
		seen[id] = true
		if w.Compartment != "" && w.Compartment != c.Compartment {
			errs = append(errs, fmt.Sprintf("schedule[%d].compartment must be empty or equal compartment %q", i, c.Compartment))
		}
		start, ok1 := parseClock(w.Start)
		end, ok2 := parseClock(w.End)
		if !ok1 || !ok2 {
			errs = append(errs, fmt.Sprintf("schedule[%d] start/end must be HH:MM", i))
		} else if !end.After(start) {
			errs = append(errs, fmt.Sprintf("schedule[%d] end must be after start (midnight-crossing windows are not supported)", i))
		}
	}

	if c.Reminder == nil {
		errs = append(errs, "reminder is required")
	} else {
		if c.Reminder.Freq < 0 || c.Reminder.Freq > 9 {
			errs = append(errs, "reminder.freq must be an integer from 0 to 9")
		}
		if c.Reminder.Duration < 0 || c.Reminder.Duration > 9 {
			errs = append(errs, "reminder.duration must be an integer from 0 to 9")
		}
	}

	if c.Display != nil {
		if _, err := canonicalDisplayArgs(c.Display.ReminderArgs); err != nil {
			errs = append(errs, fmt.Sprintf("display.reminder_args: %v", err))
		}
		if _, err := canonicalDisplayArgs(c.Display.IdleArgs); err != nil {
			errs = append(errs, fmt.Sprintf("display.idle_args: %v", err))
		}
		if len(c.Display.MissedArgs) != 0 {
			if _, err := canonicalDisplayArgs(c.Display.MissedArgs); err != nil {
				errs = append(errs, fmt.Sprintf("display.missed_args: %v", err))
			}
		}
	}

	if len(errs) != 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func UnmarshalConfig(raw []byte) (Config, error) {
	if len(raw) == 0 {
		return Config{}, fmt.Errorf("configuration is empty")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Config{}, fmt.Errorf("configuration must contain exactly one JSON object")
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) hasSchedule(id string) bool {
	_, ok := c.scheduleSpec(id)
	return ok
}

func (c Config) scheduleSpec(id string) (WindowSpec, bool) {
	for _, w := range c.Schedule {
		if w.ID == id {
			return w, true
		}
	}
	return WindowSpec{}, false
}

func (c Config) resolvedCompartment() string {
	return c.Compartment
}

func parseClock(raw string) (time.Time, bool) {
	if len(raw) != 5 || raw[2] != ':' {
		return time.Time{}, false
	}
	h, err := time.Parse("15:04", raw)
	if err != nil {
		return time.Time{}, false
	}
	return h, true
}

func canonicalDisplayArgs(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || len(raw) > maxDisplayArgsBytes {
		return "", fmt.Errorf("must be a JSON object of at most %d bytes", maxDisplayArgsBytes)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var object map[string]any
	if err := dec.Decode(&object); err != nil {
		return "", fmt.Errorf("must be a JSON object: %w", err)
	}
	if len(object) == 0 {
		return "", fmt.Errorf("must be a non-empty JSON object")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return "", fmt.Errorf("must contain exactly one JSON object")
	}
	encoded, err := json.Marshal(object)
	if err != nil || len(encoded) > maxDisplayArgsBytes {
		return "", fmt.Errorf("encoded JSON object must be at most %d bytes", maxDisplayArgsBytes)
	}
	return string(encoded), nil
}
